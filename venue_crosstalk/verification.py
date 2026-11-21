"""Acceptance verifier for generated sample workspaces."""

from __future__ import annotations

import math
from pathlib import Path
from typing import Any

from .config import config_from_dict
from .errors import E_POLARITY_UNKNOWN, VenueError
from .utils import atomic_write_json, read_json


def _check(name: str, ok: bool, detail: Any) -> dict[str, Any]:
    return {"name": name, "ok": bool(ok), "detail": detail}


def verify_published_workspace(
    workspace: str | Path,
    *,
    max_relative_magnitude_error: float = 0.16,
    max_phase_error_deg: float = 12.0,
    max_alignment_residual_samples: int = 2,
    output: str | Path | None = None,
) -> dict[str, Any]:
    root = Path(workspace).resolve()
    snapshot_path = root / "publish" / "snapshot.json"
    truth_path = root / "ground_truth.json"
    try:
        snapshot = read_json(snapshot_path)
        truth = read_json(truth_path)
    except (OSError, ValueError) as exc:
        raise VenueError(
            "E_NOT_PUBLISHED",
            f"cannot read published snapshot or ground truth: {exc}",
            {"workspace": str(root)},
        ) from exc
    truth_by_pair = {item["pair"]: item for item in truth.get("pairs", [])}
    relative_errors: list[float] = []
    phase_errors: list[float] = []
    coupling_count = 0
    matrix_mismatches: list[dict[str, Any]] = []
    for section in snapshot.get("bands", []):
        band_name = section["spec"]["name"]
        layout = section["matrix"]["layout"]
        sources = layout["sources"]
        pickups = layout["pickups"]
        source_index = {name: index for index, name in enumerate(sources)}
        pickup_index = {name: index for index, name in enumerate(pickups)}
        for estimate in section.get("coupling", []):
            pair = truth_by_pair.get(estimate["pair"])
            if pair is None:
                continue
            expected_magnitude = float(pair["band_gains"][band_name])
            relative_errors.append(
                abs(float(estimate["coupling_magnitude"]) - expected_magnitude)
                / expected_magnitude
            )
            expected_phase = float(pair["band_phases_deg"][band_name])
            phase_errors.append(
                abs(
                    (float(estimate["phase_deg"]) - expected_phase + 180.0) % 360.0
                    - 180.0
                )
            )
            pair_parts = estimate["pair"].split(">", 1)
            if len(pair_parts) == 2:
                row = pickup_index.get(pair_parts[1])
                column = source_index.get(pair_parts[0])
                if row is not None and column is not None:
                    matrix_value = float(layout["magnitude"][row][column])
                    if abs(matrix_value - float(estimate["coupling_magnitude"])) > 1e-12:
                        matrix_mismatches.append(
                            {
                                "pair": estimate["pair"],
                                "band": band_name,
                                "coupling": estimate["coupling_magnitude"],
                                "matrix": matrix_value,
                            }
                        )
            coupling_count += 1
    alignment_residuals = [
        abs(int(item["residual_offset_samples"]))
        for item in snapshot.get("alignments", [])
    ]
    phase_by_pair = {
        item["pair"]: item for item in snapshot.get("phase_continuity", [])
    }
    config = config_from_dict(snapshot["effective_config"])
    calibration_violations: list[dict[str, Any]] = []
    constraint_order_ok = True
    expected_order = [
        "phase_polarity",
        "cancel_gain_limit",
        "primary_suppression_limit",
        "target_suppression",
    ]
    for item in snapshot.get("calibration", []):
        if item["constraint_order"] != expected_order:
            constraint_order_ok = False
        if float(item["cancel_gain_linear"]) > config.max_cancel_gain + 1e-12:
            calibration_violations.append(
                {
                    "pair": item["pair"],
                    "band": item["band"],
                    "reason": "max_cancel_gain",
                    "value": item["cancel_gain_linear"],
                }
            )
        if (
            float(item["primary_suppression_db"])
            > config.max_primary_suppression_db + 1e-9
        ):
            calibration_violations.append(
                {
                    "pair": item["pair"],
                    "band": item["band"],
                    "reason": "max_primary_suppression_db",
                    "value": item["primary_suppression_db"],
                }
            )
    budget_checks: list[dict[str, Any]] = []
    for section in snapshot.get("bands", []):
        budget = section["residual_budget"]
        budget_checks.append(
            {
                "band": section["spec"]["name"],
                "pair_rows": len(budget["pairs"]),
                "pickup_rows": len(budget["pickups"]),
                "has_target_and_gap": all(
                    "target_suppression_db" in row and "gap_db" in row
                    for row in budget["pairs"]
                ),
            }
        )
    final_incremental = snapshot.get("incremental", {})
    checks = [
        _check(
            "coupling_estimate_count",
            coupling_count == len(truth_by_pair) * len(snapshot.get("bands", [])),
            {
                "actual": coupling_count,
                "expected": len(truth_by_pair) * len(snapshot.get("bands", [])),
            },
        ),
        _check(
            "coupling_magnitude_accuracy",
            bool(relative_errors)
            and max(relative_errors) <= max_relative_magnitude_error,
            {
                "mean_relative_error": (
                    sum(relative_errors) / len(relative_errors)
                    if relative_errors
                    else None
                ),
                "max_relative_error": (
                    max(relative_errors) if relative_errors else None
                ),
                "threshold": max_relative_magnitude_error,
            },
        ),
        _check(
            "coupling_phase_accuracy",
            bool(phase_errors) and max(phase_errors) <= max_phase_error_deg,
            {
                "mean_error_deg": (
                    sum(phase_errors) / len(phase_errors) if phase_errors else None
                ),
                "max_error_deg": max(phase_errors) if phase_errors else None,
                "threshold_deg": max_phase_error_deg,
            },
        ),
        _check(
            "geometry_alignment_residual",
            bool(alignment_residuals)
            and max(alignment_residuals) <= max_alignment_residual_samples,
            {
                "max_abs_residual_samples": (
                    max(alignment_residuals) if alignment_residuals else None
                ),
                "threshold_samples": max_alignment_residual_samples,
            },
        ),
        _check(
            "true_inversion_detected",
            phase_by_pair.get("FL>FR", {}).get("verdict") == "inverted",
            phase_by_pair.get("FL>FR"),
        ),
        _check(
            "boundary_wrap_not_misreported_as_inversion",
            phase_by_pair.get("FL>FC", {}).get("verdict") == "normal"
            and bool(phase_by_pair.get("FL>FC", {}).get("boundary_wraps")),
            phase_by_pair.get("FL>FC"),
        ),
        _check(
            "ambiguous_polarity_reported",
            phase_by_pair.get("FR>FL", {}).get("code") == E_POLARITY_UNKNOWN,
            phase_by_pair.get("FR>FL"),
        ),
        _check(
            "low_band_group_delay_reported",
            bool(
                phase_by_pair.get("LFE>SL", {}).get(
                    "group_delay_discontinuities"
                )
            ),
            phase_by_pair.get("LFE>SL"),
        ),
        _check("routing_matrix_consistency", not matrix_mismatches, matrix_mismatches),
        _check(
            "residual_budget_completeness",
            all(
                item["pair_rows"] == len(truth_by_pair)
                and item["pickup_rows"] == 6
                and item["has_target_and_gap"]
                for item in budget_checks
            ),
            budget_checks,
        ),
        _check(
            "calibration_gain_and_primary_limits",
            not calibration_violations and constraint_order_ok,
            {
                "violations": calibration_violations,
                "constraint_order_ok": constraint_order_ok,
            },
        ),
        _check(
            "incremental_band_scope",
            set(final_incremental.get("changed_bands", [])) == {"low", "mid"}
            and len(snapshot.get("estimation_changes", []))
            == 2 * len(truth_by_pair),
            {
                "changed_bands": final_incremental.get("changed_bands"),
                "estimation_change_count": len(
                    snapshot.get("estimation_changes", [])
                ),
            },
        ),
    ]
    report = {
        "ok": all(item["ok"] for item in checks),
        "workspace": str(root),
        "snapshot_run_id": snapshot.get("run_id"),
        "snapshot_content_hash": snapshot.get("content_hash"),
        "trace_chain_hash": snapshot.get("trace", {}).get("final_chain_hash"),
        "metrics": {
            "pair_count": len(truth_by_pair),
            "band_count": len(snapshot.get("bands", [])),
            "coupling_estimate_count": coupling_count,
            "mean_relative_magnitude_error": (
                sum(relative_errors) / len(relative_errors)
                if relative_errors
                else None
            ),
            "max_relative_magnitude_error": (
                max(relative_errors) if relative_errors else None
            ),
            "mean_phase_error_deg": (
                sum(phase_errors) / len(phase_errors) if phase_errors else None
            ),
            "max_phase_error_deg": max(phase_errors) if phase_errors else None,
            "max_alignment_residual_samples": (
                max(alignment_residuals) if alignment_residuals else None
            ),
            "final_cache_hits": final_incremental.get("cache_hits"),
            "final_cache_misses": final_incremental.get("cache_misses"),
        },
        "checks": checks,
    }
    if output is not None:
        atomic_write_json(output, report)
    return report
