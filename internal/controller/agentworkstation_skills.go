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

package controller

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// ResolvedSkill is the per-AW result of skill-binding resolution:
// the skill's spec, the binding overrides that won the conflict
// resolution, and the final rendered SKILL.md content ready to be
// written to the agent's workspace.
type ResolvedSkill struct {
	Skill       agentofficev1alpha1.Skill
	BindingName string                              // which SkillBinding granted it (winner, after lex-sort last-wins)
	Overrides   *agentofficev1alpha1.SkillOverrides // may be nil
	Version     string                              // resolved version label, mirrors SkillBinding.status.resolvedSkillVersion
	Rendered    string                              // final SKILL.md body (base content + overrides + invocation template)
}

// listAppliedSkills resolves every SkillBinding in the AW's
// namespace, keeps the ones that apply to this AW (direct
// subjects + selector match), groups them by skill name, picks
// a deterministic winner per skill (lex-sorted by binding name,
// last-wins), and returns a sorted []ResolvedSkill ready to be
// rendered into the agent workspace.
//
// Errors loading individual Skill / SkillBinding resources are
// non-fatal — they're logged and the offending entry is skipped
// so a single misconfigured binding can't prevent the rest of an
// agent's skills from rendering.
func (r *AgentWorkstationReconciler) listAppliedSkills(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) ([]ResolvedSkill, error) {
	var bindings agentofficev1alpha1.SkillBindingList
	if err := r.List(ctx, &bindings, client.InNamespace(aw.Namespace)); err != nil {
		return nil, fmt.Errorf("listing SkillBindings: %w", err)
	}

	// Group bindings that apply to this AW by skill name.
	bySkill := map[string][]agentofficev1alpha1.SkillBinding{}
	for _, sb := range bindings.Items {
		if !skillBindingAppliesToAW(&sb, aw) {
			continue
		}
		bySkill[sb.Spec.SkillRef.Name] = append(bySkill[sb.Spec.SkillRef.Name], sb)
	}

	resolved := make([]ResolvedSkill, 0, len(bySkill))
	for skillName, granters := range bySkill {
		// Conflict policy: deterministic, lex-sorted by binding
		// name, last-wins on overrides. The SkillBinding
		// controller surfaces overlaps in status.conflicts so
		// users see which bindings overlap.
		sort.SliceStable(granters, func(i, j int) bool {
			return granters[i].Name < granters[j].Name
		})
		winner := granters[len(granters)-1]

		// Resolve the Skill itself. Missing skill → skip with a
		// non-fatal log; the SkillBinding's status surface
		// already records the resolution failure.
		var skill agentofficev1alpha1.Skill
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: aw.Namespace, Name: skillName,
		}, &skill); err != nil {
			continue
		}
		// Version pin must match if specified — otherwise the
		// binding shouldn't apply.
		if winner.Spec.SkillRef.Version != "" && winner.Spec.SkillRef.Version != skill.Spec.Version {
			continue
		}

		// Resolve content from the source. Inline beats nothing,
		// configMapRef beats both. Failure to resolve is
		// non-fatal — render whatever we have (possibly just the
		// invocation template body).
		base, _ := r.resolveSkillContent(ctx, &skill)

		rendered := renderSkill(skill, base, winner.Spec.Overrides)

		resolved = append(resolved, ResolvedSkill{
			Skill:       skill,
			BindingName: winner.Name,
			Overrides:   winner.Spec.Overrides,
			Version:     resolvedVersionLabel(winner.Spec.SkillRef.Version, skill.Spec.Version),
			Rendered:    rendered,
		})
	}

	// Stable order so the workspace seed map produces consistent
	// hashes and reconciles are no-ops when nothing changed.
	sort.Slice(resolved, func(i, j int) bool {
		return resolved[i].Skill.Name < resolved[j].Skill.Name
	})
	return resolved, nil
}

// listAllCatalogSkills returns every Skill CR in the namespace as a
// []ResolvedSkill, rendered into SKILL.md form ready to be written to
// the agent's workspace.
//
// This is the v1.6.0 "discovery" path. Unlike listAppliedSkills (the
// binding path), it does NOT filter by SkillBinding — every agent
// gets the FULL local catalog rendered into its workspace. The
// runtime then chooses at runtime via progressive disclosure on the
// directory: scan SKILL.md frontmatter for metadata, load full body
// only on trigger.
//
// Why "render everything to every agent" doesn't blow up:
//   - The disk cost is small (each SKILL.md is markdown, kilobytes).
//   - The runtime's discovery model means most skills are never
//     loaded into context — they sit on disk as available reference.
//   - The alternative (filter at render-time via SkillBinding) is
//     the pre-discovery binding model we're explicitly moving away
//     from.
//
// SkillBinding's Overrides field is intentionally ignored here:
// overrides are a per-binding concept that doesn't fit the catalog
// model. If/when per-agent skill customization is needed again,
// it'll be added as a different mechanism (probably an annotation on
// the AW pointing at an overlay ConfigMap).
//
// Errors loading individual Skill content are non-fatal — the
// offending skill is skipped so one misconfigured Skill can't
// prevent the rest of the catalog from rendering.
func (r *AgentWorkstationReconciler) listAllCatalogSkills(ctx context.Context, namespace string) ([]ResolvedSkill, error) {
	var skills agentofficev1alpha1.SkillList
	if err := r.List(ctx, &skills, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing Skills: %w", err)
	}

	resolved := make([]ResolvedSkill, 0, len(skills.Items))
	for i := range skills.Items {
		skill := skills.Items[i]
		// Resolve content from source. Failure is non-fatal.
		base, _ := r.resolveSkillContent(ctx, &skill)
		// No SkillBinding overrides in the catalog model — pass nil.
		rendered := renderSkill(skill, base, nil)
		resolved = append(resolved, ResolvedSkill{
			Skill:    skill,
			Version:  skill.Spec.Version,
			Rendered: rendered,
			// BindingName + Overrides intentionally empty — this
			// skill was sourced from the catalog, not granted via
			// any specific binding.
		})
	}

	// Stable order so the workspace seed map produces consistent
	// hashes and reconciles are no-ops when the catalog hasn't
	// changed.
	sort.Slice(resolved, func(i, j int) bool {
		return resolved[i].Skill.Name < resolved[j].Skill.Name
	})
	return resolved, nil
}

// resolveSkillContent reads the skill's `SKILL.md` source.
// configMapRef wins over inline. If neither is set, returns
// empty + an error so the caller can decide whether to fall
// back to a pure-template skill.
func (r *AgentWorkstationReconciler) resolveSkillContent(ctx context.Context, skill *agentofficev1alpha1.Skill) (string, error) {
	src := skill.Spec.Source
	if src.ConfigMapRef != nil {
		var cm corev1.ConfigMap
		key := types.NamespacedName{Namespace: skill.Namespace, Name: src.ConfigMapRef.Name}
		if err := r.Get(ctx, key, &cm); err != nil {
			return "", fmt.Errorf("ConfigMap %s/%s: %w", key.Namespace, key.Name, err)
		}
		val, ok := cm.Data[src.ConfigMapRef.Key]
		if !ok {
			return "", fmt.Errorf("ConfigMap %s/%s missing key %q", key.Namespace, key.Name, src.ConfigMapRef.Key)
		}
		return val, nil
	}
	if src.Inline != "" {
		return src.Inline, nil
	}
	return "", fmt.Errorf("skill source has neither configMapRef nor inline content")
}

// skillBindingAppliesToAW reports whether `sb` should grant its
// skill to `aw`. Direct subjects and selector matches are unioned;
// empty subjects + nil selector is intentionally a no-match (so a
// typo doesn't silently grant a skill cluster-wide).
func skillBindingAppliesToAW(sb *agentofficev1alpha1.SkillBinding, aw *agentofficev1alpha1.AgentWorkstation) bool {
	for _, s := range sb.Spec.Subjects {
		if s.Kind == "AgentWorkstation" && s.Name == aw.Name {
			return true
		}
	}
	if sb.Spec.SubjectSelector == nil {
		return false
	}
	ls := sb.Spec.SubjectSelector
	// Defensive: empty matchLabels + empty matchExpressions
	// matches everything in stock Kubernetes semantics, which is
	// almost certainly a typo at this scale. Treat as no-match.
	if len(ls.MatchLabels) == 0 && len(ls.MatchExpressions) == 0 {
		return false
	}
	sel, err := metav1.LabelSelectorAsSelector(ls)
	if err != nil {
		return false
	}
	return sel.Matches(labels.Set(aw.Labels))
}

// resolvedVersionLabel mirrors the SkillBinding controller's
// resolvedSkillVersion convention so the AW reconciler and the
// binding controller agree on what "version" means.
func resolvedVersionLabel(pinned, skillVersion string) string {
	if pinned != "" {
		return pinned
	}
	if skillVersion != "" {
		return "floating:" + skillVersion
	}
	return "floating"
}

// renderSkill composes the final SKILL.md body:
//  1. The base content (from configMapRef or inline) — typically
//     a markdown procedure description authored in git.
//  2. Optional Go-template body from spec.invocation.promptTemplate
//     rendered with {Skill, BindingOverrides} as context.
//  3. The binding's promptAdditions appended at the end so per-AW
//     narrowing comes after the base authored content.
//
// Template execution failures degrade gracefully: we log the
// failure (in the caller, via the returned content body containing
// a `<!-- skill template error: ... -->` HTML comment) and ship
// what we have rather than block the agent from getting any
// skill content.
func renderSkill(skill agentofficev1alpha1.Skill, base string, overrides *agentofficev1alpha1.SkillOverrides) string {
	var b strings.Builder

	// Header: skill identity, helps the LLM ground when reading
	// SKILL_<name>.md in its working set.
	displayName := skill.Spec.DisplayName
	if displayName == "" {
		displayName = skill.Name
	}
	fmt.Fprintf(&b, "# %s\n\n", displayName)
	if skill.Spec.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", skill.Spec.Description)
	}
	if skill.Spec.Invocation.Tool != "" {
		fmt.Fprintf(&b, "*This skill is built on the `%s` tool. Invoke it through that tool's normal interface.*\n\n", skill.Spec.Invocation.Tool)
	}

	// Body: base content (authored .md from git or inline).
	if base != "" {
		b.WriteString(strings.TrimRight(base, "\n"))
		b.WriteString("\n\n")
	}

	// Optional template render. Pure-template skills (no base
	// .md) are valid — the template *is* the body in that case.
	if tpl := strings.TrimSpace(skill.Spec.Invocation.PromptTemplate); tpl != "" {
		t, err := template.New("skill").Parse(tpl)
		if err != nil {
			fmt.Fprintf(&b, "<!-- skill template parse error: %v -->\n\n", err)
		} else {
			data := map[string]any{
				"Skill":            skill,
				"BindingOverrides": overridesParams(overrides),
			}
			var rendered bytes.Buffer
			if err := t.Execute(&rendered, data); err != nil {
				fmt.Fprintf(&b, "<!-- skill template render error: %v -->\n\n", err)
			} else {
				b.WriteString(strings.TrimRight(rendered.String(), "\n"))
				b.WriteString("\n\n")
			}
		}
	}

	// Trailer: binding promptAdditions, rendered last so per-AW
	// narrowing layers on top of the base + template body.
	if overrides != nil && strings.TrimSpace(overrides.PromptAdditions) != "" {
		b.WriteString("---\n\n")
		b.WriteString(strings.TrimRight(overrides.PromptAdditions, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// overridesParams pulls the `InvocationParams` map out of the
// (possibly nil) overrides struct, returning a deterministic
// empty map when none are set so templates referencing
// `{{.BindingOverrides.foo}}` don't blow up on nil.
func overridesParams(o *agentofficev1alpha1.SkillOverrides) map[string]string {
	if o == nil || o.InvocationParams == nil {
		return map[string]string{}
	}
	return o.InvocationParams
}

// renderWikiMd returns the body of the agent's `WIKI.md`
// workspace file listing every KnowledgeBase attached to the
// AW's gateway. Empty string when the AW has no gateway (i.e.
// dedicated runtime) or no KBs are attached — caller skips the
// write in that case.
//
// Format is intentionally Markdown that LLMs read like a manual:
// the agent learns the wiki name, mount path, and what skills
// to use against it. This file is the bridge between the
// declarative KnowledgeBase + Skill CRDs and the
// inference-time prompt the agent consumes.
func (r *AgentWorkstationReconciler) renderWikiMd(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation, gwName string) (string, error) {
	if gwName == "" {
		return "", nil
	}
	var kbList agentofficev1alpha1.KnowledgeBaseList
	if err := r.List(ctx, &kbList, client.InNamespace(aw.Namespace)); err != nil {
		return "", err
	}
	type entry struct {
		Name        string
		DisplayName string
		Description string
		MountPath   string
	}
	var entries []entry
	for _, kb := range kbList.Items {
		if kb.Spec.GatewayRef.Name != gwName {
			continue
		}
		dn := kb.Spec.DisplayName
		if dn == "" {
			dn = kb.Name
		}
		entries = append(entries, entry{
			Name:        kb.Name,
			DisplayName: dn,
			Description: kb.Spec.Description,
			MountPath:   "/home/node/.openclaw/wiki/" + kb.Name,
		})
	}
	if len(entries) == 0 {
		return "", nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	var b strings.Builder
	b.WriteString("# Available Knowledge Bases\n\n")
	b.WriteString("These wikis are mounted into the gateway pod and shared across every logical agent. Read existing articles before answering questions; file new findings back into the wiki so future sessions benefit.\n\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "## %s\n\n", e.DisplayName)
		if e.Description != "" {
			fmt.Fprintf(&b, "%s\n\n", e.Description)
		}
		fmt.Fprintf(&b, "- **Mount path**: `%s`\n", e.MountPath)
		fmt.Fprintf(&b, "- **KB name**: `%s` (use this when calling `wiki-*` skills)\n", e.Name)
		b.WriteString("- **Conventions**: `raw/clips/` for unsorted captures, `topics/` for curated articles, `concepts/` for canonical concept pages, `queries/` for query-result snapshots, `_lint/` for nightly lint reports, `_index.md` for the manifest, **`log.md` for the chronological activity feed**.\n\n")
	}
	b.WriteString("## Reading + writing\n\n")
	b.WriteString("- **Search**: prefer the `wiki-search` skill (or fall back to `exec` running `rg <query> /home/node/.openclaw/wiki/<kb>/`).\n")
	b.WriteString("- **Clip a URL**: use the `wiki-clip` skill — it captures the page through the VM browser and files clean Markdown into `raw/clips/`.\n")
	b.WriteString("- **File an article**: use the `wiki-write` skill so the article includes correct frontmatter, backlinks, and entries in `_index.md`.\n")
	b.WriteString("- **Log every meaningful operation**: every `wiki-clip`, `wiki-write`, and substantial `wiki-search` skill appends a single entry to `log.md` with the format `## [YYYY-MM-DD] <op> | <subject>`. This makes the wiki's history greppable and gives the linter agent a structured timeline to walk.\n")
	b.WriteString("- **Cite**: when answering from the wiki, cite the source article path so the user can navigate to it directly.\n")
	return b.String(), nil
}
