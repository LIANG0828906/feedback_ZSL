from __future__ import annotations

import argparse
import time
from typing import Any

import numpy as np

from train.common import (
    atomic_write_json,
    load_config,
    load_data_metadata,
    load_manifest_for_config,
    open_shard,
    parameter_names,
    parameter_shape,
    resolve_path,
    step_batch_slice,
)
from train.model import accuracy, loss_and_gradient


def run_reference_training(
    config: dict[str, Any], include_snapshots: bool = False
) -> dict[str, Any]:
    manifest = load_manifest_for_config(config)
    metadata = load_data_metadata(config)
    steps = int(manifest["hyperparameters"]["steps"])
    worker_count = int(manifest["hyperparameters"]["workers"])
    samples_per_worker = int(
        manifest["hyperparameters"]["samples_per_worker_per_step"]
    )
    learning_rate = float(manifest["hyperparameters"]["learning_rate"])
    names = parameter_names(manifest)
    rows, classes = parameter_shape(manifest)
    parameters = np.zeros(len(names), dtype=np.float64)
    shards = [
        open_shard(config, metadata, shard_index)
        for shard_index in range(worker_count)
    ]
    loss_sequence: list[float] = []
    snapshots: list[list[float]] = []
    started = time.perf_counter()
    for step in range(1, steps + 1):
        aggregate = np.zeros_like(parameters)
        total_samples = 0
        weighted_loss = 0.0
        for features, labels in shards:
            sample_count = int(features.shape[0])
            batch = step_batch_slice(step, samples_per_worker, sample_count)
            local_loss, gradient = loss_and_gradient(
                np.asarray(features[batch], dtype=np.float32),
                np.asarray(labels[batch], dtype=np.int64),
                parameters,
                manifest,
            )
            aggregate += gradient * samples_per_worker
            weighted_loss += local_loss * samples_per_worker
            total_samples += samples_per_worker
        aggregate /= float(total_samples)
        step_loss = weighted_loss / float(total_samples)
        parameters = parameters - learning_rate * aggregate
        loss_sequence.append(step_loss)
        if include_snapshots:
            snapshots.append(parameters.copy())
    elapsed = time.perf_counter() - started

    validation_features = np.load(
        resolve_path(config["data"]["directory"])
        / metadata["validation_features_file"],
        mmap_mode="r",
    )
    validation_labels = np.load(
        resolve_path(config["data"]["directory"])
        / metadata["validation_labels_file"],
        mmap_mode="r",
    )
    validation_accuracy = accuracy(
        np.asarray(validation_features, dtype=np.float32),
        np.asarray(validation_labels, dtype=np.int64),
        parameters,
        manifest,
    )
    result = {
        "accuracy": validation_accuracy,
        "elapsed_seconds": round(elapsed, 3),
        "final_parameters": {
            name: float(parameters[index]) for index, name in enumerate(names)
        },
        "loss_sequence": [float(value) for value in loss_sequence],
        "learning_rate": learning_rate,
        "rows": rows,
        "classes": classes,
        "steps": steps,
        "worker_count": worker_count,
        "samples_per_worker_per_step": samples_per_worker,
    }
    if include_snapshots:
        result["parameter_snapshots"] = snapshots
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description="Run deterministic single-process reference")
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--output", default="reports/baseline.json")
    args = parser.parse_args()
    config = load_config(args.config)
    result = run_reference_training(config)
    atomic_write_json(resolve_path(args.output), result)
    print(
        f"reference steps={result['steps']} loss={result['loss_sequence'][-1]:.6f} "
        f"accuracy={result['accuracy']:.6f} elapsed={result['elapsed_seconds']}s",
        flush=True,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
