"""Deterministic gate-parameter comparison experiments."""

from __future__ import annotations

import copy
from collections.abc import Iterable
from typing import Any

from .gate import GateParameters, apply_gate
from .jsonutil import canonical_bytes, sha256_bytes


def _status_vector(row: dict[str, Any]) -> tuple[bool, bool, bool, bool, bool]:
    return (
        bool(row["absolute_pass"]),
        bool(row["relative_pass"]),
        bool(row["final_pass"]),
        bool(row["true_peak_pass"]),
        bool(row["compliance_pass"]),
    )


def run_batch(
    blocks: Iterable[dict[str, Any]],
    parameter_combinations: Iterable[GateParameters],
    *,
    asset_id: str | None = None,
    source_content_hash: str | None = None,
    analysis: dict[str, Any] | None = None,
) -> dict[str, Any]:
    combinations = list(parameter_combinations)
    if not combinations:
        raise ValueError("at least one parameter combination is required")
    base_blocks = copy.deepcopy(list(blocks))
    results: list[dict[str, Any]] = []
    matrix: dict[str, dict[str, Any]] = {
        row["block_id"]: {} for row in base_blocks
    }

    for index, parameters in enumerate(combinations):
        combo_id = f"{index:04d}"
        gated = apply_gate(copy.deepcopy(base_blocks), parameters)
        rows = gated["blocks"]
        results.append(
            {
                "combo_id": combo_id,
                "parameters": parameters.as_dict(),
                "absolute_threshold_energy": gated["absolute_threshold_energy"],
                "relative_threshold_energy": gated[
                    "relative_threshold_energy"
                ],
                "integrated_loudness": gated["integrated_loudness"],
                "max_true_peak": gated["max_true_peak"],
                "max_true_peak_dbtp": gated["max_true_peak_dbtp"],
                "true_peak_compliant": gated["true_peak_compliant"],
                "absolute_pass_count": gated["absolute_pass_count"],
                "relative_pass_count": gated["relative_pass_count"],
                "final_pass_count": gated["final_pass_count"],
                "removed_block_ids": gated["removed_block_ids"],
            }
        )
        for row in rows:
            matrix[row["block_id"]][combo_id] = {
                "absolute_pass": row["absolute_pass"],
                "relative_pass": row["relative_pass"],
                "final_pass": row["final_pass"],
                "true_peak_pass": row["true_peak_pass"],
                "compliance_pass": row["compliance_pass"],
                "removed_reason": row["removed_reason"],
            }

    flipped: set[str] = set()
    for block_id, states in matrix.items():
        vectors = set()
        for combo_id in states:
            state = states[combo_id]
            vectors.add(
                (
                    state["absolute_pass"],
                    state["relative_pass"],
                    state["final_pass"],
                    state["true_peak_pass"],
                    state["compliance_pass"],
                )
            )
        if len(vectors) > 1:
            flipped.add(block_id)

    report: dict[str, Any] = {
        "schema_version": 1,
        "asset_id": asset_id,
        "source_content_hash": source_content_hash,
        "analysis": copy.deepcopy(analysis),
        "combination_count": len(combinations),
        "block_count": len(base_blocks),
        "results": results,
        "status_matrix": matrix,
        "flipped_block_ids": sorted(flipped, key=int),
        "block_ids": [row["block_id"] for row in base_blocks],
    }
    report["report_hash"] = sha256_bytes(canonical_bytes(report))
    return report

