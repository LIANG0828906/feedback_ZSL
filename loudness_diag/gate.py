"""Two-stage BS.1770 gating with per-block decision evidence."""

from __future__ import annotations

import copy
import math
from collections.abc import Iterable
from dataclasses import asdict, dataclass
from typing import Any

from .dsp import energy_to_loudness, loudness_to_energy


@dataclass(frozen=True)
class GateParameters:
    absolute_lufs: float = -70.0
    relative_offset_lu: float = -10.0
    true_peak_limit_dbtp: float = -1.0

    def __post_init__(self) -> None:
        if not math.isfinite(self.absolute_lufs):
            raise ValueError("absolute_lufs must be finite")
        if not math.isfinite(self.relative_offset_lu):
            raise ValueError("relative_offset_lu must be finite")
        if not math.isfinite(self.true_peak_limit_dbtp):
            raise ValueError("true_peak_limit_dbtp must be finite")

    @property
    def absolute_energy(self) -> float:
        return loudness_to_energy(self.absolute_lufs)

    @property
    def true_peak_limit_linear(self) -> float:
        return 10.0 ** (self.true_peak_limit_dbtp / 20.0)

    def as_dict(self) -> dict[str, float]:
        return {
            "absolute_lufs": self.absolute_lufs,
            "relative_offset_lu": self.relative_offset_lu,
            "true_peak_limit_dbtp": self.true_peak_limit_dbtp,
        }


def _peak_dbtp(value: float) -> float | None:
    if value <= 0.0 or not math.isfinite(value):
        return None
    return 20.0 * math.log10(value)


def apply_gate(
    blocks: Iterable[dict[str, Any]], parameters: GateParameters
) -> dict[str, Any]:
    """Apply absolute gating, derive the relative threshold, then gate again."""

    rows = copy.deepcopy(list(blocks))
    absolute_energy = parameters.absolute_energy
    absolute_pass_energies: list[float] = []
    for row in rows:
        energy = float(row["ungated_energy"])
        absolute_pass = energy >= absolute_energy
        row["absolute_pass"] = absolute_pass
        if absolute_pass:
            absolute_pass_energies.append(energy)

    if absolute_pass_energies:
        first_stage_energy = sum(absolute_pass_energies) / len(
            absolute_pass_energies
        )
        first_stage_loudness = energy_to_loudness(first_stage_energy)
        relative_loudness = (
            None
            if first_stage_loudness is None
            else first_stage_loudness + parameters.relative_offset_lu
        )
        relative_energy = (
            None if relative_loudness is None else loudness_to_energy(relative_loudness)
        )
    else:
        first_stage_energy = None
        first_stage_loudness = None
        relative_loudness = None
        relative_energy = None

    final_energies: list[float] = []
    true_peak_limit_linear = parameters.true_peak_limit_linear
    for row in rows:
        energy = float(row["ungated_energy"])
        absolute_pass = bool(row["absolute_pass"])
        relative_pass = (
            relative_energy is not None and energy >= relative_energy
        )
        final_pass = absolute_pass and relative_pass
        true_peak = float(row["true_peak"])
        true_peak_pass = true_peak <= true_peak_limit_linear
        compliance_pass = final_pass and true_peak_pass
        if not absolute_pass:
            removed_reason = "absolute_gate"
        elif not relative_pass:
            removed_reason = "relative_gate"
        elif not true_peak_pass:
            removed_reason = "true_peak_limit"
        else:
            removed_reason = None

        row["relative_pass"] = relative_pass
        row["final_pass"] = final_pass
        row["true_peak_dbtp"] = _peak_dbtp(true_peak)
        row["true_peak_pass"] = true_peak_pass
        row["compliance_pass"] = compliance_pass
        row["removed_reason"] = removed_reason
        if final_pass:
            final_energies.append(energy)

    integrated_loudness = (
        energy_to_loudness(sum(final_energies) / len(final_energies))
        if final_energies
        else None
    )
    max_true_peak = max((float(row["true_peak"]) for row in rows), default=0.0)
    max_true_peak_dbtp = _peak_dbtp(max_true_peak)
    ungated_loudness = (
        energy_to_loudness(
            sum(float(row["ungated_energy"]) for row in rows) / len(rows)
        )
        if rows
        else None
    )
    return {
        "parameters": parameters.as_dict(),
        "absolute_threshold_energy": absolute_energy,
        "absolute_threshold_lufs": parameters.absolute_lufs,
        "first_stage_energy": first_stage_energy,
        "first_stage_loudness": first_stage_loudness,
        "relative_threshold_energy": relative_energy,
        "relative_threshold_loudness": relative_loudness,
        "relative_offset_lu": parameters.relative_offset_lu,
        "ungated_loudness": ungated_loudness,
        "integrated_loudness": integrated_loudness,
        "integrated_energy": (
            sum(final_energies) / len(final_energies) if final_energies else None
        ),
        "max_true_peak": max_true_peak,
        "max_true_peak_dbtp": max_true_peak_dbtp,
        "true_peak_limit_dbtp": parameters.true_peak_limit_dbtp,
        "true_peak_compliant": max_true_peak <= true_peak_limit_linear,
        "absolute_pass_count": sum(1 for row in rows if row["absolute_pass"]),
        "relative_pass_count": sum(1 for row in rows if row["relative_pass"]),
        "final_pass_count": sum(1 for row in rows if row["final_pass"]),
        "compliance_pass_count": sum(1 for row in rows if row["compliance_pass"]),
        "removed_count": sum(1 for row in rows if row["removed_reason"] is not None),
        "removed_block_ids": [
            row["block_id"] for row in rows if row["removed_reason"] is not None
        ],
        "gated_block_ids": [row["block_id"] for row in rows if row["final_pass"]],
        "blocks": rows,
    }


def reconstruct_gate_decisions(
    blocks: Iterable[dict[str, Any]],
    *,
    absolute_threshold_energy: float,
    relative_threshold_energy: float | None,
) -> dict[str, dict[str, Any]]:
    """Rebuild every gate decision from block IDs and stored energies alone."""

    decisions: dict[str, dict[str, Any]] = {}
    for row in blocks:
        energy = float(row["ungated_energy"])
        absolute_pass = energy >= absolute_threshold_energy
        relative_pass = (
            relative_threshold_energy is not None
            and energy >= relative_threshold_energy
        )
        final_pass = absolute_pass and relative_pass
        if not absolute_pass:
            removed_reason = "absolute_gate"
        elif not relative_pass:
            removed_reason = "relative_gate"
        else:
            removed_reason = None
        decisions[row["block_id"]] = {
            "absolute_pass": absolute_pass,
            "relative_pass": relative_pass,
            "final_pass": final_pass,
            "removed_reason": removed_reason,
        }
    return decisions


def gate_parameters_dict(parameters: GateParameters) -> dict[str, float]:
    return asdict(parameters)

