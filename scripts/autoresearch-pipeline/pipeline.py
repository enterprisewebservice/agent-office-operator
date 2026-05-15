"""Kubeflow Pipelines v2 pipeline for AutoResearch QLoRA experiments.

Compiled to pipeline.yaml in the same directory. The operator embeds
that compiled YAML at build time, uploads it to DSP on startup if
missing, then submits Runs against it each cycle.

Single-step pipeline: the train container runs the AutoResearch
trainer image we already built (autoresearch-trainer:v0.0.1). Each
run parameter (base model, dataset, QLoRA config JSON, eval metric)
flows through to the trainer's env vars exactly as the v0.0.1 raw-
Job path used. The trainer's existing AUTORESEARCH_RESULT=... stdout
line is parsed by the operator the same way it parses Job logs; we
additionally emit a kfp metric so the value shows up as a sortable
column in OpenShift AI's Experiments UI.

Why a single step instead of a clone-patch-train-eval graph:
- The trainer already does clone + patch (HF model download) + train
  + eval in one process. Breaking that into separate kfp steps
  would force checkpoint passing between steps, blow up artifact
  storage, and slow each cycle by minutes of overhead.
- v2 design space remains open: we can add a separate "deep eval"
  step (LMEvalJob-equivalent) AFTER the train step in a later
  version. For v0.0.2 we keep it lean.

Compile:
  pip install kfp
  python pipeline.py     # writes pipeline.yaml in cwd
"""

from kfp import dsl, compiler


@dsl.container_component
def train_qlora(
    base_model: str,
    base_model_revision: str,
    training_data: str,
    training_split: str,
    training_sample_count: int,
    qlora_config_json: str,
    eval_metric: str,
    eval_direction: str,
    autoresearch_project: str,
    autoresearch_round: int,
    autoresearch_run_id: str,
    metrics: dsl.Output[dsl.Metrics],
):
    """Runs one QLoRA fine-tune + eval cycle inside the
    AutoResearch trainer image. Emits eval_loss as a kfp Metric
    so it surfaces in the Experiments UI; also leaves a parsable
    AUTORESEARCH_RESULT= line in stdout for the operator's
    log-scrape fallback."""
    return dsl.ContainerSpec(
        # Trainer image we built in Phase 0 (Konflux). v0.0.2
        # adds CLI-arg support to run.py — required because
        # kfp v2 container components pass inputs as args, not
        # env vars. v0.0.1 of the image won't work in this
        # pipeline.
        # The funny hostname is the Quay route pattern
        # (route=quay-quay in namespace=quay-test).
        image="quay-quay-quay-test.apps.salamander.aimlworkbench.com/deanpeterson/autoresearch-trainer:v0.0.2",
        command=["python", "/opt/autoresearch/run.py"],
        args=[
            "--base-model", base_model,
            "--base-model-revision", base_model_revision,
            "--training-data", training_data,
            "--training-split", training_split,
            "--training-sample-count", training_sample_count,
            "--qlora-config-json", qlora_config_json,
            "--eval-metric", eval_metric,
            "--eval-direction", eval_direction,
            "--autoresearch-project", autoresearch_project,
            "--autoresearch-round", autoresearch_round,
            "--autoresearch-run-id", autoresearch_run_id,
            # kfp's metrics output path placeholder — gets
            # substituted at runtime with the per-run artifact
            # destination. Trainer writes the kfp metrics file
            # there; DSP catalogs it and OpenShift AI's
            # Experiments UI renders it.
            "--metrics-output-path", metrics.path,
        ],
    )


@dsl.pipeline(
    name="autoresearch-qlora-train-eval",
    description="One QLoRA fine-tune + eval cycle for an AutoResearchProject. "
                "Operator submits one Run per experiment cycle; eval_loss feeds "
                "the keep/revert decision.",
)
def autoresearch_qlora_pipeline(
    base_model: str = "ibm-granite/granite-3.1-8b-instruct",
    base_model_revision: str = "main",
    training_data: str = "tatsu-lab/alpaca",
    training_split: str = "train",
    training_sample_count: int = 2000,
    qlora_config_json: str = '{}',
    eval_metric: str = "eval_loss",
    eval_direction: str = "minimize",
    autoresearch_project: str = "",
    autoresearch_round: int = 0,
    autoresearch_run_id: str = "",
):
    """One-step pipeline wrapping the AutoResearch trainer image."""
    step = train_qlora(
        base_model=base_model,
        base_model_revision=base_model_revision,
        training_data=training_data,
        training_split=training_split,
        training_sample_count=training_sample_count,
        qlora_config_json=qlora_config_json,
        eval_metric=eval_metric,
        eval_direction=eval_direction,
        autoresearch_project=autoresearch_project,
        autoresearch_round=autoresearch_round,
        autoresearch_run_id=autoresearch_run_id,
    )
    # GPU resource request. Single GPU per experiment in v0.0.2;
    # multi-GPU FSDP comes later (Phase 5 of the roadmap).
    step.set_accelerator_type("nvidia.com/gpu").set_accelerator_limit(1)
    # Memory + CPU reservations matching what the raw-Job version
    # asked for.
    step.set_memory_limit("64Gi").set_memory_request("16Gi")
    step.set_cpu_request("2").set_cpu_limit("8")
    # Per-experiment timeout. 35 minutes covers cold image pull +
    # model download + 200-step training + eval at the 250W cap.
    step.set_caching_options(False)


if __name__ == "__main__":
    compiler.Compiler().compile(
        pipeline_func=autoresearch_qlora_pipeline,
        package_path="pipeline.yaml",
    )
    print("compiled pipeline.yaml")
