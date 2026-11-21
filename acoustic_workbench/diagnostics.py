"""Scripted end-to-end diagnostics and deterministic replay checks."""

from __future__ import annotations

import shutil
from pathlib import Path
from typing import Any, Dict, List, Mapping, Optional, Sequence

from .codes import AcousticError
from .engine import Workbench
from .samplegen import generate_samples, mutate_scene
from .util import atomic_write_json, read_json, sha256_file
from .wavio import read_wav_16


def _reset(path: Path) -> None:
    if path.exists():
        shutil.rmtree(path)
    path.mkdir(parents=True, exist_ok=True)


def _error_code(callable_) -> Optional[str]:
    try:
        callable_()
    except AcousticError as exc:
        return exc.code
    return None


def _check(
    checks: List[Dict[str, Any]],
    check_id: str,
    ok: bool,
    evidence: Mapping[str, Any],
) -> None:
    checks.append(
        {
            "id": check_id,
            "ok": bool(ok),
            "evidence": dict(evidence),
        }
    )


def run_diagnostics(
    workspace: str | Path,
    *,
    seed: int = 20260101,
) -> Dict[str, Any]:
    root = Path(workspace)
    root.mkdir(parents=True, exist_ok=True)
    samples_root = root / "samples"
    validation_root = root / "validation"
    determinism_root = root / "determinism"
    _reset(samples_root)
    _reset(validation_root)
    _reset(determinism_root)

    samples_manifest = generate_samples(
        samples_root,
        seed=seed,
        build_baselines=True,
    )
    validation_samples = generate_samples(
        validation_root / "source",
        seed=seed,
        venue_names=("small",),
        build_baselines=True,
    )
    scene_dir = validation_root / "source" / "scenes" / "small"
    scene_path = scene_dir / "scene.json"
    workbench = Workbench(scene_dir, scene_path="scene.json")
    checks: List[Dict[str, Any]] = []

    baseline = workbench.rebuild("L1")
    second_listener = workbench.rebuild("L2")
    _check(
        checks,
        "baseline_reconstruction",
        baseline["ok"] and second_listener["ok"],
        {
            "L1_field_version": baseline["field_version"],
            "L2_field_version": second_listener["field_version"],
        },
    )
    initial_trajectory = baseline["trajectory"]
    _check(
        checks,
        "five_one_field",
        baseline["field"]["channel_layout"] == ["FL", "FR", "FC", "LFE", "BL", "BR"],
        {"channel_layout": baseline["field"]["channel_layout"]},
    )

    noop = workbench.rebuild("L1")
    _check(
        checks,
        "noop_all_cache_hit",
        noop["trajectory"]["recomputed_nodes"] == []
        and len(noop["trajectory"]["cache_hits"]) == 7,
        {
            "cache_hits": noop["trajectory"]["cache_hits"],
            "field_version_unchanged": (
                noop["field_version"] == baseline["field_version"]
            ),
        },
    )

    mutate_scene(
        scene_path,
        {"venue.surfaces.wall": "heavy_curtain"},
    )
    material_rebuild = workbench.rebuild("L1")
    material_recomputed = material_rebuild["trajectory"]["recomputed_nodes"]
    _check(
        checks,
        "material_incremental_reuse",
        material_recomputed
        == [
            "decay_model",
            "late_reverb",
            "spatial_synthesis",
            "field_coefficients",
        ]
        and "early_reflections" in material_rebuild["trajectory"]["cache_hits"],
        {
            "recomputed_nodes": material_recomputed,
            "cache_hits": material_rebuild["trajectory"]["cache_hits"],
            "earliest_affected_node": material_rebuild["trajectory"][
                "earliest_affected_node"
            ],
        },
    )

    mutate_scene(scene_path, {"window_params.early_window_ms": 95.0})
    window_rebuild = workbench.rebuild("L1")
    _check(
        checks,
        "window_incremental_with_event_impact",
        window_rebuild["trajectory"]["recomputed_nodes"][0]
        == "early_reflections"
        and window_rebuild["trajectory"]["reflection_event_impact"] is not None,
        {
            "recomputed_nodes": window_rebuild["trajectory"]["recomputed_nodes"],
            "reflection_event_impact": window_rebuild["trajectory"][
                "reflection_event_impact"
            ],
        },
    )

    current_version = window_rebuild["field_version"]
    output_path = workbench.renders / "stale.wav"
    if output_path.exists():
        output_path.unlink()
    mutate_scene(scene_path, {"source.position.0": 2.0})
    stale_code = _error_code(
        lambda: workbench.render("L1", output="renders/stale.wav")
    )
    _check(
        checks,
        "stale_field_rejected",
        stale_code == "E_FIELD_STALE" and not output_path.exists(),
        {
            "code": stale_code,
            "old_field_version": current_version,
            "output_written": output_path.exists(),
        },
    )
    rebuilt_after_stale = workbench.rebuild("L1")
    source_trajectory = rebuilt_after_stale["trajectory"]
    _check(
        checks,
        "source_incremental_reuse",
        source_trajectory["earliest_affected_node"] == "early_reflections"
        and source_trajectory["cache_hits"] == ["ir_load", "direct_alignment"]
        and source_trajectory["recomputed_nodes"][0] == "early_reflections",
        {
            "factors_changed": source_trajectory["factors_changed"],
            "earliest_affected_node": source_trajectory["earliest_affected_node"],
            "cache_hits": source_trajectory["cache_hits"],
            "recomputed_nodes": source_trajectory["recomputed_nodes"],
        },
    )
    render_result = workbench.render("L1", output="renders/L1.wav")
    render_header = read_wav_16(workbench.workspace / "renders" / "L1.wav")
    _check(
        checks,
        "render_after_rebuild",
        render_result["ok"] and render_header.channel_count == 6,
        {
            "field_version": rebuilt_after_stale["field_version"],
            "output_sha256": render_result["output_sha256"],
            "channel_count": render_header.channel_count,
        },
    )

    field_bytes_before = (
        workbench.cache.published_path("L1").read_bytes()
    )
    inject_expectations = {
        "ir_invalid": "E_IR_INVALID",
        "decay_negative": "E_DECAY_NEGATIVE",
        "position_out_of_bounds": "E_POSITION_OUT_OF_BOUNDS",
        "channel_layout_missing": "E_CHANNEL_LAYOUT_MISMATCH",
    }
    injection_results = {}
    for injection, expected in inject_expectations.items():
        code = _error_code(
            lambda injection=injection: workbench.rebuild(
                "L1", injections=[injection]
            )
        )
        injection_results[injection] = {
            "expected_code": expected,
            "actual_code": code,
            "passed": code == expected,
        }
    field_bytes_after = workbench.cache.published_path("L1").read_bytes()
    _check(
        checks,
        "anomaly_injection_business_codes",
        all(item["passed"] for item in injection_results.values())
        and field_bytes_before == field_bytes_after,
        {
            "injections": injection_results,
            "field_unchanged": field_bytes_before == field_bytes_after,
        },
    )

    with workbench.tasks.acquire("L1", "diagnostic-holder"):
        busy_code = _error_code(lambda: workbench.rebuild("L1"))
    _check(
        checks,
        "listener_busy_guard",
        busy_code == "E_BUSY",
        {"code": busy_code, "listener_id": "L1"},
    )

    cancelled_before = workbench.cache.published_path("L1").read_bytes()
    mutate_scene(scene_path, {"window_params.early_window_ms": 105.0})
    cancel_code = _error_code(
        lambda: workbench.rebuild("L1", cancel_after_stage="early_reflections")
    )
    cancelled_after = workbench.cache.published_path("L1").read_bytes()
    _check(
        checks,
        "cooperative_cancel_preserves_commit",
        cancel_code == "E_CANCELLED" and cancelled_before == cancelled_after,
        {
            "code": cancel_code,
            "last_committed_field_preserved": cancelled_before == cancelled_after,
        },
    )
    workbench.rebuild("L1")

    deterministic_results = []
    for label in ("run_a", "run_b"):
        target = determinism_root / label
        generated = generate_samples(
            target,
            seed=seed,
            venue_names=("small",),
            build_baselines=False,
        )
        del generated
        det_scene_dir = target / "scenes" / "small"
        det_scene_path = det_scene_dir / "scene.json"
        bench = Workbench(det_scene_dir, scene_path="scene.json")
        bench.rebuild("L1")
        mutate_scene(
            det_scene_path,
            {"venue.surfaces.wall": "heavy_curtain"},
        )
        field_result = bench.rebuild("L1")
        render = bench.render("L1", output="renders/L1.wav")
        deterministic_results.append(
            {
                "trajectory_sha256": sha256_file(
                    det_scene_dir / "logs" / "trajectory.jsonl"
                ),
                "field_sha256": sha256_file(
                    bench.cache.published_path("L1")
                ),
                "render_sha256": render["output_sha256"],
                "field_version": field_result["field_version"],
            }
        )
    deterministic_ok = (
        deterministic_results[0]["trajectory_sha256"]
        == deterministic_results[1]["trajectory_sha256"]
        and deterministic_results[0]["field_sha256"]
        == deterministic_results[1]["field_sha256"]
        and deterministic_results[0]["render_sha256"]
        == deterministic_results[1]["render_sha256"]
    )
    _check(
        checks,
        "byte_deterministic_replay",
        deterministic_ok,
        {
            "run_a": deterministic_results[0],
            "run_b": deterministic_results[1],
        },
    )

    report = {
        "schema_version": 1,
        "ok": all(check["ok"] for check in checks),
        "seed": seed,
        "checks": checks,
        "artifacts": {
            "samples_manifest": "samples/manifest.json",
            "diagnostic_trajectory": "validation/source/scenes/small/logs/trajectory.jsonl",
            "validation_render": "validation/source/scenes/small/renders/L1.wav",
        },
        "determinism": {
            "trajectory_sha256": deterministic_results[0]["trajectory_sha256"],
            "field_sha256": deterministic_results[0]["field_sha256"],
            "render_sha256": deterministic_results[0]["render_sha256"],
        },
    }
    logs_dir = root / "logs"
    logs_dir.mkdir(parents=True, exist_ok=True)
    atomic_write_json(logs_dir / "diagnostic_report.json", report)
    return report

