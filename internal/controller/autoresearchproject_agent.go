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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
	"github.com/enterprisewebservice/agent-office-operator/internal/wiki"
)

// agentExecTimeout caps how long the operator will wait on
// `openclaw agent` to return. The LLM reasoning + tool calls can
// legitimately take a minute or two; anything longer is a stuck call.
const agentExecTimeout = 10 * time.Minute

// gatewayContainerName is the openclaw container inside the
// research-gateway pod we exec into.
const gatewayContainerName = "openclaw"

// PRE-v0.2.0 architecture: proposeViaAgent ran one agent turn via
// the gateway, parsed the reply as a QLoRAConfig, and was on the
// proposal critical path. Removed in v1.1.0 — the deterministic
// goptuna-backed searcher (internal/search) owns proposal
// generation now; the LLM agent runs as the strategist on cadence
// (see autoresearchproject_strategist.go), writing HYPOTHESES.md
// instead of raw configs. The helpers proposeViaAgent depended on
// (parseAgentProposal, stripCodeFences, extractJSONObject,
// buildExperimenterPrompt, seedWorkspaceLog) are also gone.
//
// Kept from the old agent-path file because the strategist runner
// still uses them:
//
//   * findGatewayPodForAgent — locates the openclaw pod for the
//     strategist's AgentWorkstation.
//   * execInGatewayPod        — execs `openclaw agent --json` for
//     one strategist turn.
//   * validateQLoRAConfig    — sanity-checks a config before it's
//     submitted to DSP. Now also called from
//     internal/controller/autoresearchproject_render_test.go.

// findGatewayPodForAgent locates the openclaw container we need to
// exec into for an agent turn. Handles both runtime modes the AW
// API supports:
//
//   * runtime.shared.gatewayRef set  → look up the AgentGateway's
//     Deployment pod (labeled agentoffice.ai/gateway=<name>).
//   * runtime.dedicated (or runtime nil — same default)
//                                    → look up the AW's own
//     Deployment pod (labeled agentoffice.ai/agent=<name>, set by
//     agentworkstation_resources.agentLabels()).
//
// Either way, every Codex-backed agent in the agent-office
// namespace mounts the same `codex-subscription-credentials`
// Secret at ~/.codex/auth.json — so the exec target's container
// has the OAuth token regardless of which runtime model we picked.
func (r *AutoResearchProjectReconciler) findGatewayPodForAgent(
	ctx context.Context,
	namespace, agentName string,
) (*corev1.Pod, error) {
	var aw agentofficev1alpha1.AgentWorkstation
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: agentName}, &aw); err != nil {
		return nil, fmt.Errorf("get AgentWorkstation %s/%s: %w", namespace, agentName, err)
	}

	// Compute the label selector for the right pod set, depending
	// on which runtime the user picked in the template.
	var (
		selector map[string]string
		descr    string
	)
	if aw.Spec.Runtime != nil && aw.Spec.Runtime.Shared != nil {
		gwName := aw.Spec.Runtime.Shared.GatewayRef
		if gwName == "" {
			return nil, fmt.Errorf("AgentWorkstation %s has spec.runtime.shared but no gatewayRef", agentName)
		}
		selector = map[string]string{"agentoffice.ai/gateway": gwName}
		descr = fmt.Sprintf("AgentGateway %s/%s", namespace, gwName)
	} else {
		// Default (nil runtime OR explicit runtime.dedicated): the AW
		// has its own Deployment + Pod, labeled by agentLabels().
		selector = map[string]string{"agentoffice.ai/agent": agentName}
		descr = fmt.Sprintf("AgentWorkstation %s/%s (dedicated)", namespace, agentName)
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(namespace),
		client.MatchingLabels(selector),
	); err != nil {
		return nil, fmt.Errorf("list pods for %s: %w", descr, err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodRunning && podReady(p) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no Ready pod found for %s (found %d candidates)", descr, len(pods.Items))
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
	// IMPORTANT: stdout and stderr MUST be separate buffers.
	// client-go's SPDY executor writes the two streams from separate
	// goroutines, and bytes.Buffer is not safe for concurrent writes.
	// Pointing both at the same buffer is a data race that can
	// silently drop the entire stdout payload — observed in the
	// wild as `parse agent reply: empty reply` even when the
	// openclaw process emitted a full JSON envelope. (Reproduces
	// only under the operator's runtime; manual `oc exec` works
	// because the CLI buffers locally.)
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		// On error, return both streams concatenated so the caller
		// sees whatever diagnostic the binary printed.
		combined := stdout.String() + stderr.String()
		return combined, fmt.Errorf("stream: %w", err)
	}
	return stdout.String(), nil
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

// wikiPushProposal writes proposals/round-N.yaml + a Markdown
// summary into the KB's wiki repo via a single atomic transaction
// (clone → write → commit → push). Idempotent on the round number —
// re-runs replace the prior round-N file rather than appending.
//
// v0.1.0 (Phase 0 of the autoresearch flywheel refactor): rewritten
// to use internal/wiki.Transaction. The old `openWikiRepo +
// commitAndPush` helper pair carried a malformed refspec
// (`refs/heads/+HEAD:refs/heads/HEAD`) that silently no-op'd every
// wiki push for the entire lifetime of the operator. See
// internal/wiki/transaction.go for the post-mortem.
func (r *AutoResearchProjectReconciler) wikiPushProposal(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
	round int,
	cfg QLoRAConfig,
	reasoning string,
) error {
	tx, err := r.beginWikiTx(ctx, p)
	if err != nil {
		return err
	}
	defer tx.Close()

	cfgYAML, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal proposal: %w", err)
	}
	if err := tx.Write(fmt.Sprintf("proposals/round-%d.yaml", round), cfgYAML); err != nil {
		return err
	}
	md := fmt.Sprintf("# Round %d — proposal\n\n**Agent reasoning:**\n\n%s\n\n**Config (also in `round-%d.yaml`):**\n\n```yaml\n%s```\n",
		round, reasoning, round, string(cfgYAML))
	if err := tx.Write(fmt.Sprintf("proposals/round-%d.md", round), []byte(md)); err != nil {
		return err
	}
	return tx.CommitAndPush(ctx, fmt.Sprintf("propose: round %d", round))
}

// wikiPushResult writes results/round-N.md + updates log/log.md
// with the eval_loss, adapter URI, kept/reverted decision, and a
// short narrative. Called from the drain loop when a round becomes
// terminal.
//
// v0.1.0: rewritten on top of wiki.Transaction (see wikiPushProposal
// post-mortem). The log.md merge logic preserves the existing
// entries-newest-at-top ordering using tx.Read for the current
// content.
func (r *AutoResearchProjectReconciler) wikiPushResult(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
	round int,
	evalLoss float64,
	adapterURI string,
	kept bool,
) error {
	tx, err := r.beginWikiTx(ctx, p)
	if err != nil {
		return err
	}
	defer tx.Close()

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
	if err := tx.Write(fmt.Sprintf("results/round-%d.md", round), []byte(resultMD)); err != nil {
		return err
	}

	// Update log/log.md: read existing, build new with the new
	// entry at the top, write back.
	existing, err := tx.Read("log/log.md")
	if err != nil {
		return fmt.Errorf("read log/log.md: %w", err)
	}
	entry := fmt.Sprintf("- **Round %d** (%s) — eval_loss=%f, %s, adapter=`%s`\n",
		round, time.Now().UTC().Format("2006-01-02 15:04 UTC"), evalLoss, keptStr, adapterURI)
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
	if err := tx.Write("log/log.md", []byte(newContent)); err != nil {
		return err
	}

	return tx.CommitAndPush(ctx, fmt.Sprintf("result: round %d (eval_loss=%.4f, %s)", round, evalLoss, keptStr))
}

// beginWikiTx is the controller's adapter onto wiki.Transaction:
// looks up the project's KnowledgeBase + opens a transaction using
// GitHubAppBasicAuth as the token minter. Centralized here so the
// per-CR ARP, the future strategist, and any other wiki-writing
// path go through the same auth resolution.
func (r *AutoResearchProjectReconciler) beginWikiTx(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
) (*wiki.Transaction, error) {
	if p.Spec.KnowledgeBase == "" {
		return nil, fmt.Errorf("spec.knowledgeBase is empty — no wiki to push to")
	}
	var kb agentofficev1alpha1.KnowledgeBase
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: p.Namespace,
		Name:      p.Spec.KnowledgeBase,
	}, &kb); err != nil {
		return nil, fmt.Errorf("get KnowledgeBase %s/%s: %w",
			p.Namespace, p.Spec.KnowledgeBase, err)
	}
	tx, err := wiki.Begin(ctx, r.Client, &kb, GitHubAppBasicAuth)
	if err != nil {
		return nil, err
	}
	return tx, nil
}

// readWikiLogSnapshot fetches the current log.md content from the
// KB repo so the operator can include it in the prompt. Routes
// through wiki.Transaction (open in read-only mode — no Write/
// Commit/Push calls — so the temp dir is cleaned by Close).
func (r *AutoResearchProjectReconciler) readWikiLogSnapshot(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
) (string, error) {
	tx, err := r.beginWikiTx(ctx, p)
	if err != nil {
		return "", err
	}
	defer tx.Close()
	raw, err := tx.Read("log/log.md")
	if err != nil {
		return "", err
	}
	if raw == nil {
		return "(no log entries yet)", nil
	}
	return string(raw), nil
}

// truncateMsg is shared with autoresearchproject_controller.go's
// truncateMsg (already defined there); kept here as a wrapper for
// clarity. Returns the body verbatim if shorter than max.
// (No new symbol — re-uses package-level truncateMsg.)
var _ = truncateMsg

// Removed in Phase 0 (v0.1.0) of the autoresearch refactor:
//
//   - resolveWikiAuth (no longer needed; internal/wiki uses
//     GitHubAppBasicAuth directly as the TokenMinter callback).
//   - openWikiRepo, commitAndPush (replaced by wiki.Transaction).
//   - refName, intStr (helpers only used by the removed code).
//   - commitedFilesRE (vestigial — explicit `tx.Write(path,...)`
//     calls already give us per-file commits without globbing).
//   - ErrAgentUnreachable, notFoundOnlyAsAgentUnreachable (unused
//     in the v0.1.0 propose path; the caller handles errors
//     directly without sentinel-error inspection).
//
// See internal/wiki/transaction.go for the replacement and the
// refspec post-mortem.
