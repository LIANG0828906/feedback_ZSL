"""End-to-end metering pipeline helpers shared by the demo and the tests."""

from __future__ import annotations

import numpy as np

from .blocks import BlockMeasurement, measure_block, slice_blocks
from .config import MeterConfig
from .gating import compute_gating


def meter_asset(asset_id: str,
                samples: np.ndarray,
                config: MeterConfig,
                committed: tuple = (),
                stop_after: int | None = None,
                sync_start: int = 0) -> list[BlockMeasurement]:
    """Meter blocks in order, skipping already committed ones.

    ``stop_after`` simulates a forced termination after N newly metered
    blocks (no partial block is ever produced: a block is either fully
    metered and committed, or absent).
    """
    committed_ids = set(committed)
    windows = slice_blocks(samples.shape[0], config.block_samples)
    out = []
    for w in windows:
        if w.index in committed_ids:
            continue
        out.append(measure_block(asset_id, w, sync_start + w.index,
                                 samples[w.start:w.end],
                                 config.channel_weights,
                                 config.oversample_factor))
        if stop_after is not None and len(out) >= stop_after:
            break
    return out


def build_manifest(asset_id: str, config: MeterConfig,
                   blocks: list[BlockMeasurement], params_version: int) -> dict:
    """Assemble the immutable publish manifest (measurements + gating)."""
    gating = compute_gating(blocks, config.abs_gate_lufs,
                            config.rel_gate_offset_lu)
    return {
        "asset_id": asset_id,
        "params_version": params_version,
        "params": config.to_dict(),
        "blocks": [b.to_dict() for b in sorted(blocks, key=lambda b: b.index)],
        "gating": gating.to_dict(),
    }
