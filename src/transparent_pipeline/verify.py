"""Analytic-truth, convergence, and three-path consistency verification."""

from __future__ import annotations

import math
import shutil
from pathlib import Path
from typing import Any

import numpy as np

from .comparison import compare_results
from .engine import RenderEngine, SimulatedCrash
from .errors import E_DIVERGE, E_STATE, PipelineError
from .renderer import RenderConfig, trace_ray
from .resources import ResourceLimits
from .scene import Scene
from .util import write_json_atomic


def verify_truth(scene: Scene, truth: dict[str, Any], config: RenderConfig | None = None) -> dict[str, Any]:
    config = config or RenderConfig()
    failures: list[dict[str, Any]] = []
    boundary_errors: list[float] = []
    path_errors: list[float] = []
    transmittance_errors: list[float] = []
    scattering_errors: list[float] = []

    for case in truth["boundary_exit"]:
        result = trace_ray(
            scene,
            np.asarray(case["origin"], dtype=np.float64),
            np.asarray(case["direction"], dtype=np.float64),
            int(case["wavelength_index"]),
            config,
        )
        expected_exit = np.asarray(case["expected_exit"], dtype=np.float64)
        if not result["hit"] or result["exit_point"] is None:
            failures.append({"case": case["id"], "field": "exit_point", "reason": "ray did not hit target medium"})
            continue
        exit_error = float(np.linalg.norm(result["exit_point"] - expected_exit))
        path_error = abs(float(result["path_length_quantized"]) - float(case["expected_path_length"]))
        boundary_errors.append(exit_error)
        path_errors.append(path_error)
        if exit_error > float(case["tolerance"]):
            failures.append(
                {
                    "case": case["id"],
                    "field": "exit_point",
                    "error": exit_error,
                    "tolerance": case["tolerance"],
                    "actual": result["exit_point"].tolist(),
                    "expected": expected_exit.tolist(),
                }
            )
        if path_error > max(float(case["tolerance"]), 1.0e-8):
            failures.append(
                {
                    "case": case["id"],
                    "field": "path_length",
                    "error": path_error,
                    "tolerance": max(float(case["tolerance"]), 1.0e-8),
                    "actual": result["path_length_quantized"],
                    "expected": case["expected_path_length"],
                }
            )

    for case in truth["ray_transmittance"]:
        result = trace_ray(
            scene,
            np.asarray(case["origin"], dtype=np.float64),
            np.asarray(case["direction"], dtype=np.float64),
            int(case["wavelength_index"]),
            config,
        )
        error = abs(float(result["transmission"]) - float(case["expected_transmittance"]))
        transmittance_errors.append(error)
        if error > float(case["tolerance"]):
            failures.append(
                {
                    "case": case["id"],
                    "field": "transmittance",
                    "error": error,
                    "tolerance": case["tolerance"],
                    "actual": result["transmission"],
                    "expected": case["expected_transmittance"],
                }
            )

    for case in truth["ray_scattering"]:
        result = trace_ray(
            scene,
            np.asarray(case["origin"], dtype=np.float64),
            np.asarray(case["direction"], dtype=np.float64),
            int(case["wavelength_index"]),
            config,
        )
        contribution = result["scattering_by_light"].get(case["light_id"], 0.0)
        error = abs(float(contribution) - float(case["expected_integral"]))
        scattering_errors.append(error)
        if error > float(case["tolerance"]):
            failures.append(
                {
                    "case": case["id"],
                    "field": "scattering_integral",
                    "error": error,
                    "tolerance": case["tolerance"],
                    "actual": contribution,
                    "expected": case["expected_integral"],
                }
            )

    deterministic = False
    if truth["boundary_exit"]:
        case = truth["boundary_exit"][0]
        arguments = (
            scene,
            np.asarray(case["origin"], dtype=np.float64),
            np.asarray(case["direction"], dtype=np.float64),
            int(case["wavelength_index"]),
            config,
        )
        first = trace_ray(*arguments)
        second = trace_ray(*arguments)
        deterministic = (
            first["exit_point"].tobytes() == second["exit_point"].tobytes()
            and first["path_units"] == second["path_units"]
            and first["optical_units"] == second["optical_units"]
            and float(first["transmission"]).hex() == float(second["transmission"]).hex()
            and float(first["scattering"]).hex() == float(second["scattering"]).hex()
        )
        if not deterministic:
            failures.append({"case": case["id"], "field": "determinism", "reason": "repeated trace differed"})

    convergence: dict[str, Any] = {"ok": False, "steps": [], "errors": []}
    scattering_case = next(
        (case for case in truth["ray_scattering"] if float(case["expected_integral"]) > 1.0e-6),
        None,
    )
    if scattering_case is not None:
        steps = [0.2, 0.1, 0.05, 0.025, 0.0125]
        errors: list[float] = []
        for step in steps:
            result = trace_ray(
                scene,
                np.asarray(scattering_case["origin"], dtype=np.float64),
                np.asarray(scattering_case["direction"], dtype=np.float64),
                int(scattering_case["wavelength_index"]),
                RenderConfig(
                    block_width=config.block_width,
                    block_height=config.block_height,
                    step_size=step,
                    sampling_strategy=config.sampling_strategy,
                    max_segments=config.max_segments,
                ),
            )
            contribution = result["scattering_by_light"].get(scattering_case["light_id"], 0.0)
            errors.append(abs(float(contribution) - float(scattering_case["expected_integral"])))
        monotonic = all(
            errors[index] <= errors[index - 1] + 1.0e-15
            for index in range(1, len(errors))
        )
        convergence = {
            "ok": monotonic and errors[-1] <= float(scattering_case["tolerance"]),
            "case": scattering_case["id"],
            "steps": steps,
            "errors": errors,
            "monotonic": monotonic,
            "final_within_tolerance": errors[-1] <= float(scattering_case["tolerance"]),
            "tolerance": scattering_case["tolerance"],
        }
        if not convergence["ok"]:
            failures.append({"case": scattering_case["id"], "field": "step_convergence", "details": convergence})

    return {
        "ok": not failures,
        "failures": failures,
        "counts": {
            "boundary_exit": len(truth["boundary_exit"]),
            "ray_transmittance": len(truth["ray_transmittance"]),
            "ray_scattering": len(truth["ray_scattering"]),
        },
        "max_errors": {
            "boundary_exit": max(boundary_errors, default=0.0),
            "path_length": max(path_errors, default=0.0),
            "transmittance": max(transmittance_errors, default=0.0),
            "scattering": max(scattering_errors, default=0.0),
        },
        "deterministic_repeat": deterministic,
        "convergence": convergence,
        "config": config.to_dict(),
    }


def verify_three_paths(
    scene: Scene,
    config: RenderConfig | None = None,
    *,
    work_directory: str | Path = "runtime/verification",
    sample_index: int = 0,
    crash_after_commits: int | None = None,
    limits: ResourceLimits | None = None,
) -> dict[str, Any]:
    config = config or RenderConfig()
    work_directory = Path(work_directory)
    work_directory.mkdir(parents=True, exist_ok=True)
    paths = {name: work_directory / name for name in ("full", "preview", "resume")}
    for path in paths.values():
        if path.exists():
            shutil.rmtree(path)
        path.mkdir(parents=True)

    full_engine = RenderEngine(
        scene,
        config,
        checkpoint_directory=paths["full"],
        limits=limits,
        sample_index=sample_index,
    )
    full = full_engine.render(mode="full")

    preview_engine = RenderEngine(
        scene,
        config,
        checkpoint_directory=paths["preview"],
        limits=limits,
        sample_index=sample_index,
    )
    preview = preview_engine.render(mode="preview")

    total_blocks = len(full.block_windows)
    crash_at = crash_after_commits if crash_after_commits is not None else max(1, total_blocks // 2)
    resume_engine = RenderEngine(
        scene,
        config,
        checkpoint_directory=paths["resume"],
        limits=limits,
        sample_index=sample_index,
    )
    crashed = False
    try:
        resume_engine.render(mode="full", crash_after_commits=crash_at)
    except SimulatedCrash:
        crashed = True
    committed_before_resume = resume_engine.progress()["committed"]
    resumed_engine = RenderEngine(
        scene,
        config,
        checkpoint_directory=paths["resume"],
        limits=limits,
        sample_index=sample_index,
    )
    resumed = resumed_engine.render(mode="resume")

    comparisons = {
        "full_vs_preview": compare_results(preview, full).to_dict(),
        "full_vs_resume": compare_results(resumed, full).to_dict(),
    }
    divergence = next(
        (
            result
            for result in comparisons.values()
            if not result["equal"]
        ),
        None,
    )
    complete = full.complete and preview.complete and resumed.complete
    report: dict[str, Any] = {
        "ok": complete and divergence is None and crashed,
        "code": None if complete and divergence is None else (E_DIVERGE if divergence else E_STATE),
        "crashed_as_requested": crashed,
        "committed_before_resume": committed_before_resume,
        "total_blocks": total_blocks,
        "comparisons": comparisons,
        "paths": {
            "full": _path_summary(full),
            "preview": _path_summary(preview),
            "resume": _path_summary(resumed),
        },
        "config": config.to_dict(),
        "scene_hash": scene.content_hash,
        "resume_decision": resumed.resume,
    }
    if divergence is not None:
        report["error"] = divergence["error"]
        report["actual"] = divergence.get("actual")
        report["expected"] = divergence.get("expected")
    elif not complete:
        report["error"] = {
            "code": E_STATE,
            "field": "progress",
            "message": "one or more verification paths did not commit every block",
        }
    write_json_atomic(work_directory / "verification.json", report)
    return report


def _path_summary(outcome: Any) -> dict[str, Any]:
    return {
        "complete": outcome.complete,
        "progress": outcome.progress,
        "commit_order": outcome.commit_order,
        "actual_step": outcome.actual_step,
        "preview_error_bound": outcome.preview_error_bound,
        "stats": outcome.stats,
        "statuses": {str(key): value for key, value in outcome.statuses.items()},
    }


__all__ = ["verify_three_paths", "verify_truth"]
