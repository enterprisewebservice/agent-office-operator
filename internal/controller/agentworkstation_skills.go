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
