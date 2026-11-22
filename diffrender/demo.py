"""End-to-end acceptance demonstration including fault and error chains."""

from __future__ import annotations

import json
import os
import shutil
from dataclasses import replace
from pathlib import Path
from typing import Any, Callable

import torch

from .blocks import BlockLedger, BlockRecord, make_full_image_specs
from .compare import assert_reports_equal, compare_reports
from .errors import AppError
from .generator import generate_dataset
from .mode_compare import compare_downsample_modes
from .optimize import OptimizationRunner, RunConfig
from .paths import run_three_paths
from .renderer import RenderConfig
from .schema import Dataset, load_dataset
from .serialization import tensor_content_hash


class OneShotFault:
    def __init__(
        self,
        *,
        iteration: int,
        sequence: int,
        stage: str,
        code: str = "E_BLOCK",
        message: str = "injected transient block failure",
    ) -> None:
        self.iteration = iteration
        self.sequence = sequence
        self.stage = stage
        self.code = code
        self.message = message
        self.fired = 0

    def __call__(self, stage: str, iteration: int, spec: Any, attempt: int) -> None:
        if (
            self.fired == 0
            and stage == self.stage
            and iteration == self.iteration
            and spec.sequence == self.sequence
        ):
            self.fired += 1
            raise AppError(
                self.code,
                self.message,
                "fault_injection",
                {
                    "iteration": iteration,
                    "block_sequence": spec.sequence,
                    "block_id": spec.block_id,
                    "stage": stage,
                    "attempt": attempt,
                },
            )


def _capture(fn: Callable[[], Any]) -> dict[str, Any] | None:
    try:
        fn()
        return None
    except AppError as exc:
        return exc.to_dict()


def _write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def _success_metrics(result: dict[str, Any]) -> dict[str, Any]:
    report = result["realtime"]
    loss_start = report["loss_history"][0]
    loss_end = report["loss_history"][-1]
    return {
        "comparisons": {
            key: value["equal"] for key, value in result["comparisons"].items()
        },
        "strategy": report["strategy"],
        "loss_start": loss_start,
        "loss_end": loss_end,
        "loss_reduction_ratio": 1.0 - loss_end / loss_start,
        "final_loss": report["final_loss"],
        "iterations": report["iterations_completed"],
        "parameter_version": report["parameter_version"],
        "block_submissions": len(report["submission_sequence"]),
        "diagnostics": result["diagnostics"],
        "success": (
            all(value["equal"] for value in result["comparisons"].values())
            and loss_end < loss_start
        ),
    }


def run_demo(
    dataset_dir: str | os.PathLike[str],
    output_dir: str | os.PathLike[str],
    *,
    seed: int = 20261123,
    iterations: int = 20,
) -> dict[str, Any]:
    torch.set_num_threads(1)
    dataset_root = Path(dataset_dir).resolve()
    if not (dataset_root / "manifest.json").is_file():
        generate_dataset(dataset_root, seed=seed, force=False)
    dataset = load_dataset(dataset_root)
    root = Path(output_dir).resolve()
    root.mkdir(parents=True, exist_ok=True)

    config = RunConfig(
        iterations=iterations,
        learning_rate=0.012,
        learning_rate_min=2.0e-4,
        lr_schedule="cosine",
        mode="auto",
        checkpoint_every=2,
    )
    summary: dict[str, Any] = {"ok": True, "dataset": str(dataset_root), "stages": {}}

    single = dataset.task("single")
    single_paths = run_three_paths(
        single, config, root / "single_three_paths"
    )
    single_metrics = _success_metrics(single_paths)
    summary["stages"]["single_reconstruction"] = single_metrics

    multi = dataset.task("multi")
    multi_paths = run_three_paths(multi, config, root / "multi_three_paths")
    multi_metrics = _success_metrics(multi_paths)
    summary["stages"]["multi_reconstruction"] = multi_metrics

    summary["stages"]["downsample_monotonicity"] = {
        "single": compare_downsample_modes(single),
        "multi": compare_downsample_modes(multi),
    }
    summary["stages"]["downsample_monotonicity"]["success"] = all(
        summary["stages"]["downsample_monotonicity"][group_id][
            "monotonic_non_decreasing"
        ]
        for group_id in ("single", "multi")
    )

    guard_config = RunConfig(
        iterations=4,
        learning_rate=0.006,
        mode="exact",
        checkpoint_every=2,
        failure_patience=2,
        explode_threshold=100.0,
        clip_enabled=True,
        clip_norm=5.0,
    )
    gradient_dir = root / "gradient_failure" / "checkpoints"
    pre_failure = OptimizationRunner(
        single, guard_config, checkpoint_dir=gradient_dir
    )
    pre_failure.run_to(2)
    before = pre_failure.report().final_parameters
    failing_runner = OptimizationRunner(
        single,
        guard_config,
        checkpoint_dir=gradient_dir,
        gradient_spikes={
            2: {"sphere_0.radius": 1.0e18},
            3: {"sphere_0.radius": 1.0e18},
        },
    )
    load_info = failing_runner.load_checkpoint()
    gradient_error = _capture(lambda: failing_runner.run_to(4))
    after_failure = failing_runner.parameter_errors()
    after_values = {
        label: values["estimate"] for label, values in after_failure.items()
    }
    failed_label = "sphere_0.radius"
    frozen_unchanged = after_values[failed_label] == before[failed_label]
    other_changed = after_values["sphere_0.center.x"] != before["sphere_0.center.x"]
    no_fault_at_failure = OptimizationRunner(
        single, guard_config, checkpoint_dir=gradient_dir
    )
    no_fault_at_failure.load_checkpoint()
    no_fault_at_failure.checkpoint_store = None
    no_fault_at_failure.run_to(3)
    failing_round_gradients = failing_runner.guard.history[2]["parameter_norms"]
    no_fault_round_gradients = no_fault_at_failure.guard.history[2][
        "parameter_norms"
    ]
    other_parameters_isolated = all(
        failing_round_gradients[label] == no_fault_round_gradients[label]
        for label in failing_round_gradients
        if label != failed_label
    ) and sorted(failing_runner.guard.frozen) == [failed_label]
    recovery_runner = OptimizationRunner(
        single, guard_config, checkpoint_dir=gradient_dir
    )
    recovery_load = recovery_runner.load_checkpoint()
    recovery_runner.run_to(4)
    recovered_report = recovery_runner.report().to_dict()
    reference_runner = OptimizationRunner(single, guard_config)
    reference_runner.run_to(4)
    reference_report = reference_runner.report().to_dict()
    recovery_comparison = assert_reports_equal(
        reference_report,
        recovered_report,
        compare_gradient_history=True,
        label="gradient recovery",
    )
    summary["stages"]["gradient_failure_recovery"] = {
        "error": gradient_error,
        "checkpoint_load": load_info,
        "frozen_parameters": sorted(failing_runner.guard.frozen),
        "failed_parameter_unchanged": frozen_unchanged,
        "other_parameter_updated": other_changed,
        "other_gradients_match_no_fault_injection_round": other_parameters_isolated,
        "recovery_checkpoint_load": recovery_load,
        "recovery_matches_uninterrupted_reference": recovery_comparison["equal"],
        "success": (
            gradient_error is not None
            and gradient_error["code"] == "E_GRAD"
            and frozen_unchanged
            and other_changed
            and other_parameters_isolated
            and recovery_comparison["equal"]
        ),
    }

    tiled_config = RunConfig(
        iterations=2,
        learning_rate=0.006,
        mode="tiled",
        tile_size=16,
        max_parallel_blocks=2,
        memory_budget_mb=64.0,
        checkpoint_every=0,
        max_block_retries=1,
    )
    no_fault_runner = OptimizationRunner(single, tiled_config)
    no_fault_runner.run_to(2)
    no_fault_report = no_fault_runner.report().to_dict()
    transient = OneShotFault(iteration=0, sequence=1, stage="backward")
    fault_runner = OptimizationRunner(
        single, tiled_config, fault_function=transient
    )
    fault_runner.run_to(2)
    fault_report = fault_runner.report().to_dict()
    fault_comparison = compare_reports(
        no_fault_report,
        fault_report,
        compare_gradient_history=True,
    )
    permanent_fault = OneShotFault(
        iteration=0,
        sequence=0,
        stage="forward",
        message="injected permanent block failure",
    )
    permanent_runner = OptimizationRunner(
        single,
        replace(tiled_config, iterations=1, max_block_retries=0),
        fault_function=permanent_fault,
    )
    permanent_error = _capture(lambda: permanent_runner.run_to(1))
    summary["stages"]["block_exception_recovery"] = {
        "transient_fault": {
            "fired": transient.fired,
            "retries": fault_report["block_statistics"]["retries"],
            "failed_attempts": fault_report["block_statistics"]["failed_attempts"],
            "matches_no_fault_run": fault_comparison["equal"],
            "first_difference": fault_comparison["first_difference"],
            "success": (
                transient.fired == 1
                and fault_report["block_statistics"]["retries"] >= 1
                and fault_report["block_statistics"]["failed_attempts"] >= 1
                and fault_comparison["equal"]
            ),
        },
        "permanent_fault_error": permanent_error,
        "success": (
            transient.fired == 1
            and fault_report["block_statistics"]["retries"] >= 1
            and fault_comparison["equal"]
            and permanent_error is not None
            and permanent_error["code"] == "E_BLOCK"
        ),
    }

    corrupt_root = root / "corrupt_dataset"
    shutil.copytree(dataset_root, corrupt_root, dirs_exist_ok=True)
    corrupt_target = corrupt_root / multi.targets[0].path
    corrupt_target.write_bytes(b"this is not a numpy target image")
    corrupt_dataset = load_dataset(corrupt_root)
    decode_error = _capture(
        lambda: OptimizationRunner(
            corrupt_dataset.task("multi"),
            RunConfig(iterations=1, checkpoint_every=0),
        )
    )

    schema_root = root / "schema_corrupt_dataset"
    shutil.copytree(dataset_root, schema_root, dirs_exist_ok=True)
    schema_scene_path = schema_root / "single" / "scene.json"
    schema_scene = json.loads(schema_scene_path.read_text(encoding="utf-8"))
    del schema_scene["cameras"][0]["intrinsics"]
    schema_scene_path.write_text(
        json.dumps(schema_scene, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    schema_dataset = load_dataset(schema_root)
    schema_error = _capture(lambda: schema_dataset.task("single"))

    parameter_error = _capture(lambda: RenderConfig(shadow_samples=0).validate())

    state_ledger = BlockLedger()
    state_specs = make_full_image_specs(
        [
            (0, "a", "cam_front", 4, 4),
            (1, "b", "cam_left", 4, 4),
        ],
        base_seed=0,
    )
    snapshot_hash = tensor_content_hash(torch.zeros(3, dtype=torch.float64))
    state_ledger.begin_round(0, state_specs, snapshot_hash, 0)
    illegal_record = BlockRecord(
        round_index=0,
        sequence=1,
        block_id=state_specs[1].block_id,
        status="committed",
        snapshot_hash=snapshot_hash,
        parameter_version=0,
        shadow_seed=state_specs[1].shadow_seed,
        attempts=1,
        loss=0.0,
        gradient_norm=0.0,
        duration_ms=0.0,
    )
    state_error = _capture(
        lambda: state_ledger.submit(
            illegal_record, torch.zeros(3, dtype=torch.float64)
        )
    )
    state_preserved = (
        state_ledger.rounds[0].committed_count == 0
        and not state_ledger.rounds[0].records
    )
    summary["stages"]["error_code_chain"] = {
        "decode": decode_error,
        "schema": schema_error,
        "parameter": parameter_error,
        "state": state_error,
        "illegal_submit_state_preserved": state_preserved,
        "success": (
            decode_error is not None
            and decode_error["code"] == "E_DECODE"
            and schema_error is not None
            and schema_error["code"] == "E_SCHEMA"
            and parameter_error is not None
            and parameter_error["code"] == "E_PARA"
            and state_error is not None
            and state_error["code"] == "E_STATE"
            and state_preserved
        ),
    }

    summary["ok"] = all(
        bool(stage.get("success", True))
        for stage in summary["stages"].values()
    )
    summary["ok"] = summary["ok"] and all(
        stage.get("monotonic_non_decreasing", True)
        for stage in (
            summary["stages"]["downsample_monotonicity"]["single"],
            summary["stages"]["downsample_monotonicity"]["multi"],
        )
    )
    _write_json(root / "demo_summary.json", summary)
    return summary

