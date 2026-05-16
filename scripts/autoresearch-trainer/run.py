#!/usr/bin/env python3
"""AutoResearch trainer entrypoint.

The AutoResearchProject reconciler submits a Kubernetes Job that
runs this script as the trainer container's command. The script:

  1. Reads its QLoRA config from $AUTORESEARCH_CONFIG (JSON).
  2. Loads the base model in 4-bit (via bitsandbytes).
  3. Applies LoRA adapters per the proposed config.
  4. Fine-tunes on the configured Hugging Face dataset.
  5. Runs inline eval on a held-out slice → eval_loss.
  6. Emits a single line `AUTORESEARCH_RESULT=<json>` to stdout
     that the operator's parseTrainerResult() picks up.

Designed to fail safely: any exception emits an error result line
and exits 1, so the operator sees a definite outcome (Job Failed
state) rather than an indefinite "still running."

Env vars read:
  AUTORESEARCH_CONFIG          — JSON-encoded QLoRAConfig from the agent
  AUTORESEARCH_PROJECT         — AutoResearchProject name
  AUTORESEARCH_ROUND           — round number for this experiment
  AUTORESEARCH_RUN_ID          — unique ID for this Job
  BASE_MODEL                   — Hugging Face model ID (e.g. ibm-granite/granite-3.1-8b-instruct)
  BASE_MODEL_REVISION          — branch/tag/commit (default: main)
  TRAINING_DATA                — HF dataset ID (e.g. tatsu-lab/alpaca)
  TRAINING_SPLIT               — dataset split (default: train)
  TRAINING_SAMPLE_COUNT        — cap on samples used (default: 2000)
  EVAL_METRIC                  — for now only eval_loss is supported
  EVAL_DIRECTION               — minimize|maximize (default: minimize)
  HF_TOKEN                     — optional, for gated models
"""

import argparse
import json
import os
import pathlib
import sys
import time
import traceback

# Result delimiters the operator's parseTrainerResult() reads.
RESULT_PREFIX = "AUTORESEARCH_RESULT="


def _parse_args() -> argparse.Namespace:
    """Accept both CLI args (kfp container component pattern) and
    env vars (raw Job pattern). CLI args win when both are set.
    Keeps the script compatible with v0.0.1's raw-Job invocation
    while supporting v0.0.2's kfp pipeline invocation."""
    p = argparse.ArgumentParser()
    p.add_argument("--base-model", default=os.environ.get("BASE_MODEL"))
    p.add_argument("--base-model-revision", default=os.environ.get("BASE_MODEL_REVISION", "main"))
    p.add_argument("--training-data", default=os.environ.get("TRAINING_DATA"))
    p.add_argument("--training-split", default=os.environ.get("TRAINING_SPLIT", "train"))
    p.add_argument("--training-sample-count", type=int, default=int(os.environ.get("TRAINING_SAMPLE_COUNT", "2000")))
    p.add_argument("--qlora-config-json", default=os.environ.get("AUTORESEARCH_CONFIG", "{}"))
    p.add_argument("--eval-metric", default=os.environ.get("EVAL_METRIC", "eval_loss"))
    p.add_argument("--eval-direction", default=os.environ.get("EVAL_DIRECTION", "minimize"))
    p.add_argument("--autoresearch-project", default=os.environ.get("AUTORESEARCH_PROJECT", ""))
    p.add_argument("--autoresearch-round", default=os.environ.get("AUTORESEARCH_ROUND", "0"))
    p.add_argument("--autoresearch-run-id", default=os.environ.get("AUTORESEARCH_RUN_ID", ""))
    # kfp v2 passes the metrics output path on the command line.
    # When set we emit the metrics file kfp expects so values
    # show up in OpenShift AI's Experiments UI as sortable columns.
    p.add_argument("--metrics-output-path", default=os.environ.get("METRICS_OUTPUT_PATH"))
    return p.parse_args()


def _emit_kfp_metrics(path: str, metrics: dict) -> None:
    """Write a kfp v2 Metrics artifact at the given path. kfp's
    SDK expects the path to be a JSON file matching the
    system.Metrics schema. If the path is None (e.g. raw-Job
    run, no kfp context) this is a no-op."""
    if not path:
        return
    try:
        # kfp v2 metrics file: { "metrics": [ { "name": ..., "numberValue": ... }, ... ] }
        # The runtime writes to the path as a file directly.
        out = {"metrics": [{"name": k, "numberValue": float(v)} for k, v in metrics.items()]}
        p = pathlib.Path(path)
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(json.dumps(out))
        print(f"[autoresearch] wrote kfp metrics to {path}: {out}", flush=True)
    except Exception as e:
        # Non-fatal: stdout AUTORESEARCH_RESULT line is the
        # authoritative signal for the operator; kfp metrics are
        # for the Experiments UI bonus.
        print(f"[autoresearch] WARN failed to write kfp metrics: {e}", flush=True)


def emit_result(payload: dict) -> None:
    """Print exactly one delimiter line so the operator can parse
    it deterministically out of pod logs."""
    print(RESULT_PREFIX + json.dumps(payload, separators=(",", ":")), flush=True)


def emit_progress(msg: str) -> None:
    """Human-readable progress prints, prefixed for greppability."""
    print(f"[autoresearch] {msg}", flush=True)


def main() -> int:
    started = time.time()
    cli = _parse_args()
    # Save the metrics output path before TrainingArguments
    # shadows any name we might keep cli args under. Past bug:
    # `args` was reused for both the CLI namespace and the
    # HF TrainingArguments, so the final _emit_kfp_metrics
    # crashed with AttributeError after a successful training
    # run — losing the AUTORESEARCH_RESULT= line the operator
    # depends on. We now hold all cli values in distinct locals.
    metrics_output_path = cli.metrics_output_path
    project = cli.autoresearch_project or "(unknown)"
    run_id = cli.autoresearch_run_id or "(unknown)"
    round_num = cli.autoresearch_round

    emit_progress(f"start project={project} round={round_num} run_id={run_id}")

    try:
        cfg = json.loads(cli.qlora_config_json)
    except json.JSONDecodeError as e:
        emit_result({"status": "error", "error": f"qlora-config-json not valid JSON: {e}"})
        return 1

    base_model = cli.base_model
    if not base_model:
        emit_result({"status": "error", "error": "--base-model (or BASE_MODEL env) not set"})
        return 1
    base_revision = cli.base_model_revision
    training_data = cli.training_data
    if not training_data:
        emit_result({"status": "error", "error": "--training-data (or TRAINING_DATA env) not set"})
        return 1
    training_split = cli.training_split
    sample_count = cli.training_sample_count

    emit_progress(f"config: {json.dumps(cfg)}")
    emit_progress(f"base_model={base_model}@{base_revision}")
    emit_progress(f"training_data={training_data} split={training_split} cap={sample_count}")

    # Lazy imports — keep import-time errors visible if the trainer
    # image lacks a dependency, rather than failing during model
    # download.
    try:
        import torch
        from datasets import load_dataset
        from transformers import (
            AutoModelForCausalLM,
            AutoTokenizer,
            BitsAndBytesConfig,
        )
        from peft import LoraConfig, get_peft_model, prepare_model_for_kbit_training
        # SFTConfig (a TrainingArguments subclass) is the trl 0.12+
        # way to pass SFT-specific knobs (dataset_text_field,
        # max_seq_length, packing). Using SFTConfig keeps everything
        # in one config object and avoids version-drift bugs where
        # newer trl moves fields off TrainingArguments.
        from trl import SFTTrainer, SFTConfig
    except Exception as e:
        emit_result({"status": "error", "stage": "imports", "error": str(e), "trace": traceback.format_exc()})
        return 1

    try:
        emit_progress("loading tokenizer + 4-bit base model")
        bnb = BitsAndBytesConfig(
            load_in_4bit=True,
            bnb_4bit_quant_type="nf4",
            bnb_4bit_compute_dtype=torch.bfloat16,
            bnb_4bit_use_double_quant=True,
        )

        tokenizer = AutoTokenizer.from_pretrained(base_model, revision=base_revision)
        if tokenizer.pad_token is None:
            tokenizer.pad_token = tokenizer.eos_token

        model = AutoModelForCausalLM.from_pretrained(
            base_model,
            revision=base_revision,
            quantization_config=bnb,
            device_map="auto",
            torch_dtype=torch.bfloat16,
        )
        model = prepare_model_for_kbit_training(model)

        emit_progress("attaching LoRA adapters")
        lora_cfg = LoraConfig(
            r=int(cfg.get("lora_rank", 8)),
            lora_alpha=int(cfg.get("lora_alpha", 16)),
            lora_dropout=float(cfg.get("lora_dropout", 0.05)),
            target_modules=cfg.get("target_modules", ["q_proj", "v_proj"]),
            bias="none",
            task_type="CAUSAL_LM",
        )
        model = get_peft_model(model, lora_cfg)
        model.print_trainable_parameters()
    except Exception as e:
        emit_result({"status": "error", "stage": "model_load", "error": str(e), "trace": traceback.format_exc()})
        return 1

    try:
        emit_progress("loading + truncating dataset")
        ds = load_dataset(training_data, split=training_split)
        if sample_count and len(ds) > sample_count:
            ds = ds.shuffle(seed=42).select(range(sample_count))

        # Normalize whatever column shape the dataset has into a single
        # "text" column SFTTrainer can consume. Public instruction
        # datasets disagree on column names; rather than make the user
        # specify in the CR for every dataset, we autodetect common
        # shapes here. Add more cases as we onboard more datasets.
        cols = set(ds.column_names)
        if "text" in cols:
            # tatsu-lab/alpaca, openbmb/UltraFeedback (text variant)
            emit_progress(f"dataset shape: 'text' column present, using as-is. columns={sorted(cols)}")
        elif {"problem", "solution"}.issubset(cols):
            # ise-uiuc/Magicoder-OSS-Instruct-75K and friends
            lang_in_cols = "lang" in cols
            def _fmt_problem_solution(ex):
                lang = ex.get("lang", "") if lang_in_cols else ""
                fence = f"```{lang}\n" if lang else "```\n"
                return {"text": f"{ex['problem']}\n\n{fence}{ex['solution']}\n```"}
            ds = ds.map(_fmt_problem_solution)
            emit_progress(f"dataset shape: formatted from problem+solution into 'text'. columns={sorted(cols)}")
        elif {"instruction", "output"}.issubset(cols):
            # tatsu-lab/alpaca-cleaned, sahil2801/CodeAlpaca-20k, etc.
            has_input = "input" in cols
            def _fmt_inst_resp(ex):
                inp = ex.get("input", "") if has_input else ""
                instr = ex["instruction"]
                prompt = (f"### Instruction:\n{instr}\n\n### Input:\n{inp}\n\n### Response:\n"
                          if inp else
                          f"### Instruction:\n{instr}\n\n### Response:\n")
                return {"text": prompt + ex["output"]}
            ds = ds.map(_fmt_inst_resp)
            emit_progress(f"dataset shape: formatted from instruction+output into 'text'. columns={sorted(cols)}")
        else:
            # Unknown shape — fail loudly with the column list so the
            # user can either pick a different dataset or extend this
            # autodetector. Better than silent KeyError inside trl.
            raise ValueError(
                f"Dataset {training_data!r} has columns {sorted(cols)}; "
                f"autoresearch trainer can autoformat 'text', "
                f"'problem+solution', or 'instruction+output' shapes. "
                f"Add a case in run.py for this dataset's shape."
            )

        # ~10% held out for eval
        split = ds.train_test_split(test_size=0.1, seed=42)
        train_ds, eval_ds = split["train"], split["test"]
        emit_progress(f"train={len(train_ds)} eval={len(eval_ds)}")
    except Exception as e:
        emit_result({"status": "error", "stage": "dataset", "error": str(e), "trace": traceback.format_exc()})
        return 1

    try:
        emit_progress("starting fine-tune")
        output_dir = f"/workspace/output/{run_id}"
        os.makedirs(output_dir, exist_ok=True)
        # SFTConfig (TrainingArguments subclass) holds both the
        # HF + trl-specific knobs. dataset_text_field="text"
        # matches tatsu-lab/alpaca's pre-formatted column;
        # max_seq_length passed here, not as a SFTTrainer kwarg.
        sft_args = SFTConfig(
            output_dir=output_dir,
            per_device_train_batch_size=int(cfg.get("per_device_batch_size", 4)),
            gradient_accumulation_steps=int(cfg.get("gradient_accumulation_steps", 4)),
            num_train_epochs=1,
            max_steps=int(cfg.get("num_training_steps", 200)),
            learning_rate=float(cfg.get("learning_rate", 2e-4)),
            warmup_steps=int(cfg.get("warmup_steps", 20)),
            weight_decay=float(cfg.get("weight_decay", 0.0)),
            logging_steps=20,
            save_strategy="no",  # adapter saved at end via .save_pretrained
            eval_strategy="no",  # we do one final eval below; eval_strategy="steps" hurts speed
            bf16=True,
            gradient_checkpointing=True,
            report_to="none",
            dataloader_pin_memory=False,
            dataset_text_field="text",
            max_seq_length=int(cfg.get("max_seq_length", 1024)),
            packing=False,
        )
        trainer = SFTTrainer(
            model=model,
            args=sft_args,
            train_dataset=train_ds,
            eval_dataset=eval_ds,
            processing_class=tokenizer,
        )
        trainer.train()
        emit_progress("training finished; running final eval")
        # NB: pass no eval_dataset arg here — SFTTrainer's __init__
        # tokenizes the eval_dataset we gave it into input_ids /
        # attention_mask / labels columns. Re-passing the RAW eval_ds
        # bypasses that tokenization and the base Trainer evaluate()
        # path then raises "No columns in the dataset match the
        # model's forward method signature" (v0.0.9 failure mode).
        eval_result = trainer.evaluate()
        eval_loss = float(eval_result.get("eval_loss", float("nan")))
        emit_progress(f"eval_loss={eval_loss}")

        # Save adapter — kept variants get this artifact copied
        # somewhere durable by the operator/pipeline. v0.0.1
        # writes to PVC; v0.0.2 will push to MinIO via DSP.
        adapter_path = f"{output_dir}/adapter"
        trainer.model.save_pretrained(adapter_path)
        emit_progress(f"adapter saved to {adapter_path}")
    except Exception as e:
        emit_result({"status": "error", "stage": "train", "error": str(e), "trace": traceback.format_exc()})
        return 1

    elapsed = time.time() - started

    # Emit kfp metrics so the value shows in OpenShift AI's
    # Experiments UI columns. Multi-dimensional metrics help the
    # UI's sort + filter — we expose the QLoRA config dimensions
    # alongside the eval_loss so users can correlate "rank 16
    # variants tended to win" without reading every JSON.
    _emit_kfp_metrics(metrics_output_path, {
        "eval_loss": eval_loss,
        "elapsed_seconds": elapsed,
        "lora_rank": cfg.get("lora_rank", 0),
        "lora_alpha": cfg.get("lora_alpha", 0),
        "learning_rate": cfg.get("learning_rate", 0),
        "training_steps": cfg.get("num_training_steps", 0),
    })

    emit_result({
        "status": "ok",
        "project": project,
        "round": int(round_num),
        "run_id": run_id,
        "eval_loss": eval_loss,
        "adapter_path": adapter_path,
        "elapsed_seconds": round(elapsed, 1),
        "config": cfg,
    })
    return 0


if __name__ == "__main__":
    sys.exit(main())
