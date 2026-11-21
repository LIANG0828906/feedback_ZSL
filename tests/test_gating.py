import numpy as np
import pytest

from loudmeter.blocks import BlockMeasurement, LOUDNESS_OFFSET
from loudmeter.gating import compute_gating


def _blk(idx, energy, tp=-10.0):
    loud = LOUDNESS_OFFSET + 10 * np.log10(energy) if energy > 0 else float("-inf")
    return BlockMeasurement("a", idx, idx * 100, (idx + 1) * 100, idx,
                            f"hash{idx}", energy, loud, tp)


def test_two_stage_values():
    blocks = [_blk(0, 0.01), _blk(1, 0.01), _blk(2, 1e-9)]
    g = compute_gating(blocks, -70.0, -10.0)
    # stage 1: ungated over all three
    exp_ungated = LOUDNESS_OFFSET + 10 * np.log10((0.020000001) / 3)
    assert g.ungated_integrated_lufs == pytest.approx(exp_ungated)
    assert g.relative_gate_lufs == pytest.approx(exp_ungated - 10.0)
    # stage 2: the tiny block is gated out by the relative gate
    assert g.gated_out_block_ids == (2,)
    assert g.selected_block_ids == (0, 1)
    assert g.integrated_lufs == pytest.approx(LOUDNESS_OFFSET + 10 * np.log10(0.01))
    assert g.true_peak_max_dbtp == -10.0


def test_absolute_gate():
    quiet = LOUDNESS_OFFSET + 10 * np.log10(1e-8)  # about -80.7 LUFS
    blocks = [_blk(0, 0.01), _blk(1, 1e-8)]
    g = compute_gating(blocks, -70.0, -10.0)
    assert 1 in g.gated_out_block_ids  # below the -70 LUFS absolute gate


def test_gate_linkage_blockset_change():
    # The relative gate depends on the whole block set: adding a loud block
    # moves the gate and changes the selection.
    base = [_blk(0, 0.01), _blk(1, 0.001)]
    g1 = compute_gating(base, -70.0, -10.0)
    extended = base + [_blk(2, 1.0)]
    g2 = compute_gating(extended, -70.0, -10.0)
    assert g1.relative_gate_lufs != g2.relative_gate_lufs
    assert g1.selected_block_ids != g2.selected_block_ids


def test_empty_set_rejected():
    with pytest.raises(ValueError):
        compute_gating([], -70.0, -10.0)
