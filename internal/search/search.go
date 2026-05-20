/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

// Package search is the deterministic kernel of the autoresearch
// flywheel. Given the history of completed trials (config, eval_loss),
// it produces the next config to try.
//
// "Deterministic" is the operative word: this package's Suggest call
// is guaranteed to return a valid Trial. It can never return empty,
// time out, hit a rate limit, or fail due to a stale subscription
// token — the entire failure surface of "ask an LLM" is gone. That's
// the property that turns the autoresearch loop into an actual
// flywheel instead of "train the same starter config 100 times
// whenever the LLM is sick."
//
// The LLM is still in the picture — see Phase 2 of the refactor for
// the strategist role — but it's now off the critical path. The
// kernel turns even when every Codex API call is failing.
//
// Implementation: backed by github.com/c-bata/goptuna, using the
// Storage interface directly (not Study.Optimize) so we get
// Ask/Tell semantics. Every Suggest call:
//
//   1. Creates a fresh in-memory study with the configured sampler.
//   2. Replays the supplied history into the study via Storage primitives
//      (CreateNewTrial → SetTrialParam → SetTrialValue → SetTrialState).
//   3. Creates one new running trial, calls the appropriate Suggest*
//      method on it for each param in the search space — this is what
//      causes the sampler to fit the history and produce the next point.
//   4. Reads the suggested params back from Storage and returns them.
//
// Stateless across calls. Identical (history, Config, Seed) → identical
// suggestion. Re-fitting the sampler on every Suggest is cheap (TPE is
// O(N²) in trial count, with N=~100 trials per project that's <10ms).
package search

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"

	"github.com/c-bata/goptuna"
	"github.com/c-bata/goptuna/tpe"
)

// Sampler identifies which Bayesian / sampler strategy to use.
type Sampler string

const (
	// SamplerTPE is goptuna's Tree-structured Parzen Estimator —
	// the production default. Good general-purpose sequential
	// Bayesian-style sampler.
	SamplerTPE Sampler = "tpe"

	// SamplerRandom is the warmup default and a useful baseline
	// for ablations. Identical seed → identical sequence.
	SamplerRandom Sampler = "random"
)

// Direction is the optimization direction.
type Direction string

const (
	// DirectionMinimize is the default for loss metrics.
	DirectionMinimize Direction = "minimize"
	// DirectionMaximize is used for accuracy-style metrics.
	DirectionMaximize Direction = "maximize"
)

// IntSpec describes an integer-valued hyperparameter.
//
//   - Step == 0 → uniform integer in [Low, High].
//   - Step > 0  → uniform integer in [Low, High] aligned to Step
//     (e.g. Low=4, High=64, Step=4 → {4, 8, 12, ..., 64}).
type IntSpec struct {
	Low, High int
	Step      int
}

// FloatSpec describes a float-valued hyperparameter.
//
//   - Log == false → uniform in [Low, High].
//   - Log == true  → log-uniform in [Low, High] (Low must be > 0).
type FloatSpec struct {
	Low, High float64
	Log       bool
}

// CategoricalSpec describes a choice-from-list hyperparameter.
type CategoricalSpec struct {
	Choices []string
}

// Space is the full search space — the union of every param's spec.
// Iteration order over the maps is non-deterministic, but Suggest
// sorts param names before materializing them so the goptuna trial
// sees params in a stable order across calls.
type Space struct {
	Ints         map[string]IntSpec
	Floats       map[string]FloatSpec
	Categoricals map[string]CategoricalSpec
}

// Trial is the suggester's output. Params holds:
//   - int values for Space.Ints
//   - float64 values for Space.Floats
//   - string values for Space.Categoricals
//
// The caller is responsible for mapping these into whatever typed
// config shape they need (QLoRAConfig, etc.).
type Trial struct {
	Params map[string]any
}

// Observation is one completed trial in the history. Same shape as
// Trial.Params, plus the observed metric (EvalLoss).
type Observation struct {
	Params   map[string]any
	EvalLoss float64
}

// Config configures a Searcher.
type Config struct {
	// Sampler picks the sampling strategy. Defaults to TPE.
	Sampler Sampler

	// Direction picks min vs max. Defaults to minimize.
	Direction Direction

	// Seed is the RNG seed for reproducible suggestions. 0 means
	// non-deterministic (current time). Production callers
	// typically pin a Seed per project so the search history is
	// replayable.
	Seed int64

	// Space is the search space. At least one param required.
	Space Space

	// Warmup is the number of trials to draw from a Random sampler
	// before switching to the configured Sampler. Lets TPE see
	// some scatter before it starts exploiting. 5 is a sane
	// default for the QLoRA-scale spaces we use.
	Warmup int
}

// Searcher proposes the next configuration to try based on the
// history of completed trials.
type Searcher interface {
	// Suggest returns the next Trial. Stateless: identical inputs
	// produce identical outputs (given a pinned Seed).
	Suggest(history []Observation) (Trial, error)
}

// ErrEmptySpace is returned when Config.Space contains no params.
var ErrEmptySpace = errors.New("search: Space has no parameters")

// NewSearcher constructs a goptuna-backed Searcher.
func NewSearcher(cfg Config) (Searcher, error) {
	if cfg.Sampler == "" {
		cfg.Sampler = SamplerTPE
	}
	if cfg.Direction == "" {
		cfg.Direction = DirectionMinimize
	}
	if cfg.Warmup < 0 {
		cfg.Warmup = 0
	}
	if len(cfg.Space.Ints) == 0 && len(cfg.Space.Floats) == 0 && len(cfg.Space.Categoricals) == 0 {
		return nil, ErrEmptySpace
	}
	// Validate IntSpecs: Low <= High, Step >= 0.
	for name, s := range cfg.Space.Ints {
		if s.Low > s.High {
			return nil, fmt.Errorf("search: IntSpec %q: Low (%d) > High (%d)", name, s.Low, s.High)
		}
		if s.Step < 0 {
			return nil, fmt.Errorf("search: IntSpec %q: Step (%d) must be >= 0", name, s.Step)
		}
	}
	// Validate FloatSpecs: Low <= High, Log → Low > 0.
	for name, s := range cfg.Space.Floats {
		if s.Low > s.High {
			return nil, fmt.Errorf("search: FloatSpec %q: Low (%g) > High (%g)", name, s.Low, s.High)
		}
		if s.Log && s.Low <= 0 {
			return nil, fmt.Errorf("search: FloatSpec %q: Log=true requires Low > 0 (got %g)", name, s.Low)
		}
	}
	for name, s := range cfg.Space.Categoricals {
		if len(s.Choices) == 0 {
			return nil, fmt.Errorf("search: CategoricalSpec %q: Choices is empty", name)
		}
	}
	return &goptunaSearcher{cfg: cfg}, nil
}

type goptunaSearcher struct {
	cfg Config
}

// Suggest is the workhorse. See package doc comment for the full flow.
func (s *goptunaSearcher) Suggest(history []Observation) (Trial, error) {
	// 1. Pick the sampler. During warmup (history below threshold)
	//    we always use Random so TPE has something to fit later;
	//    otherwise the configured sampler applies.
	var sampler goptuna.Sampler
	useRandom := len(history) < s.cfg.Warmup || s.cfg.Sampler == SamplerRandom
	seed := s.cfg.Seed
	if seed == 0 {
		// Without a pinned seed, derive one from the history
		// length + a salt so successive calls vary but each call
		// is still deterministic in the test sense.
		seed = int64(len(history)*9973 + 17)
	}
	rng := rand.New(rand.NewSource(seed))
	if useRandom {
		sampler = goptuna.NewRandomSampler(goptuna.RandomSamplerOptionSeed(rng.Int63()))
	} else {
		sampler = tpe.NewSampler(tpe.SamplerOptionSeed(rng.Int63()))
	}

	// 2. Build a fresh in-memory study + storage.
	direction := goptuna.StudyDirectionMinimize
	if s.cfg.Direction == DirectionMaximize {
		direction = goptuna.StudyDirectionMaximize
	}
	study, err := goptuna.CreateStudy(
		"autoresearch", // study name is irrelevant for in-memory
		goptuna.StudyOptionSampler(sampler),
		goptuna.StudyOptionDirection(direction),
		goptuna.StudyOptionStorage(goptuna.NewInMemoryStorage()),
	)
	if err != nil {
		return Trial{}, fmt.Errorf("search: create study: %w", err)
	}

	// 3. Replay history. Each Observation is injected as a
	//    completed trial. We materialize the distribution from the
	//    Space so the sampler knows what kind of param it's seeing.
	paramNames := sortedParamNames(s.cfg.Space)
	for _, obs := range history {
		if err := s.injectObservation(study, paramNames, obs); err != nil {
			return Trial{}, err
		}
	}

	// 4. Create a new trial and call Suggest* for each param.
	//    The order matters for some samplers (TPE is order-
	//    insensitive but it's still good hygiene to be stable).
	trialID, err := study.Storage.CreateNewTrial(study.ID)
	if err != nil {
		return Trial{}, fmt.Errorf("search: create new trial: %w", err)
	}
	trial := goptuna.Trial{Study: study, ID: trialID}

	// catRng is a separately-seeded RNG for the categorical
	// workaround (see below). Pinning it to our cfg.Seed-derived
	// `seed` makes Suggest fully reproducible.
	catRng := rand.New(rand.NewSource(seed))

	out := make(map[string]any, len(paramNames))
	for _, name := range paramNames {
		if spec, ok := s.cfg.Space.Ints[name]; ok {
			val, err := suggestInt(&trial, name, spec)
			if err != nil {
				return Trial{}, fmt.Errorf("search: suggest int %q: %w", name, err)
			}
			out[name] = val
			continue
		}
		if spec, ok := s.cfg.Space.Floats[name]; ok {
			val, err := suggestFloat(&trial, name, spec)
			if err != nil {
				return Trial{}, fmt.Errorf("search: suggest float %q: %w", name, err)
			}
			out[name] = val
			continue
		}
		if spec, ok := s.cfg.Space.Categoricals[name]; ok {
			// WORKAROUND for goptuna v0.9.0 bug: in
			// sampler.go, the RandomSampler's
			// CategoricalDistribution branch uses
			// `rand.Intn(...)` (global math/rand RNG)
			// instead of its own `s.rng`. The seed plumbing
			// is silently bypassed for categoricals,
			// producing non-deterministic output across
			// runs with the same configured seed.
			//
			// We dodge it by picking the index ourselves
			// with `catRng`, then recording the choice
			// directly via SetTrialParam so the goptuna
			// study still sees the param + distribution
			// (TPE can learn from categorical history on
			// future calls).
			idx := catRng.Intn(len(spec.Choices))
			dist := goptuna.CategoricalDistribution{Choices: spec.Choices}
			if err := study.Storage.SetTrialParam(trialID, name, float64(idx), dist); err != nil {
				return Trial{}, fmt.Errorf("search: set categorical param %q: %w", name, err)
			}
			out[name] = spec.Choices[idx]
			continue
		}
		// Unreachable — paramNames is built from the Space.
		return Trial{}, fmt.Errorf("search: param %q not found in any spec map", name)
	}

	return Trial{Params: out}, nil
}

// injectObservation records a completed historical trial into the
// study via Storage primitives. Each param is converted to its
// internal float64 representation per goptuna's convention
// (ints stored as float64, log-uniform stored in the linear space,
// categoricals stored as the index into Choices as a float64).
func (s *goptunaSearcher) injectObservation(study *goptuna.Study, paramNames []string, obs Observation) error {
	trialID, err := study.Storage.CreateNewTrial(study.ID)
	if err != nil {
		return fmt.Errorf("search: create historical trial: %w", err)
	}
	for _, name := range paramNames {
		val, ok := obs.Params[name]
		if !ok {
			// History entry doesn't have this param — skip it.
			// goptuna tolerates missing params; the sampler
			// just treats this trial as ignorant of that dim.
			continue
		}
		if spec, ok := s.cfg.Space.Ints[name]; ok {
			ival, ok := toInt(val)
			if !ok {
				return fmt.Errorf("search: history param %q is not an int (got %T)", name, val)
			}
			var dist any
			if spec.Step > 0 {
				dist = goptuna.StepIntUniformDistribution{Low: spec.Low, High: spec.High, Step: spec.Step}
			} else {
				dist = goptuna.IntUniformDistribution{Low: spec.Low, High: spec.High}
			}
			if err := study.Storage.SetTrialParam(trialID, name, float64(ival), dist); err != nil {
				return fmt.Errorf("search: set int param %q: %w", name, err)
			}
			continue
		}
		if spec, ok := s.cfg.Space.Floats[name]; ok {
			fval, ok := toFloat(val)
			if !ok {
				return fmt.Errorf("search: history param %q is not a float (got %T)", name, val)
			}
			var dist any
			if spec.Log {
				dist = goptuna.LogUniformDistribution{Low: spec.Low, High: spec.High}
			} else {
				dist = goptuna.UniformDistribution{Low: spec.Low, High: spec.High}
			}
			if err := study.Storage.SetTrialParam(trialID, name, fval, dist); err != nil {
				return fmt.Errorf("search: set float param %q: %w", name, err)
			}
			continue
		}
		if spec, ok := s.cfg.Space.Categoricals[name]; ok {
			sval, ok := val.(string)
			if !ok {
				return fmt.Errorf("search: history param %q is not a string (got %T)", name, val)
			}
			idx := -1
			for i, c := range spec.Choices {
				if c == sval {
					idx = i
					break
				}
			}
			if idx < 0 {
				return fmt.Errorf("search: history param %q value %q not in Choices", name, sval)
			}
			dist := goptuna.CategoricalDistribution{Choices: spec.Choices}
			if err := study.Storage.SetTrialParam(trialID, name, float64(idx), dist); err != nil {
				return fmt.Errorf("search: set categorical param %q: %w", name, err)
			}
			continue
		}
	}
	if err := study.Storage.SetTrialValue(trialID, obs.EvalLoss); err != nil {
		return fmt.Errorf("search: set trial value: %w", err)
	}
	if err := study.Storage.SetTrialState(trialID, goptuna.TrialStateComplete); err != nil {
		return fmt.Errorf("search: set trial state: %w", err)
	}
	return nil
}

func suggestInt(t *goptuna.Trial, name string, spec IntSpec) (int, error) {
	if spec.Step > 0 {
		return t.SuggestStepInt(name, spec.Low, spec.High, spec.Step)
	}
	return t.SuggestInt(name, spec.Low, spec.High)
}

func suggestFloat(t *goptuna.Trial, name string, spec FloatSpec) (float64, error) {
	if spec.Log {
		return t.SuggestLogFloat(name, spec.Low, spec.High)
	}
	return t.SuggestFloat(name, spec.Low, spec.High)
}

// sortedParamNames returns the union of param names from all three
// maps in deterministic sorted order. Used so successive Suggest
// calls produce trials with params in the same order, which keeps
// goptuna's internal storage deterministic for any given (history,
// config) input.
func sortedParamNames(sp Space) []string {
	names := make([]string, 0, len(sp.Ints)+len(sp.Floats)+len(sp.Categoricals))
	for n := range sp.Ints {
		names = append(names, n)
	}
	for n := range sp.Floats {
		names = append(names, n)
	}
	for n := range sp.Categoricals {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// toInt accepts int / int32 / int64 / float64 (in case JSON
// unmarshaled numbers come in as float64) and returns the int value.
func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int32:
		return int(x), true
	case int64:
		return int(x), true
	case float64:
		if x == float64(int(x)) {
			return int(x), true
		}
		return 0, false
	}
	return 0, false
}

// toFloat accepts float64 / float32 / int (caller might have an
// int that fits a float param like learning_rate=0).
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}
