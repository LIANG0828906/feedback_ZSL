import numpy as np

from loudmeter.config import MeterConfig
from loudmeter.gating import compute_gating
from loudmeter.pipeline import meter_asset
from loudmeter.recompute import plan_recompute


def _samples():
    n = 48000
    t = np.arange(n) / 48000.0
    sig = np.zeros(n)
    loud = 0.25 * np.sin(2 * np.pi * 800 * t[:24000])
    sig[:24000] = loud
    sig[24000:] = 1e-5 * np.sin(2 * np.pi * 800 * t[:24000])  # very quiet
    return np.tile(sig[:, None], (1, 6))


def test_gate_only_change_is_incremental():
    cfg = MeterConfig()
    s = _samples()
    blocks = meter_asset("a", s, cfg)
    gating = compute_gating(blocks, cfg.abs_gate_lufs, cfg.rel_gate_offset_lu)
    cfg2 = cfg.update(target_loudness_lufs=-23.0,
                      true_peak_ceiling_dbtp=-1.0)
    plan, new_blocks, _ = plan_recompute("a", s, cfg, cfg2, blocks, gating)
    assert plan.mode == "incremental"
    assert plan.affected_block_ids == ()
    assert new_blocks == blocks  # block values untouched


def test_weight_change_with_stable_selection_is_incremental():
    cfg = MeterConfig()
    s = _samples()
    blocks = meter_asset("a", s, cfg)
    gating = compute_gating(blocks, cfg.abs_gate_lufs, cfg.rel_gate_offset_lu)
    cfg2 = cfg.update(channel_weights=(1.0, 1.0, 1.0, 0.0, 1.0, 1.0))
    plan, new_blocks, new_gating = plan_recompute("a", s, cfg, cfg2, blocks, gating)
    assert plan.mode == "incremental"
    assert plan.gating_selection_changed is False
    assert new_gating.selected_block_ids == gating.selected_block_ids
    assert new_blocks[0].k_weighted_energy != blocks[0].k_weighted_energy


def test_selection_change_forces_full_recompute():
    cfg = MeterConfig()
    s = _samples()
    blocks = meter_asset("a", s, cfg)
    gating = compute_gating(blocks, cfg.abs_gate_lufs, cfg.rel_gate_offset_lu)
    # tightening the relative gate moves the selection -> full recompute
    cfg2 = cfg.update(rel_gate_offset_lu=-2.0)
    plan, _, new_gating = plan_recompute("a", s, cfg, cfg2, blocks, gating)
    assert plan.mode == "full"
    assert plan.gating_selection_changed is True
    assert new_gating.selected_block_ids != gating.selected_block_ids


def test_block_length_change_reslices():
    cfg = MeterConfig()
    s = _samples()
    blocks = meter_asset("a", s, cfg)
    gating = compute_gating(blocks, cfg.abs_gate_lufs, cfg.rel_gate_offset_lu)
    cfg2 = cfg.update(block_samples=9600)
    plan, new_blocks, _ = plan_recompute("a", s, cfg, cfg2, blocks, gating)
    assert plan.mode == "full"
    assert plan.reslice["old_block_count"] == 3
    assert plan.reslice["new_block_count"] == 5
    assert plan.reslice["block_count_delta"] == 2
    assert plan.reslice["max_window_offset_shift"] > 0
    assert len(new_blocks) == 5
