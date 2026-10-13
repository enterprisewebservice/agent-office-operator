package controller

import "os"

// Image defaults. Every default can be overridden on the operator Deployment
// through an environment variable, so a catalog built for one registry still
// works when the images are mirrored elsewhere (RHDP orders pull from quay.io).
const (
	fallbackOpenClawImage = "quay.io/dpeterso/openclaw:v2026.7.1"
	fallbackSkillsImage   = "quay.io/dpeterso/agent-office-skills:v0.0.1"
	fallbackTrainerImage  = "quay.io/dpeterso/autoresearch-trainer:v0.0.13"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// DefaultOpenClawImage is the OpenClaw runtime image used when an
// AgentGateway or AgentWorkstation does not pin one (env OPENCLAW_IMAGE).
func DefaultOpenClawImage() string { return envOr("OPENCLAW_IMAGE", fallbackOpenClawImage) }

// defaultSkillsImage is the skills catalog image mounted into gateway pods
// when the AgentGateway does not set spec.skillsImage (env SKILLS_IMAGE).
func defaultSkillsImage() string { return envOr("SKILLS_IMAGE", fallbackSkillsImage) }

// defaultTrainerImage is the AutoResearch trainer image (env TRAINER_IMAGE).
func defaultTrainerImage() string { return envOr("TRAINER_IMAGE", fallbackTrainerImage) }
