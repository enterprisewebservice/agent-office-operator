package controller

import (
	"strings"
	"testing"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

func TestRegistrationsSignatureStableAndSensitive(t *testing.T) {
	a := []registrationEntry{{Name: "b", Ready: true, ToolCount: 2}, {Name: "a", Ready: false}}
	b := []registrationEntry{{Name: "a", Ready: false}, {Name: "b", Ready: true, ToolCount: 2}}
	sa, na := registrationsSignature(a)
	sb, nb := registrationsSignature(b)
	if sa != sb || strings.Join(na, ",") != "a,b" || strings.Join(nb, ",") != "a,b" {
		t.Fatalf("order must not matter: %s/%v vs %s/%v", sa, na, sb, nb)
	}
	c := []registrationEntry{{Name: "a", Ready: true}, {Name: "b", Ready: true, ToolCount: 2}}
	if sc, _ := registrationsSignature(c); sc == sa {
		t.Fatalf("readiness must change the signature")
	}
	if se, ne := registrationsSignature(nil); se == "" || len(ne) != 0 {
		t.Fatalf("empty set must still fingerprint: %q %v", se, ne)
	}
}

func TestRegistrationDiff(t *testing.T) {
	if got := registrationDiff([]string{"a"}, []string{"a", "b"}); got != "added b" {
		t.Fatalf("got %q", got)
	}
	if got := registrationDiff([]string{"a", "b"}, []string{"b"}); got != "removed a" {
		t.Fatalf("got %q", got)
	}
	if got := registrationDiff([]string{"a"}, []string{"a"}); got != "a registration changed" {
		t.Fatalf("got %q", got)
	}
}

func TestSkillSetSignature(t *testing.T) {
	mk := func(name, ver string) ResolvedSkill {
		s := ResolvedSkill{Version: ver}
		s.Skill.Name = name
		return s
	}
	got := skillSetSignature([]ResolvedSkill{mk("z", "1"), mk("a", ""), mk("m", "2.0")})
	if strings.Join(got, ",") != "a,m@2.0,z@1" {
		t.Fatalf("got %v", got)
	}
	var s agentofficev1alpha1.Skill
	s.Spec.Version = "3"
	rs := ResolvedSkill{Skill: s}
	rs.Skill.Name = "q"
	if got := skillSetSignature([]ResolvedSkill{rs}); got[0] != "q@3" {
		t.Fatalf("spec version fallback: %v", got)
	}
}
