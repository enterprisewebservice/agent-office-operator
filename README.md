# Agent Office Operator

OLM-managed OpenShift operator for the **Governed Agent Platform**.

Owns two CRDs in the `agentoffice.ai` API group:

- **`AgentWorkstation`** (`aw`) — one governed coding-agent instance. The
  operator reconciles its Deployment, Service, ConfigMap, PVC, and Secret
  references.
- **`MemoryModule`** (`mm`) — shared `.md` content (AGENTS.md, USER.md,
  SKILL_*.md) referenced by one or more AgentWorkstations. The operator
  computes a content hash and a referenced-by index so the UI can show
  which agents share which memory.

## Architecture

The operator follows the AAIF (Agentic AI Foundation, Linux Foundation,
December 2025) cross-tool conventions for `AGENTS.md` and the Anthropic
Skills Open Standard for `SKILL_<name>.md` packaging.

It ships a **ConsolePlugin** that adds Memory Module and Agent Workstation
tabs to the operator's CSV detail page in the OpenShift Console.

## Build pipeline

Three Tekton PipelineRuns in `.tekton/`, all triggered by Pipelines-as-Code
on push to `main`:

| Pipeline | Output |
|---|---|
| `operator-image-on-push.yaml` | `quay.../agent-office-operator:v0.0.1` |
| `operator-bundle-on-push.yaml` | `quay.../agent-office-operator-bundle:v0.0.1` |
| `operator-catalog-on-push.yaml` | `quay.../agent-office-operator-catalog:v0.0.1` |

OLM's `registryPoll` on the `CatalogSource` detects digest changes behind
the stable tags every 5 minutes, so the tile in **Ecosystem Software
Catalog** auto-updates without ArgoCD Image Updater wiring.

## Related repos

- [`agent-office`](https://github.com/enterprisewebservice/agent-office) —
  the office UI (Map / Discord / WebRTC) and (legacy) Go backend.
- [`agent-office-memory-modules`](https://github.com/enterprisewebservice/agent-office-memory-modules) —
  shared `.md` content for the `MemoryModule` CRs the operator manages.

## License

Apache 2.0.
