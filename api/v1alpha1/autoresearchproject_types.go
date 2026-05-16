/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AutoResearchExperimenter is the AgentWorkstation that proposes
// the next QLoRA configuration each cycle by reading the wiki's
// experiment log and emitting a YAML proposal. The agent must
// have wiki-* skills bound (so it can read prior runs) and a
// model with strong reasoning over Markdown.
type AutoResearchExperimenter struct {
	// Name of the AgentWorkstation in the same namespace as this
	// AutoResearchProject. Required.
	Workstation string `json:"workstation"`

	// Optional: override the per-cycle prompt. When empty the
	// operator uses its baked-in default prompt that points the
	// agent at the wiki's experiment log + the current best
	// configuration and asks for a next-experiment proposal.
	// +optional
	PromptOverride string `json:"promptOverride,omitempty"`
}

// AutoResearchBaseModel pins the base model the QLoRA adapters
// fine-tune on top of. Hugging Face is the canonical source.
type AutoResearchBaseModel struct {
	// HuggingfaceID (e.g. "ibm-granite/granite-3.1-8b-instruct").
	// Required.
	HuggingfaceID string `json:"huggingfaceId"`

	// Revision (commit / tag / branch) for reproducibility.
	// Defaults to "main" when empty.
	// +optional
	Revision string `json:"revision,omitempty"`
}

// AutoResearchTrainingData configures the dataset agents
// fine-tune on. v0.0.1 supports Hugging Face datasets; later
// versions will add ConfigMap-mounted custom corpora.
type AutoResearchTrainingData struct {
	// Hugging Face dataset id (e.g. "tatsu-lab/alpaca",
	// "openbmb/UltraFeedback").
	HuggingfaceDataset string `json:"huggingfaceDataset"`

	// Optional split (e.g. "train", "train[:2000]"). When empty,
	// the trainer uses "train".
	// +optional
	Split string `json:"split,omitempty"`

	// SampleCount caps the number of training samples used per
	// experiment. Smaller values = faster iteration. Default is
	// 2000 when unset — chosen to keep single-GPU QLoRA
	// experiments under ~5 minutes per cycle.
	// +optional
	SampleCount *int32 `json:"sampleCount,omitempty"`
}

// AutoResearchEval is the keep/revert signal. v0.0.1 uses the
// inline eval Training Hub emits during fine-tuning (eval_loss
// on a held-out slice of the training data). Phase 3 adds an
// LMEvalJob CR for deep benchmark eval of kept variants — see
// .deepBenchmark below.
type AutoResearchEval struct {
	// Metric the keep/revert decision keys on. v0.0.1 supports
	// "eval_loss" (inline, from Training Hub). Future:
	// "mmlu", "arc_easy", etc. (LMEvalJob-backed).
	// +kubebuilder:default=eval_loss
	// +optional
	Metric string `json:"metric,omitempty"`

	// Direction: minimize for losses, maximize for accuracies.
	// +kubebuilder:validation:Enum=minimize;maximize
	// +kubebuilder:default=minimize
	// +optional
	Direction string `json:"direction,omitempty"`

	// MarginPct is how much better than current best a new
	// experiment must score to be "kept" (anti-noise). String-
	// encoded float (e.g. "0.5") because CRDs discourage raw
	// floats. Default "0" = any improvement keeps; "1" = must
	// be 1% better.
	// +optional
	MarginPct string `json:"marginPct,omitempty"`

	// DeepBenchmark, when set, triggers a TrustyAI LMEvalJob CR
	// for each kept variant — runs benchmark tasks (MMLU, ARC,
	// HellaSwag) against the merged-adapter checkpoint. Slower
	// than inline eval, so only runs on variants that already
	// passed the keep/revert gate. Phase 3.
	// +optional
	DeepBenchmark *AutoResearchDeepBenchmark `json:"deepBenchmark,omitempty"`
}

// AutoResearchDeepBenchmark configures the LMEvalJob CR the
// operator submits for kept variants. The LMEvalJob is a
// TrustyAI primitive — it wraps EleutherAI's
// lm-evaluation-harness and runs benchmark tasks in its own pod
// using its own evaluator image (the trainer image isn't
// involved). Phase 3.
type AutoResearchDeepBenchmark struct {
	// Tasks is the lm-evaluation-harness task list to run
	// (e.g. ["mmlu", "arc_easy", "hellaswag"]). Required when
	// DeepBenchmark is set.
	Tasks []string `json:"tasks"`

	// FewShot is the number of in-context examples per task.
	// Default 0 (zero-shot). 5 is common for MMLU.
	// +optional
	FewShot *int32 `json:"fewShot,omitempty"`

	// Limit caps the number of examples per task — useful for
	// fast smoke-test benchmarks. Default 200; full eval (no
	// limit) takes ~30-60 minutes per kept variant on this
	// cluster's GPUs.
	// +optional
	Limit *int32 `json:"limit,omitempty"`
}

// AutoResearchLoopConfig governs cadence + caps on the
// autonomous loop.
type AutoResearchLoopConfig struct {
	// CadenceMinutes between reconcile-triggered cycles. Each
	// cycle asks the experimenter for a proposal + submits a
	// pipeline run if budget allows. Default 30 — enough for
	// most single-GPU QLoRA experiments to complete between
	// cycles without queuing up.
	// +kubebuilder:default=30
	// +optional
	CadenceMinutes int32 `json:"cadenceMinutes,omitempty"`

	// MaxExperimentsPerCycle caps how many experiments the
	// operator will submit per cycle. Default 1 (serial).
	// Raise once you trust the loop + power watchdog.
	// +kubebuilder:default=1
	// +optional
	MaxExperimentsPerCycle int32 `json:"maxExperimentsPerCycle,omitempty"`

	// MaxTotalExperiments stops the loop after this many runs
	// total. Default 100. Safety belt against an agent running
	// forever or burning through budget unobserved.
	// +kubebuilder:default=100
	// +optional
	MaxTotalExperiments int32 `json:"maxTotalExperiments,omitempty"`

	// MaxConcurrentRuns caps how many in-flight DSP runs the
	// operator allows at once (across cycles). Default 1.
	// +kubebuilder:default=1
	// +optional
	MaxConcurrentRuns int32 `json:"maxConcurrentRuns,omitempty"`
}

// AutoResearchTrainer configures the workload that runs inside
// each DSP pipeline step. The image must match the
// AutoResearchProject's reference image (the one Konflux built
// in Phase 0).
type AutoResearchTrainer struct {
	// Image is the trainer container's pinned tag. Defaults to
	// the Konflux-built v0.0.1 image when empty.
	// +optional
	Image string `json:"image,omitempty"`

	// GPUCount is the number of GPUs per experiment.
	// v0.0.1 supports only 1 (single-GPU QLoRA, optionally with
	// CPU/disk offload for larger bases). Phase 5 raises this
	// for multi-GPU FSDP-QLoRA via Kubeflow Trainer.
	// +kubebuilder:default=1
	// +optional
	GPUCount int32 `json:"gpuCount,omitempty"`

	// PowerCapWatts is the per-GPU power cap the init container
	// applies via `nvidia-smi -pl` before training starts.
	// Safety-critical on this cluster's 2x 4090 node — leave
	// generous headroom under your circuit's rated amperage.
	// Default 250W (a 4090 at 250W draws ~56% of TDP for ~80%
	// of performance; safe on any household circuit).
	// +kubebuilder:default=250
	// +optional
	PowerCapWatts int32 `json:"powerCapWatts,omitempty"`

	// OneTrainingPodPerNode, when true, adds a pod
	// anti-affinity that prevents two trainer pods landing on
	// the same node simultaneously. Belt-and-suspenders for
	// the 4090 node: per-GPU cap + node spread. Default true.
	// +kubebuilder:default=true
	// +optional
	OneTrainingPodPerNode *bool `json:"oneTrainingPodPerNode,omitempty"`

	// OffloadStrategy hints DeepSpeed/training-hub about
	// memory offload. v0.0.1 enum:
	//   - "none"   — fits in GPU memory; fastest
	//   - "cpu"    — optimizer states + grads on CPU RAM
	//   - "nvme"   — full ZeRO-Infinity offload to disk; for
	//                models that don't fit in GPU + CPU
	// Agents can override per-experiment by including the key
	// in their proposed config.
	// +kubebuilder:validation:Enum=none;cpu;nvme
	// +kubebuilder:default=cpu
	// +optional
	OffloadStrategy string `json:"offloadStrategy,omitempty"`
}

// AutoResearchPipelineRef points at the kfp pipeline registered
// in DSP. The operator submits Runs against this pipeline each
// cycle, parameterized with the agent's proposed config.
type AutoResearchPipelineRef struct {
	// Name of the kfp pipeline in the DSP namespace.
	Name string `json:"name"`

	// Namespace where the DataSciencePipelinesApplication lives
	// (typically the same as this AutoResearchProject).
	Namespace string `json:"namespace"`
}

// AutoResearchProjectSpec defines the desired state — what to
// research, with whom, against what target, under what limits.
type AutoResearchProjectSpec struct {
	// DisplayName shown in the OpenShift Console plugin and in
	// the wiki's project pages.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Description shown in the same surfaces.
	// +optional
	Description string `json:"description,omitempty"`

	// KnowledgeBase that the experimenter agent reads prior
	// runs from and files new findings into. The KB must be
	// bound to the same gateway as the experimenter
	// AgentWorkstation (so the agent can see the wiki PVC).
	KnowledgeBase string `json:"knowledgeBase"`

	// Experimenter is the agent proposing experiments. v0.0.1
	// supports one; later versions support multiple
	// (council-pattern parallel proposers + a chairman that
	// picks winners across them).
	Experimenter AutoResearchExperimenter `json:"experimenter"`

	// BaseModel the QLoRA adapters fine-tune.
	BaseModel AutoResearchBaseModel `json:"baseModel"`

	// TrainingData is what the agents fine-tune on.
	TrainingData AutoResearchTrainingData `json:"trainingData"`

	// Eval is the keep/revert metric + (optional) deep
	// benchmark config.
	Eval AutoResearchEval `json:"eval"`

	// LoopConfig governs cadence + safety caps.
	// +optional
	LoopConfig AutoResearchLoopConfig `json:"loopConfig,omitempty"`

	// Trainer configures the per-experiment workload (image,
	// GPU count, power cap, offload strategy).
	// +optional
	Trainer AutoResearchTrainer `json:"trainer,omitempty"`

	// PipelineRef points at the DSP-registered kfp pipeline.
	PipelineRef AutoResearchPipelineRef `json:"pipelineRef"`

	// ExperimentName is the OpenShift AI Experiment that
	// pipeline runs roll up under. All runs from this project
	// land in the same Experiment so the Experiments UI shows
	// them grouped + sortable. Defaults to metadata.name.
	// +optional
	ExperimentName string `json:"experimentName,omitempty"`

	// ValidationMode, when true, overrides the proposed QLoRA
	// config with a smoke-test profile (10 training steps,
	// batch=1, rank=4, max_seq_length=256) and clamps the
	// dataset sample count to 200. Use to validate end-to-end
	// plumbing — image pull, GPU scheduling, model load,
	// dataset load, AUTORESEARCH_RESULT= emission, operator
	// status writeback, kfp metrics — in ~3-5 minutes per
	// round before committing to a real 30-90 min run.
	// Eval loss is still reported but is meaningless at this
	// scale; do not feed validation rounds into keep/revert
	// heuristics. Flip to false for production runs.
	// +optional
	ValidationMode bool `json:"validationMode,omitempty"`
}

// AutoResearchExperimentRecord is a single run's summary kept in
// status for the wiki/UI to render. The authoritative record
// lives in the wiki (one Markdown page per round) + in the DSP
// run history; this is the operator's bookkeeping.
type AutoResearchExperimentRecord struct {
	// Round is the cycle number (1, 2, 3, ...).
	Round int32 `json:"round"`

	// RunID is the DSP pipeline run identifier.
	RunID string `json:"runId"`

	// Experimenter is the agent name that proposed this
	// experiment (helpful when council-mode is enabled later).
	// +optional
	Experimenter string `json:"experimenter,omitempty"`

	// ProposalPath is the wiki path where the agent's proposal
	// (config + reasoning) was filed.
	// +optional
	ProposalPath string `json:"proposalPath,omitempty"`

	// EvalLoss from Training Hub's inline eval, string-encoded
	// (CRDs discourage raw floats). Empty when the run is
	// still in flight.
	// +optional
	EvalLoss string `json:"evalLoss,omitempty"`

	// MetricValue is the configured eval metric's value when
	// it isn't eval_loss (e.g. mmlu accuracy). String-encoded.
	// Same in-flight semantics.
	// +optional
	MetricValue string `json:"metricValue,omitempty"`

	// Kept is the keep/revert decision (true = adopted as new
	// baseline; false = reverted).
	// +optional
	Kept *bool `json:"kept,omitempty"`

	// AdapterArtifactURI is the S3/Object-store path to the
	// LoRA adapter checkpoint (only set for kept runs).
	// +optional
	AdapterArtifactURI string `json:"adapterArtifactUri,omitempty"`

	// StartedAt + CompletedAt are operator-observed timestamps.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// AutoResearchProjectCondition is a standard k8s condition.
type AutoResearchProjectCondition struct {
	Type               string      `json:"type"`
	Status             string      `json:"status"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// AutoResearchProjectStatus reflects observed state.
type AutoResearchProjectStatus struct {
	// Phase is one of: Pending (waiting on deps), Running
	// (cycles firing), Paused (user-suspended), Completed
	// (hit MaxTotalExperiments), Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Round is the current cycle number.
	// +optional
	Round int32 `json:"round,omitempty"`

	// ExperimentsRun is the total count of submitted runs
	// (kept + reverted).
	// +optional
	ExperimentsRun int32 `json:"experimentsRun,omitempty"`

	// ExperimentsKept counts the kept (improving) runs.
	// +optional
	ExperimentsKept int32 `json:"experimentsKept,omitempty"`

	// BestEvalLoss / BestMetricValue track the current
	// champion's score, string-encoded (whichever metric the
	// user configured).
	// +optional
	BestEvalLoss string `json:"bestEvalLoss,omitempty"`
	// +optional
	BestMetricValue string `json:"bestMetricValue,omitempty"`

	// BestRunID is the DSP run ID of the current champion.
	// +optional
	BestRunID string `json:"bestRunId,omitempty"`

	// LastCycleTime is when the reconciler last evaluated
	// whether to submit a new experiment. Used to gate
	// CadenceMinutes.
	// +optional
	LastCycleTime *metav1.Time `json:"lastCycleTime,omitempty"`

	// OpenRuns is a map of DSP runs the operator submitted
	// that haven't completed yet. Keyed by run ID. The next
	// reconcile cycle polls each for status + drains them.
	// +optional
	OpenRuns map[string]string `json:"openRuns,omitempty"`

	// RecentRuns is a bounded ring of the last N experiment
	// records (typically 20) for the UI to render.
	// +optional
	RecentRuns []AutoResearchExperimentRecord `json:"recentRuns,omitempty"`

	// Conditions reflect the latest observations. Standard
	// types: Ready, PipelineResolved, KnowledgeBaseReady,
	// ExperimenterReady.
	// +optional
	Conditions []AutoResearchProjectCondition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=arp,categories=agent-office
// +kubebuilder:printcolumn:name="DisplayName",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="BaseModel",type="string",JSONPath=".spec.baseModel.huggingfaceId"
// +kubebuilder:printcolumn:name="Round",type="integer",JSONPath=".status.round"
// +kubebuilder:printcolumn:name="Kept/Total",type="string",JSONPath=".status.experimentsKept",description="Kept experiments out of total run"
// +kubebuilder:printcolumn:name="Best",type="string",JSONPath=".status.bestEvalLoss"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AutoResearchProject is a Karpathy-style autonomous
// ML-experimentation loop, packaged as a declarative resource.
//
// Each AutoResearchProject runs an unbounded sequence of QLoRA
// fine-tuning experiments against a base model. The
// experimenter agent proposes configurations by reading the
// referenced KnowledgeBase's experiment log; the operator
// submits Data Science Pipeline runs; results land back in
// the wiki + the OpenShift AI Experiments UI; kept variants
// optionally trigger TrustyAI LMEvalJob benchmark runs.
//
// Hardware-aware: the trainer pod's init container applies a
// per-GPU `nvidia-smi -pl` cap before any training so power
// stays within circuit budget. Pod-anti-affinity keeps one
// trainer pod per node by default (relevant on the 2x 4090
// node, where two simultaneous full-throttle pods would risk
// tripping a 15A breaker).
type AutoResearchProject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AutoResearchProjectSpec   `json:"spec,omitempty"`
	Status AutoResearchProjectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AutoResearchProjectList contains a list of AutoResearchProject.
type AutoResearchProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AutoResearchProject `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AutoResearchProject{}, &AutoResearchProjectList{})
}
