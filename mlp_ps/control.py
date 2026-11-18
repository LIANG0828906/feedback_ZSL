from __future__ import annotations

import argparse
import sys
import time
from pathlib import Path
from typing import Any, Dict, Optional, Sequence

import torch

from .config import Config, SHARD_COUNT, load_config
from .data import eval_dataset, local_batch, train_dataset
from .determinism import configure_deterministic_runtime
from .errors import BusinessError, E_CONFIG
from .layout import build_layout
from .model import (
    assign_gradients,
    average_gradients,
    build_model,
    build_optimizer,
    evaluate_loss,
    full_gradient_snapshot,
    gradient_norm,
    model_state_by_name,
    parameter_shapes,
    zero_gradients,
)
from .tensor_codec import pack_named_tensors
from .util import atomic_write_bytes, atomic_write_text, pretty_json_text, sha256_file, utc_timestamp


def run_control(config: Config, output_dir: Path) -> Dict[str, Any]:
    configure_deterministic_runtime(config.seed)
    started = time.perf_counter()
    model = build_model(config.seed, config.input_dim, config.hidden_dim, config.output_dim)
    optimizer = build_optimizer(model, config.learning_rate, config.momentum)
    layout = build_layout(parameter_shapes(model))
    features, targets = train_dataset(config)
    eval_features, eval_targets = eval_dataset(config)
    initial_eval_loss = evaluate_loss(model, eval_features, eval_targets)

    for step in range(1, config.total_steps + 1):
        gradients = []
        for shard_id in range(SHARD_COUNT):
            zero_gradients(model)
            batch_features, batch_targets, _, _ = local_batch(
                features,
                targets,
                step - 1,
                shard_id,
                config,
            )
            prediction = model(batch_features)
            loss = torch.mean((prediction - batch_targets) ** 2)
            loss.backward()
            gradients.append(full_gradient_snapshot(model))
        averaged = average_gradients(gradients, list(layout.parameter_names))
        assign_gradients(model, averaged)
        optimizer.step()
        zero_gradients(model)

    output_dir.mkdir(parents=True, exist_ok=True)
    state = model_state_by_name(model)
    segments, payload = pack_named_tensors(list(layout.parameter_names), state)
    params_path = output_dir / "params.bin"
    atomic_write_bytes(params_path, payload)
    index = {
        "format": "mlp-ps-final-v1",
        "global_step": config.total_steps,
        "layout_id": layout.layout_id,
        "layout": layout.spec,
        "parameter_order": list(layout.parameter_names),
        "parameter_shapes": {
            name: list(shape) for name, shape in layout.parameter_shapes.items()
        },
        "segments": segments,
        "consumed_samples": {
            str(shard_id): config.total_steps * config.local_batch_size
            for shard_id in range(SHARD_COUNT)
        },
        "data_bytes": len(payload),
        "sha256": sha256_file(params_path),
    }
    atomic_write_text(output_dir / "index.json", pretty_json_text(index))
    result = {
        "status": "OK",
        "mode": "single_process_control",
        "timestamp_utc": utc_timestamp(),
        "global_step": config.total_steps,
        "layout_id": layout.layout_id,
        "initial_eval_mse": initial_eval_loss,
        "final_eval_mse": evaluate_loss(model, eval_features, eval_targets),
        "final_params_sha256": index["sha256"],
        "elapsed_seconds": round(time.perf_counter() - started, 6),
    }
    atomic_write_text(output_dir / "control_result.json", pretty_json_text(result))
    return result


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = argparse.ArgumentParser(description="Run deterministic single-process control training")
    parser.add_argument("--config", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--run-dir")
    args = parser.parse_args(argv)
    try:
        config = load_config(args.config, run_dir_override=args.run_dir)
        result = run_control(config, Path(args.output).resolve())
        print(pretty_json_text(result), end="")
        return 0
    except BusinessError as exc:
        print(pretty_json_text({"status": "ERROR", **exc.to_dict()}), file=sys.stderr, end="")
        return exc.exit_code


if __name__ == "__main__":
    raise SystemExit(main())
