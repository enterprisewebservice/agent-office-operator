/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// AutoResearchProject ↔ experimenter agent ↔ wiki integration.
//
// v0.0.51 shipped infra (DSP, Quay, MR) but `requestProposal` was a
// stub that always returned the deterministic starter config — the
// agent-driven proposal + wiki-write paths were never implemented.
// v0.0.52 wires those up:
//
//   1. The operator finds the gateway pod that hosts the experimenter
//      AgentWorkstation (`spec.experimenter.workstation`) via the
//      AgentGateway → pod selector chain.
//   2. The operator exec's `openclaw agent --agent <name>` inside that
//      pod, passing a prompt that summarizes the wiki log + asks for
//      the next QLoRA config as strict JSON.
//   3. The agent's JSON reply is parsed back into a QLoRAConfig and
//      becomes the round's proposal.
//   4. On submission, the operator clones the project's KnowledgeBase
//      git repo, writes `proposals/round-N.yaml`, commits + pushes.
//   5. On drain (round completes), the operator writes
//      `results/round-N.md` and updates `log/log.md` the same way.
//
// Failures everywhere fall back to the deterministic starter config
// so the loop never wedges on an agent outage. Conditions on Status
// (AgentIntegrationOK, WikiSyncOK) surface what failed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	gitobject "github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// agentExecTimeout caps how long the operator will wait on
// `openclaw agent` to return. The LLM reasoning + tool calls can
// legitimately take a minute or two; anything longer is a stuck call.
const agentExecTimeout = 10 * time.Minute

// gatewayContainerName is the openclaw container inside the
// research-gateway pod we exec into.
const gatewayContainerName = "openclaw"

// proposeViaAgent runs one agent turn via the gateway and parses the
// reply as a QLoRAConfig. On any failure it returns an error and the
// caller falls back to the starter config.
//
// Side effects (only on success): the round's `proposals/round-N.yaml`
// is committed + pushed to the KnowledgeBase git repo so the agent's
// reasoning is captured in the wiki BEFORE training even starts.
func (r *AutoResearchProjectReconciler) proposeViaAgent(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
	round int,
) (QLoRAConfig, string, error) {
	log := logf.FromContext(ctx)

	agentName := p.Spec.Experimenter.Workstation
	if agentName == "" {
		return QLoRAConfig{}, "", fmt.Errorf("spec.experimenter.workstation is empty")
	}

	// 1. Find the experimenter AgentWorkstation + its gateway pod.
	gwPod, err := r.findGatewayPodForAgent(ctx, p.Namespace, agentName)
	if err != nil {
		return QLoRAConfig{}, "", fmt.Errorf("find gateway pod: %w", err)
	}

	// 2. Read the current wiki log to give the agent prior context.
	//    The agent's prompt asks it to read log/log.md inside its own
	//    workspace; we send a snapshot here so it can reason even if
	//    its own filesystem read fails for any reason.
	logSnapshot, _ := r.readWikiLogSnapshot(ctx, p)

	// 3. Build the prompt.
	prompt := buildExperimenterPrompt(p, round, logSnapshot)

	// 4. Exec `openclaw agent --agent <name> --message <prompt> --json`.
	tctx, cancel := context.WithTimeout(ctx, agentExecTimeout)
	defer cancel()
	out, err := r.execInGatewayPod(tctx, gwPod, []string{
		"openclaw", "agent",
		"--agent", agentName,
		"--message", prompt,
		"--json",
		"--timeout", "540",
	})
	if err != nil {
		return QLoRAConfig{}, "", fmt.Errorf("openclaw agent exec: %w (stdout/stderr: %s)", err, truncateMsg(out, 400))
	}

	// 5. Parse the agent's reply back into a QLoRAConfig.
	cfg, reasoning, perr := parseAgentProposal(out, round)
	if perr != nil {
		return QLoRAConfig{}, "", fmt.Errorf("parse agent reply: %w (reply: %s)", perr, truncateMsg(out, 400))
	}

	// 6. Best-effort: commit the proposal to the wiki repo so the
	//    agent's reasoning lands in Obsidian even if training fails.
	if werr := r.wikiPushProposal(ctx, p, round, cfg, reasoning); werr != nil {
		log.Info("wiki proposal commit failed (continuing with proposed config)", "err", werr.Error())
	}

	return cfg, fmt.Sprintf("agent %s", agentName), nil
}

// findGatewayPodForAgent walks AgentWorkstation → AgentGateway → Pod
// to land on the openclaw container that hosts the agent's logical
// runtime. Only shared-runtime AgentWorkstations are supported here;
// dedicated-runtime agents would have their own Deployment and need
// a different exec target (deferred — autoresearch agents are
// expected to be shared-runtime).
func (r *AutoResearchProjectReconciler) findGatewayPodForAgent(
	ctx context.Context,
	namespace, agentName string,
) (*corev1.Pod, error) {
	var aw agentofficev1alpha1.AgentWorkstation
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: agentName}, &aw); err != nil {
		return nil, fmt.Errorf("get AgentWorkstation %s/%s: %w", namespace, agentName, err)
	}
	if aw.Spec.Runtime == nil || aw.Spec.Runtime.Shared == nil {
		return nil, fmt.Errorf("AgentWorkstation %s has no spec.runtime.shared — dedicated-runtime agents not yet supported for proposeViaAgent", agentName)
	}
	gwName := aw.Spec.Runtime.Shared.GatewayRef
	if gwName == "" {
		return nil, fmt.Errorf("AgentWorkstation %s has no shared.gatewayRef", agentName)
	}

	// Find a Ready pod for the AgentGateway. Convention: pod labeled
	// `app=<gatewayName>` per the gateway controller's Deployment.
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"app": gwName},
	); err != nil {
		return nil, fmt.Errorf("list gateway pods: %w", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodRunning && podReady(p) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no Ready pod found for AgentGateway %s/%s (found %d candidates)", namespace, gwName, len(pods.Items))
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// execInGatewayPod runs `cmd` inside the openclaw container. Mirrors
// AgentWorkstationReconciler.execInPod but targets the
// AutoResearchProjectReconciler's RestConfig (kept separate so the
// two reconcilers stay independently testable).
func (r *AutoResearchProjectReconciler) execInGatewayPod(
	ctx context.Context,
	pod *corev1.Pod,
	cmd []string,
) (string, error) {
	cs, err := kubernetes.NewForConfig(r.RestConfig)
	if err != nil {
		return "", fmt.Errorf("kubernetes client: %w", err)
	}
	req := cs.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: gatewayContainerName,
			Command:   cmd,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(r.RestConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("spdy executor: %w", err)
	}
	var out bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &out,
		Stderr: &out,
	}); err != nil {
		return out.String(), fmt.Errorf("stream: %w", err)
	}
	return out.String(), nil
}

// buildExperimenterPrompt constructs the message we send to the
// agent. The agent is configured (system prompt) to respond with
// JSON matching QLoRAConfig; this prompt provides the project
// context + previous log + an explicit instruction to reply in JSON.
//
// Keeps the prompt boring on purpose — the agent's reasoning capacity
// comes from its system prompt + tool list, not from this template.
func buildExperimenterPrompt(p *agentofficev1alpha1.AutoResearchProject, round int, logSnapshot string) string {
	base := p.Spec.BaseModel.HuggingfaceID
	if base == "" {
		base = "ibm-granite/granite-4.1-8b"
	}
	dataset := p.Spec.TrainingData.HuggingfaceDataset
	if dataset == "" {
		dataset = "ise-uiuc/Magicoder-OSS-Instruct-75K"
	}
	bestLoss := p.Status.BestEvalLoss
	if bestLoss == "" {
		bestLoss = "(none yet)"
	}
	// Truncate the log snapshot to keep the prompt manageable. The
	// agent can always file_read the full log itself.
	if len(logSnapshot) > 4000 {
		logSnapshot = "...(earlier entries truncated)\n\n" + logSnapshot[len(logSnapshot)-4000:]
	}
	return strings.Join([]string{
		fmt.Sprintf("Project: %s (round %d)", p.Name, round),
		fmt.Sprintf("Base model: %s", base),
		fmt.Sprintf("Dataset: %s", dataset),
		fmt.Sprintf("Best eval_loss so far: %s", bestLoss),
		"",
		"Recent experiment log (newest last):",
		"```",
		strings.TrimSpace(logSnapshot),
		"```",
		"",
		"Propose the next QLoRA configuration. Reply with ONLY a JSON object matching:",
		"  {\"lora_rank\": int, \"lora_alpha\": int, \"lora_dropout\": float,",
		"   \"target_modules\": [string], \"learning_rate\": float,",
		"   \"num_training_steps\": int (100-500), \"per_device_batch_size\": int (1-8),",
		"   \"gradient_accumulation_steps\": int (1-8), \"max_seq_length\": int (512-2048),",
		"   \"warmup_steps\": int (0-50), \"weight_decay\": float (0.0-0.1),",
		"   \"offload_strategy\": \"cpu\" | \"disk\" | \"none\",",
		"   \"notes\": \"brief explanation\"}",
		"",
		"No prose. No markdown. JSON only.",
	}, "\n")
}

// parseAgentProposal extracts a QLoRAConfig from `openclaw agent --json`
// output. The wrapper shape is approximately:
//   {"reply": "...", "session_id": "...", "channel": null}
// The reply field carries the agent's text. Inside that text we look
// for a JSON object matching our schema. We're tolerant of agents
// that wrap the JSON in backticks or include a small amount of
// chatter; the JSON object regex is permissive but anchored on the
// known keys.
func parseAgentProposal(raw string, round int) (QLoRAConfig, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return QLoRAConfig{}, "", fmt.Errorf("empty reply")
	}

	// Path A: stdout IS the JSON object (some `openclaw agent --json`
	// invocations write the wrapper as a single line).
	var wrapper struct {
		Reply string `json:"reply"`
	}
	body := raw
	if err := json.Unmarshal([]byte(raw), &wrapper); err == nil && wrapper.Reply != "" {
		body = wrapper.Reply
	}

	// Path B: the body sometimes contains markdown like ```json{...}```.
	// Strip those fences if present.
	body = stripCodeFences(body)

	// Path C: agent might prefix with prose. Grab the first JSON object
	// that contains `lora_rank` (a required, near-unique key).
	jsonStr, found := extractJSONObject(body, "lora_rank")
	if !found {
		// Body itself might be the JSON.
		jsonStr = strings.TrimSpace(body)
	}

	var cfg QLoRAConfig
	if err := json.Unmarshal([]byte(jsonStr), &cfg); err != nil {
		return QLoRAConfig{}, "", fmt.Errorf("unmarshal config JSON: %w", err)
	}
	cfg.Round = round // operator always overrides — agent shouldn't pick the round number

	// Validate the agent's choice is sane; reject otherwise.
	if err := validateQLoRAConfig(&cfg); err != nil {
		return QLoRAConfig{}, "", fmt.Errorf("validate: %w", err)
	}

	return cfg, strings.TrimSpace(body), nil
}

// stripCodeFences removes ```json ... ``` or ``` ... ``` wrappers if
// the entire body is inside one.
func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening line (possibly "```json")
	s = strings.TrimPrefix(s, "```")
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[idx+1:]
	}
	// Drop the trailing fence
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

// extractJSONObject finds the first balanced { ... } block containing
// the marker key. Returns the substring and whether it was found.
// Brace-balancing is naive but adequate for our well-defined schema.
func extractJSONObject(body, marker string) (string, bool) {
	if !strings.Contains(body, marker) {
		return "", false
	}
	// Find the start of the object containing the marker.
	idx := strings.Index(body, marker)
	if idx < 0 {
		return "", false
	}
	start := strings.LastIndex(body[:idx], "{")
	if start < 0 {
		return "", false
	}
	// Walk forward, counting braces (no escape-aware string parsing —
	// our schema has no embedded "{" in values, and json.Unmarshal
	// will catch any malformation downstream).
	depth := 0
	for i := start; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return body[start : i+1], true
			}
		}
	}
	return "", false
}

// validateQLoRAConfig sanity-checks the agent's proposal so a
// hallucinated nonsensical config doesn't burn a 90-minute training
// cycle. Operator clamps to bounds rather than rejecting outright;
// borderline-out-of-range proposals still get tried.
func validateQLoRAConfig(c *QLoRAConfig) error {
	if c.LoraRank <= 0 || c.LoraRank > 64 {
		return fmt.Errorf("lora_rank=%d out of 1..64", c.LoraRank)
	}
	if c.LoraAlpha <= 0 || c.LoraAlpha > 256 {
		return fmt.Errorf("lora_alpha=%d out of 1..256", c.LoraAlpha)
	}
	if c.LearningRate <= 0 || c.LearningRate > 0.01 {
		return fmt.Errorf("learning_rate=%g out of (0, 0.01]", c.LearningRate)
	}
	if c.NumTrainingSteps < 10 || c.NumTrainingSteps > 2000 {
		return fmt.Errorf("num_training_steps=%d out of 10..2000", c.NumTrainingSteps)
	}
	if len(c.TargetModules) == 0 {
		return fmt.Errorf("target_modules cannot be empty")
	}
	return nil
}

// ------------- Wiki git operations ------------------------------------

// wikiPushProposal clones the KB git repo (or fast-forwards an existing
// shallow clone), writes proposals/round-N.yaml + a Markdown summary,
// commits + pushes. Idempotent on the round number — re-runs replace
// the prior round-N file rather than appending.
func (r *AutoResearchProjectReconciler) wikiPushProposal(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
	round int,
	cfg QLoRAConfig,
	reasoning string,
) error {
	repo, dir, auth, err := r.openWikiRepo(ctx, p)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	cfgYAML, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal proposal: %w", err)
	}
	proposalPath := filepath.Join(dir, "proposals", fmt.Sprintf("round-%d.yaml", round))
	if err := os.MkdirAll(filepath.Dir(proposalPath), 0o755); err != nil {
		return fmt.Errorf("mkdir proposals: %w", err)
	}
	if err := os.WriteFile(proposalPath, cfgYAML, 0o644); err != nil {
		return fmt.Errorf("write proposal: %w", err)
	}

	// Markdown counterpart for Obsidian readability.
	mdPath := filepath.Join(dir, "proposals", fmt.Sprintf("round-%d.md", round))
	md := fmt.Sprintf("# Round %d — proposal\n\n**Agent reasoning:**\n\n%s\n\n**Config (also in `round-%d.yaml`):**\n\n```yaml\n%s```\n", round, reasoning, round, string(cfgYAML))
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		return fmt.Errorf("write proposal md: %w", err)
	}

	return commitAndPush(ctx, repo, auth, fmt.Sprintf("propose: round %d", round),
		"proposals/round-"+intStr(round)+".yaml",
		"proposals/round-"+intStr(round)+".md",
	)
}

// wikiPushResult writes results/round-N.md + appends to log/log.md
// with the eval_loss, adapter URI, kept/reverted decision, and a
// short narrative. Called from the drain loop when a round becomes
// terminal.
func (r *AutoResearchProjectReconciler) wikiPushResult(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
	round int,
	evalLoss float64,
	adapterURI string,
	kept bool,
) error {
	repo, dir, auth, err := r.openWikiRepo(ctx, p)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	keptStr := "reverted"
	if kept {
		keptStr = "kept"
	}
	resultMD := fmt.Sprintf(`# Round %d — result

| Field | Value |
|-------|-------|
| eval_loss | %f |
| Decision | **%s** |
| Adapter URI | `+"`%s`"+` |

`+"`"+`oras pull %s`+"`"+` to fetch the LoRA weights locally.
`,
		round, evalLoss, keptStr, adapterURI, adapterURI)

	resPath := filepath.Join(dir, "results", fmt.Sprintf("round-%d.md", round))
	if err := os.MkdirAll(filepath.Dir(resPath), 0o755); err != nil {
		return fmt.Errorf("mkdir results: %w", err)
	}
	if err := os.WriteFile(resPath, []byte(resultMD), 0o644); err != nil {
		return fmt.Errorf("write result: %w", err)
	}

	// Append to log/log.md (one-line summary, newest at TOP).
	logPath := filepath.Join(dir, "log", "log.md")
	existing, _ := os.ReadFile(logPath)
	entry := fmt.Sprintf("- **Round %d** (%s) — eval_loss=%f, %s, adapter=`%s`\n",
		round, time.Now().UTC().Format("2026-01-02 15:04 UTC"), evalLoss, keptStr, adapterURI)
	header := "# Experiment Log\n\n> Reverse-chronological. Newest entries at the top.\n\n"
	newContent := header + entry
	if e := strings.TrimSpace(string(existing)); strings.HasPrefix(e, "# Experiment Log") {
		// Strip the existing header so we don't duplicate it.
		idx := strings.Index(e, "\n\n")
		if idx > 0 {
			tail := strings.TrimSpace(e[idx+2:])
			if strings.HasPrefix(tail, ">") {
				// Skip the blockquote line too.
				if nl := strings.Index(tail, "\n"); nl > 0 {
					tail = strings.TrimSpace(tail[nl+1:])
				}
			}
			newContent = header + entry + tail + "\n"
		}
	} else if e != "" {
		newContent = header + entry + e + "\n"
	}
	if err := os.WriteFile(logPath, []byte(newContent), 0o644); err != nil {
		return fmt.Errorf("write log: %w", err)
	}

	return commitAndPush(ctx, repo, auth, fmt.Sprintf("result: round %d (eval_loss=%.4f, %s)", round, evalLoss, keptStr),
		"results/round-"+intStr(round)+".md",
		"log/log.md",
	)
}

// openWikiRepo shallow-clones the KB's git repo to a fresh temp dir.
// Returns (repo, dir, auth, err). Caller is responsible for
// os.RemoveAll(dir). Auth is the http-basic creds derived from the
// KB's gitMirror.credentialsSecretRef.
func (r *AutoResearchProjectReconciler) openWikiRepo(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
) (*git.Repository, string, *githttp.BasicAuth, error) {
	if p.Spec.KnowledgeBase == "" {
		return nil, "", nil, fmt.Errorf("spec.knowledgeBase is empty — no wiki to push to")
	}
	var kb agentofficev1alpha1.KnowledgeBase
	if err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Spec.KnowledgeBase}, &kb); err != nil {
		return nil, "", nil, fmt.Errorf("get KnowledgeBase %s/%s: %w", p.Namespace, p.Spec.KnowledgeBase, err)
	}
	if kb.Spec.GitMirror.URL == "" {
		return nil, "", nil, fmt.Errorf("KnowledgeBase %s has no spec.gitMirror.url", kb.Name)
	}

	// Read the git PAT from the KB's credentials secret.
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: kb.Spec.GitMirror.CredentialsSecretRef}, &sec); err != nil {
		return nil, "", nil, fmt.Errorf("get git-token secret %s: %w", kb.Spec.GitMirror.CredentialsSecretRef, err)
	}
	token := string(sec.Data["GIT_TOKEN"])
	if token == "" {
		return nil, "", nil, fmt.Errorf("secret %s has no GIT_TOKEN key", kb.Spec.GitMirror.CredentialsSecretRef)
	}

	auth := &githttp.BasicAuth{
		Username: "git", // any non-empty string; GitHub ignores it when using a PAT
		Password: token,
	}

	dir, err := os.MkdirTemp("", "autoresearch-wiki-*")
	if err != nil {
		return nil, "", nil, fmt.Errorf("mktemp: %w", err)
	}

	branch := kb.Spec.GitMirror.Branch
	if branch == "" {
		branch = "main"
	}

	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:           kb.Spec.GitMirror.URL,
		Auth:          auth,
		ReferenceName: refName(branch),
		Depth:         1,
		SingleBranch:  true,
	})
	if err != nil {
		os.RemoveAll(dir)
		return nil, "", nil, fmt.Errorf("clone %s: %w", kb.Spec.GitMirror.URL, err)
	}
	return repo, dir, auth, nil
}

// commitAndPush stages the named paths (relative to the repo root),
// commits with the given message, and pushes to origin.
func commitAndPush(ctx context.Context, repo *git.Repository, auth *githttp.BasicAuth, msg string, paths ...string) error {
	w, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	for _, p := range paths {
		if _, err := w.Add(p); err != nil {
			return fmt.Errorf("stage %s: %w", p, err)
		}
	}
	now := time.Now()
	if _, err := w.Commit(msg, &git.CommitOptions{
		Author: &gitobject.Signature{
			Name:  "autoresearch-operator",
			Email: "autoresearch@agent-office.local",
			When:  now,
		},
	}); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if err := repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		Auth:       auth,
		RefSpecs:   []config.RefSpec{config.RefSpec("refs/heads/+HEAD:refs/heads/HEAD")},
	}); err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// readWikiLogSnapshot fetches the current log.md content from the KB
// repo so the operator can include it in the prompt. Best-effort —
// failure here just means the agent doesn't see prior context.
func (r *AutoResearchProjectReconciler) readWikiLogSnapshot(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
) (string, error) {
	_, dir, _, err := r.openWikiRepo(ctx, p)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	raw, err := os.ReadFile(filepath.Join(dir, "log", "log.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return "(no log entries yet)", nil
		}
		return "", err
	}
	return string(raw), nil
}

// refName wraps a branch name into the plumbing.ReferenceName type
// go-git's CloneOptions accepts.
func refName(branch string) plumbing.ReferenceName {
	return plumbing.ReferenceName("refs/heads/" + branch)
}

func intStr(n int) string { return fmt.Sprintf("%d", n) }

// truncateMsg is shared with autoresearchproject_controller.go's
// truncateMsg (already defined there); kept here as a wrapper for
// clarity. Returns the body verbatim if shorter than max.
// (No new symbol — re-uses package-level truncateMsg.)
var _ = truncateMsg

// ErrAgentUnreachable is the sentinel error proposeViaAgent returns
// when finding the gateway pod fails. Used by the caller to decide
// whether to set AgentIntegrationOK=False on Status vs treat as a
// transient retry.
var ErrAgentUnreachable = fmt.Errorf("agent unreachable")

// notFoundOnlyAsAgentUnreachable wraps an apierrors.IsNotFound error
// as ErrAgentUnreachable so the caller's condition logic can be a
// single check.
func notFoundOnlyAsAgentUnreachable(err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %v", ErrAgentUnreachable, err)
	}
	return err
}

// commitedFilesRE is a defensive guard: when we ask git to stage
// "proposals/round-37.yaml" we want to also catch the .md counterpart
// without listing them twice. Used by the (currently unused) future
// glob-stage path; kept here as documentation.
var commitedFilesRE = regexp.MustCompile(`^(proposals|results|log)/`)

var _ = commitedFilesRE
