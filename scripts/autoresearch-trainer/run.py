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

import json
import os
import sys
import time
import traceback

# Result delimiters the operator's parseTrainerResult() reads.
RESULT_PREFIX = "AUTORESEARCH_RESULT="


def emit_result(payload: dict) -> None:
    """Print exactly one delimiter line so the operator can parse
    it deterministically out of pod logs."""
    print(RESULT_PREFIX + json.dumps(payload, separators=(",", ":")), flush=True)


def emit_progress(msg: str) -> None:
    """Human-readable progress prints, prefixed for greppability."""
    print(f"[autoresearch] {msg}", flush=True)


def main() -> int:
    started = time.time()
    project = os.environ.get("AUTORESEARCH_PROJECT", "(unknown)")
    run_id = os.environ.get("AUTORESEARCH_RUN_ID", "(unknown)")
    round_num = os.environ.get("AUTORESEARCH_ROUND", "0")

    emit_progress(f"start project={project} round={round_num} run_id={run_id}")

    try:
        cfg = json.loads(os.environ["AUTORESEARCH_CONFIG"])
    except KeyError:
        emit_result({"status": "error", "error": "AUTORESEARCH_CONFIG not set"})
        return 1
    except json.JSONDecodeError as e:
        emit_result({"status": "error", "error": f"AUTORESEARCH_CONFIG not valid JSON: {e}"})
        return 1

    base_model = os.environ.get("BASE_MODEL")
    if not base_model:
        emit_result({"status": "error", "error": "BASE_MODEL not set"})
        return 1
    base_revision = os.environ.get("BASE_MODEL_REVISION", "main")
    training_data = os.environ.get("TRAINING_DATA")
    if not training_data:
        emit_result({"status": "error", "error": "TRAINING_DATA not set"})
        return 1
    training_split = os.environ.get("TRAINING_SPLIT", "train")
    sample_count = int(os.environ.get("TRAINING_SAMPLE_COUNT", "2000"))

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
            TrainingArguments,
        )
        from peft import LoraConfig, get_peft_model, prepare_model_for_kbit_training
        from trl import SFTTrainer
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
        args = TrainingArguments(
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
        )
        trainer = SFTTrainer(
            model=model,
            args=args,
            train_dataset=train_ds,
            eval_dataset=eval_ds,
            processing_class=tokenizer,
        )
        trainer.train()
        emit_progress("training finished; running final eval")
        eval_result = trainer.evaluate(eval_dataset=eval_ds)
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
