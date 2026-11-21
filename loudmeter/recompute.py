"""Incremental recompute and gating-invalidation after parameter changes.

After target loudness / true-peak ceiling / gate / channel-weight changes,
compute the affected block set and decide whether the gating selection
changes. Only when the selection is provably unchanged may just the
affected blocks be re-metered; otherwise the whole integrated conclusion
is recomputed. The result is published as a single atomic replacement.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .blocks import (BlockMeasurement, measure_block, reslice_diff,
                     slice_blocks)
from .config import MeterConfig
from .gating import GatingResult, compute_gating


@dataclass(frozen=True)
class RecomputePlan:
    mode: str                    # "incremental" | "full"
    affected_block_ids: tuple
    gating_selection_changed: bool
    reslice: dict | None         # present when block/oversample params changed
    reason: str


def _measure_all(asset_id: str, samples: np.ndarray, config: MeterConfig,
                 sync_start: int = 0) -> list[BlockMeasurement]:
    windows = slice_blocks(samples.shape[0], config.block_samples)
    out = []
    for w in windows:
        out.append(measure_block(asset_id, w, sync_start + w.index,
                                 samples[w.start:w.end],
                                 config.channel_weights,
                                 config.oversample_factor))
    return out


def plan_recompute(asset_id: str,
                   samples: np.ndarray,
                   old_config: MeterConfig,
                   new_config: MeterConfig,
                   old_blocks: list[BlockMeasurement],
                   old_gating: GatingResult) -> tuple[RecomputePlan, list[BlockMeasurement], GatingResult]:
    """Decide and execute the recompute; returns (plan, blocks, gating)."""

    structure_changed = (
        old_config.block_samples != new_config.block_samples
        or old_config.oversample_factor != new_config.oversample_factor
    )
    weights_changed = old_config.channel_weights != new_config.channel_weights
    gates_changed = (
        old_config.abs_gate_lufs != new_config.abs_gate_lufs
        or old_config.rel_gate_offset_lu != new_config.rel_gate_offset_lu
    )

    reslice = None
    if structure_changed:
        reslice = reslice_diff(samples.shape[0], old_config.block_samples,
                               new_config.block_samples)

    if structure_changed or weights_changed:
        affected = tuple(range(len(slice_blocks(samples.shape[0],
                                                new_config.block_samples))))
    else:
        affected = tuple()  # gate/target/ceiling changes touch no block values

    # Candidate block values under the new parameters.
    if structure_changed or weights_changed:
        new_blocks = _measure_all(asset_id, samples, new_config)
    else:
        new_blocks = list(old_blocks)

    new_gating = compute_gating(new_blocks, new_config.abs_gate_lufs,
                                new_config.rel_gate_offset_lu)
    selection_changed = (
        new_gating.selected_block_ids != old_gating.selected_block_ids
    )

    if structure_changed:
        plan = RecomputePlan("full", affected, True, reslice,
                             "block length or oversampling changed; re-sliced")
    elif selection_changed or (weights_changed and gates_changed):
        # The relative gate depends on the ungated integrated value of the
        # whole block set: once the selection moves, no partial result from
        # the old gate may be reused.
        plan = RecomputePlan("full", affected, True, reslice,
                             "gating selection changed; full integrated recompute")
    elif weights_changed:
        plan = RecomputePlan("incremental", affected, False, reslice,
                             "channel weights changed but gating selection stable")
    elif gates_changed:
        plan = RecomputePlan("incremental", affected, False, reslice,
                             "gate parameters changed; block values reused")
    else:
        plan = RecomputePlan("incremental", affected, False, reslice,
                             "target/ceiling change only; no metering impact")

    return plan, new_blocks, new_gating
