/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Gateway-backed recommendation: run the selection prompt as a real
// model turn through an AgentGateway, on the ChatGPT/Codex
// SUBSCRIPTION.
//
// The obvious approach — POST to an OpenAI-compatible endpoint with an
// API key — cannot use the subscription at all. Subscription auth is
// OAuth against chatgpt.com/backend-api, not a bearer key on
// api.openai.com, and routing a request that way is exactly the
// per-token billing this platform is built to avoid.
//
// The gateway already holds that authenticated session: it is how every
// agent on this cluster talks to gpt-5.6-sol. So the recommender does
// not need credentials of its own — it borrows the runtime that already
// has them, the same way this operator already execs into that pod to
// converge config, approve node hosts, and read activity. No new
// secret, no second auth path to rotate, and the billing route is
// whatever spec.modelAuth already pins.
//
// Configure with:
//
//	AGENT_RECOMMENDER_GATEWAY  AgentGateway name (e.g. research-gateway)
//	AGENT_RECOMMENDER_AGENT    logical agent id to run the turn as
//	AGENT_RECOMMENDER_MODEL    e.g. openai/gpt-5.6-sol
//	AGENT_RECOMMENDER_TIMEOUT  budget for the turn (default 20s)
//
// Unset ⇒ this path is skipped entirely and the deterministic scorer
// answers, so a cluster with no gateway still works.
//
// The turn is bounded by that budget — chosen to stay inside the 30s
// OpenShift route timeout this request arrives through — and its output
// is validated against the live catalog exactly like the HTTP model
// path: a name the catalog does not contain is dropped, and zero
// survivors counts as failure and falls back.

// execInPod runs a command in the named container and returns stdout.
// Mirrors AgentGatewayReconciler.execInGatewayPod; kept separate so the
// catalog handler does not depend on the reconciler.
func (h *CatalogSkillsHandler) execInPod(ctx context.Context, pod *corev1.Pod, container string, cmd []string) (string, error) {
	if h.restConfig == nil {
		return "", fmt.Errorf("no RestConfig; cannot exec")
	}
	cs, err := kubernetes.NewForConfig(h.restConfig)
	if err != nil {
		return "", err
	}
	req := cs.CoreV1().RESTClient().Post().Resource("pods").
		Namespace(pod.Namespace).Name(pod.Name).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(h.restConfig, "POST", req.URL())
	if err != nil {
		return "", err
	}
	var stdout, stderr strings.Builder
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout, Stderr: &stderr,
	}); err != nil {
		return stdout.String(), fmt.Errorf("%w (stderr=%s)", err, truncate(strings.TrimSpace(stderr.String()), 200))
	}
	return stdout.String(), nil
}

// readyGatewayPod finds a Ready pod for the configured gateway.
func (h *CatalogSkillsHandler) readyGatewayPod(ctx context.Context, gwName string) (*corev1.Pod, error) {
	var pods corev1.PodList
	sel := labels.SelectorFromSet(gatewayLabels(gwName))
	if err := h.client.List(ctx, &pods,
		client.InNamespace(h.namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return p, nil
			}
		}
	}
	return nil, fmt.Errorf("no Ready pod for gateway %q", gwName)
}

// recommendViaGateway asks the model — through the gateway, on the
// subscription — to choose packs and draft an identity.
func (h *CatalogSkillsHandler) recommendViaGateway(
	ctx context.Context, desc string, packs []catalogPack,
) (*recommendResponse, error) {
	gw := strings.TrimSpace(os.Getenv("AGENT_RECOMMENDER_GATEWAY"))
	agent := strings.TrimSpace(os.Getenv("AGENT_RECOMMENDER_AGENT"))
	model := strings.TrimSpace(os.Getenv("AGENT_RECOMMENDER_MODEL"))
	if gw == "" || agent == "" {
		return nil, fmt.Errorf("gateway recommender not configured")
	}
	pod, err := h.readyGatewayPod(ctx, gw)
	if err != nil {
		return nil, err
	}

	// Render the CONTAINMENT, not just the names. A flat list makes a
	// parent and its child look like two unrelated options that merely
	// share vocabulary, and the model duly picks both — selecting
	// unreal-scripting AND parkforge-unreal-blueprint-scripting, the one
	// skill that pack contains. It also cannot weigh a meta-pack as
	// "the whole family" when nothing says it holds the other packs.
	var lines []string
	for _, p := range packs {
		kind := p.Type
		if p.ArtifactKind != "" {
			kind = p.ArtifactKind
		}
		state := "installed"
		if !p.Installed {
			state = "available from " + p.Registry
		}
		line := fmt.Sprintf("- %s %q (%s): %s", kind, p.Name, state, p.Description)
		switch {
		case len(p.Members) > 0:
			line += fmt.Sprintf("\n    CONTAINS packs: %s", strings.Join(p.Members, ", "))
		case kind == "pack":
			var kids []string
			for _, c := range packs {
				if c.Member == p.Name {
					kids = append(kids, c.Name)
				}
			}
			if len(kids) > 0 {
				line += fmt.Sprintf("\n    CONTAINS skills: %s", strings.Join(kids, ", "))
			}
		}
		if p.Member != "" {
			line += fmt.Sprintf("\n    INSIDE pack: %s", p.Member)
		}
		lines = append(lines, line)
	}
	prompt := "You compose governed AI agents from a fixed catalog.\n" +
		"Choose ONLY entries from the catalog below — never invent names — and draft an identity.\n" +
		"Prefer few, strongly relevant picks over filling a quota; choosing nothing is better than choosing something unrelated.\n" +
		"\nThe catalog is a HIERARCHY. Selecting a pack installs every skill it contains; " +
		"selecting a meta-pack installs every skill in all of its packs. Therefore:\n" +
		"  * NEVER select both a container and something already inside it — pick one.\n" +
		"  * Pick the SMALLEST container that covers the job: a single skill if one does it; " +
		"its pack if the job spans several skills in that pack; the meta-pack only if the job " +
		"spans most of its packs.\n" +
		"  * A container is worth picking for breadth, not because its name matches — a job about " +
		"one narrow task takes the one skill, even when a whole family shares its vocabulary.\n" +
		"Reply with ONE JSON object and no prose, no code fence:\n" +
		`{"name":"<dns-safe-short-name>","displayName":"...","emoji":"<one emoji>","role":"<one word>",` +
		`"systemPrompt":"<2-4 sentences: the job, which governed tools/skills to lean on, and: never fabricate data — if a tool cannot answer, say so>",` +
		`"packs":[{"name":"<exact catalog name>","reason":"<why, one clause>"}]}` +
		"\n\nCATALOG:\n" + strings.Join(lines, "\n") +
		"\n\nJOB DESCRIPTION:\n" + desc + "\n"

	// base64 the prompt rather than interpolating it into a shell
	// command: descriptions are user input and contain quotes, newlines
	// and $.
	b64 := base64.StdEncoding.EncodeToString([]byte(prompt))
	modelArg := ""
	if model != "" {
		modelArg = "--model " + model + " "
	}

	// Hard budget. This request is answering a browser: it reaches the
	// operator through the RHDH route, and an OpenShift route gives up
	// at 30s by default — that route is Helm-managed, so raising it
	// would drift on the next upgrade. A turn measures ~14s, which
	// leaves little room, so cap the model at a budget that always
	// returns in time and let the deterministic scorer answer if the
	// model is slower. A useful answer late is a 504.
	budget := 20 * time.Second
	if v := strings.TrimSpace(os.Getenv("AGENT_RECOMMENDER_TIMEOUT")); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil && d > 0 {
			budget = d
		}
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// Give the CLI a slightly shorter deadline so it exits on its own
	// and we see its output, rather than having the exec stream torn
	// out from under it.
	cliTimeout := int(budget.Seconds()) - 2
	if cliTimeout < 5 {
		cliTimeout = 5
	}
	script := fmt.Sprintf(
		`cd /home/node && printf %%s '%s' | base64 -d > /tmp/rec-prompt.txt && `+
			`openclaw agent --agent %s %s-m "$(cat /tmp/rec-prompt.txt)" --timeout %d 2>/dev/null`,
		b64, agent, modelArg, cliTimeout)

	out, err := h.execInPod(ctx, pod, "openclaw", []string{"sh", "-c", script})
	if err != nil {
		return nil, fmt.Errorf("gateway turn: %w", err)
	}

	var sel struct {
		recommendIdentity
		Packs []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"packs"`
	}
	// extractJSON strips the CLI's banner/ANSI framing.
	if err := json.Unmarshal([]byte(extractJSON(out)), &sel); err != nil {
		return nil, fmt.Errorf("gateway returned unparseable selection: %w", err)
	}

	byName := map[string]catalogPack{}
	for _, p := range packs {
		byName[p.Name] = p
	}
	chosen := []recommendPack{}
	for _, s := range sel.Packs {
		if p, ok := byName[s.Name]; ok {
			chosen = append(chosen, recommendPack{catalogPack: p, Reason: s.Reason})
		}
	}
	chosen = dropContained(chosen, packs)
	if len(chosen) == 0 {
		return nil, fmt.Errorf("gateway selected no valid packs")
	}
	id := sel.recommendIdentity
	id.Name = sanitizeAgentName(id.Name)
	if id.Name == "" || id.SystemPrompt == "" {
		return nil, fmt.Errorf("gateway returned incomplete identity")
	}
	if id.Role == "" {
		id.Role = "assistant"
	}
	return &recommendResponse{Source: "model:" + model, Identity: id, Packs: chosen}, nil
}

// dropContained removes any selection that a bigger selection already
// installs: a skill whose pack was also chosen, and a pack whose
// meta-pack was also chosen.
//
// The prompt now describes the hierarchy, but a prompt is a request,
// not a guarantee — and a redundant pick is not cosmetic. It bills the
// user for a container they did not need, and the composer would list
// the same skill twice, once on its own and once inside the pack's
// "what's inside".
//
// Keeps the container, drops the contained: the container is the
// superset, so nothing the model asked for is lost.
func dropContained(chosen []recommendPack, all []catalogPack) []recommendPack {
	if len(chosen) < 2 {
		return chosen
	}
	picked := map[string]bool{}
	for _, c := range chosen {
		picked[c.Name] = true
	}
	// pack name -> the meta-pack that lists it.
	parentOfPack := map[string]string{}
	for _, p := range all {
		for _, m := range p.Members {
			parentOfPack[m] = p.Name
		}
	}
	out := make([]recommendPack, 0, len(chosen))
	for _, c := range chosen {
		// A skill inside a chosen pack.
		if c.Member != "" && picked[c.Member] {
			continue
		}
		// A skill whose pack's meta-pack was chosen.
		if c.Member != "" && picked[parentOfPack[c.Member]] {
			continue
		}
		// A pack inside a chosen meta-pack.
		if picked[parentOfPack[c.Name]] {
			continue
		}
		out = append(out, c)
	}
	return out
}
