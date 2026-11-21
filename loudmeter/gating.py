"""Two-stage gated integrated loudness (BS.1770 gating).

Stage 1: ungated integrated loudness over all blocks -> relative gate.
Stage 2: absolute gate + relative gate select blocks -> gated integrated
loudness and the max true peak over the selected blocks.

The relative gate depends on the ungated integrated value, which depends on
the whole block set: any change to the block set invalidates the previous
gating conclusion, so callers must recompute the full two-stage result
instead of reusing partial values from an older gate.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .blocks import BlockMeasurement, LOUDNESS_OFFSET


@dataclass(frozen=True)
class GatingResult:
    ungated_integrated_lufs: float     # stage-1 intermediate
    relative_gate_lufs: float          # stage-1 intermediate
    abs_gate_lufs: float
    integrated_lufs: float             # stage-2 gated integrated
    true_peak_max_dbtp: float          # stage-2 max true peak
    selected_block_ids: tuple          # blocks passing both gates
    gated_out_block_ids: tuple         # blocks filtered out

    def to_dict(self) -> dict:
        return {
            "ungated_integrated_lufs": self.ungated_integrated_lufs,
            "relative_gate_lufs": self.relative_gate_lufs,
            "abs_gate_lufs": self.abs_gate_lufs,
            "integrated_lufs": self.integrated_lufs,
            "true_peak_max_dbtp": self.true_peak_max_dbtp,
            "selected_block_ids": list(self.selected_block_ids),
            "gated_out_block_ids": list(self.gated_out_block_ids),
        }


def _integrated(energies: list[float]) -> float:
    total = sum(energies)
    if total <= 0.0:
        return float("-inf")
    return LOUDNESS_OFFSET + 10.0 * np.log10(total / len(energies))


def compute_gating(blocks: list[BlockMeasurement], abs_gate_lufs: float,
                   rel_gate_offset_lu: float) -> GatingResult:
    """Run the full two-stage gating over a complete block list."""
    if not blocks:
        raise ValueError("cannot gate an empty block set")

    energies = [b.k_weighted_energy for b in blocks]
    ungated = _integrated(energies)
    relative_gate = ungated + rel_gate_offset_lu

    selected = []
    gated_out = []
    for b in blocks:
        loud = b.short_term_lufs
        if loud >= abs_gate_lufs and loud >= relative_gate:
            selected.append(b)
        else:
            gated_out.append(b)

    if selected:
        integrated = _integrated([b.k_weighted_energy for b in selected])
        tp_max = max(b.true_peak_dbtp for b in selected)
    else:
        integrated = float("-inf")
        tp_max = float("-inf")

    return GatingResult(
        ungated_integrated_lufs=float(ungated),
        relative_gate_lufs=float(relative_gate),
        abs_gate_lufs=float(abs_gate_lufs),
        integrated_lufs=float(integrated),
        true_peak_max_dbtp=float(tp_max),
        selected_block_ids=tuple(b.index for b in selected),
        gated_out_block_ids=tuple(b.index for b in gated_out),
    )
