"""Programmatic quickstart for one deterministic inverse-rendering run."""

from __future__ import annotations

import json
import sys
from pathlib import Path

import torch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from diffrender.diagnostics import export_diagnostics
from diffrender.generator import generate_dataset
from diffrender.optimize import OptimizationRunner, RunConfig
from diffrender.schema import load_dataset


def main() -> int:
    torch.set_num_threads(1)
    dataset_root = Path("data")
    if not (dataset_root / "manifest.json").is_file():
        generate_dataset(dataset_root, seed=20261123, force=False)
    dataset = load_dataset(dataset_root)
    task = dataset.task("single")
    runner = OptimizationRunner(
        task,
        RunConfig(
            iterations=20,
            learning_rate=0.012,
            mode="auto",
            checkpoint_every=2,
        ),
        checkpoint_dir=Path("artifacts/quickstart/checkpoints"),
    )
    runner.run_to(20)
    report = runner.report()
    diagnostics = export_diagnostics(runner, "artifacts/quickstart/diagnostics")
    print(
        json.dumps(
            {
                "task_id": task.scene.scene_id,
                "strategy": report.strategy,
                "loss_start": report.loss_history[0],
                "loss_end": report.loss_history[-1],
                "comparison": diagnostics["comparison"],
            },
            ensure_ascii=False,
            indent=2,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

