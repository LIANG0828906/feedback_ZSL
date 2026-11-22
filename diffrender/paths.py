"""Realtime, resumed and one-shot execution paths with strict comparison."""

from __future__ import annotations

import json
import os
from dataclasses import replace
from pathlib import Path
from typing import Any

from .compare import assert_reports_equal
from .diagnostics import export_diagnostics
from .optimize import OptimizationRunner, RunConfig
from .schema import Task


def _run_once(
    task: Task,
    config: RunConfig,
    checkpoint_dir: str | os.PathLike[str] | None,
    target_iterations: int,
) -> OptimizationRunner:
    runner = OptimizationRunner(task, config, checkpoint_dir=checkpoint_dir)
    runner.run_to(target_iterations)
    return runner


def run_three_paths(
    task: Task,
    config: RunConfig,
    output_dir: str | os.PathLike[str],
    *,
    checkpoint_every: int | None = None,
) -> dict[str, Any]:
    config.validate()
    root = Path(output_dir)
    root.mkdir(parents=True, exist_ok=True)
    every = config.checkpoint_every if checkpoint_every is None else checkpoint_every
    if every <= 0:
        every = max(1, config.iterations // 3)
    windowed = replace(config, checkpoint_every=every)
    realtime_dir = root / "realtime_checkpoints"
    resume_dir = root / "resume_checkpoints"

    realtime = _run_once(task, windowed, realtime_dir, config.iterations)
    realtime_report = realtime.report().to_dict()

    split = max(every, ((config.iterations // 2) // every) * every)
    split = min(split, config.iterations - 1) if config.iterations > 1 else 1
    first = _run_once(task, windowed, resume_dir, split)
    resumed_runner = OptimizationRunner(task, windowed, checkpoint_dir=resume_dir)
    load_info = resumed_runner.load_checkpoint()
    if not load_info.get("valid", False):
        from .errors import fail

        fail(
            "E_STATE",
            "resume path could not load its own checkpoint",
            "checkpoint",
            load_info=load_info,
        )
    resumed_runner.run_to(config.iterations)
    resume_report = resumed_runner.report().to_dict()

    oneshot_config = replace(config, checkpoint_every=0)
    oneshot = _run_once(task, oneshot_config, None, config.iterations)
    oneshot_report = oneshot.report().to_dict()

    comparisons = {
        "realtime_vs_resume": assert_reports_equal(
            realtime_report,
            resume_report,
            compare_gradient_history=True,
            label="resume path",
        ),
        "realtime_vs_oneshot": assert_reports_equal(
            realtime_report,
            oneshot_report,
            compare_gradient_history=True,
            label="one-shot path",
        ),
    }
    result = {
        "ok": True,
        "task_id": task.scene.scene_id,
        "iterations": config.iterations,
        "split_iteration": split,
        "checkpoint_every": every,
        "comparisons": comparisons,
        "realtime": realtime_report,
        "resumed": resume_report,
        "oneshot": oneshot_report,
        "resumed_checkpoint_load": load_info,
        "resume_first_runner_checkpoints": len(first.checkpoints),
        "diagnostics": export_diagnostics(
            realtime, root / "realtime_diagnostics"
        ),
    }
    (root / "three_path_report.json").write_text(
        json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    return result

