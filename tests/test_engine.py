"""Jitter buffer core: OLA length, gaps, dedup, committed-output freeze."""
import numpy as np
import pytest

from jitter_workbench.controller import ControllerParams
from jitter_workbench.engine import JitterEngine, WindowCommitError
from jitter_workbench.receiver import OfflineIngress


def test_output_length_matches_declaration(sample, params):
    eng = JitterEngine(sample, params)
    res = eng.run(OfflineIngress(sample.arrivals))
    assert res.output.shape[0] == sample.duration_samples
    assert res.stats["committed_windows"] == sample.num_frames
    assert len(res.gap_flags) == sample.num_frames


def test_gaps_are_silence_and_marked(sample, params):
    eng = JitterEngine(sample, params)
    res = eng.run(OfflineIngress(sample.arrivals))
    gap_seqs = [i for i, g in enumerate(res.gap_flags) if g]
    assert gap_seqs, "expected dropped frames to become gaps"
    for seq in gap_seqs:
        start = seq * sample.hop
        seg = res.output[start:start + sample.frame_len]
        # gap frame contributes exactly zero (neighbours may overlap)
        neighbour = np.zeros(sample.frame_len)
        for other in (seq - 1, seq + 1):
            if 0 <= other < sample.num_frames and not res.gap_flags[other]:
                ostart = (other - seq) * sample.hop
                lo = max(0, ostart)
                hi = min(sample.frame_len, ostart + sample.frame_len)
                neighbour[lo:hi] += np.asarray(
                    sample.frame_data(other))[lo - ostart:hi - ostart]
        assert np.allclose(seg, neighbour, atol=1e-6)
        assert res.energy[seq] == 0.0


def test_duplicates_dropped_and_recorded(sample, params):
    eng = JitterEngine(sample, params)
    res = eng.run(OfflineIngress(sample.arrivals))
    assert len(res.dup_records) == 5
    assert all(r["reason"] == "duplicate" for r in res.dup_records)


def test_energy_matches_truth_for_non_gap(sample, params):
    eng = JitterEngine(sample, params)
    res = eng.run(OfflineIngress(sample.arrivals))
    for seq in range(sample.num_frames):
        if not res.gap_flags[seq]:
            assert res.energy[seq] == pytest.approx(
                float(sample.truth_energy[seq]), rel=1e-9)


def test_committed_output_frozen_across_delay_shifts(sample):
    """Policy adjustments must never alter already-committed windows."""
    p1 = ControllerParams(init_delay_ms=120, max_delay_ms=400)
    p2 = ControllerParams(init_delay_ms=120, max_delay_ms=120)  # no raise
    r1 = JitterEngine(sample, p1).run(OfflineIngress(sample.arrivals))
    r2 = JitterEngine(sample, p2).run(OfflineIngress(sample.arrivals))
    # find first divergence in decisions; before that, outputs identical
    k1 = [d.key() for d in r1.decisions]
    k2 = [d.key() for d in r2.decisions]
    div = next((i for i, (a, b) in enumerate(zip(k1, k2)) if a != b),
               min(len(k1), len(k2)))
    if div:
        tick_ms = r1.decisions[div].tick
        # windows committed before that tick must be identical
        cut_window = max(0, (tick_ms - 120) // sample.hop_ms)
        cut_sample = cut_window * sample.hop
        assert np.array_equal(r1.output[:cut_sample],
                              r2.output[:cut_sample])


def test_window_failure_retry(sample, params, monkeypatch):
    eng = JitterEngine(sample, params)
    calls = {"n": 0}
    orig = sample.frame_data

    def flaky(seq):
        calls["n"] += 1
        if calls["n"] == 50:
            raise RuntimeError("injected decode failure")
        return orig(seq)

    monkeypatch.setattr(sample, "frame_data", flaky)
    with pytest.raises(WindowCommitError):
        eng.run(OfflineIngress(sample.arrivals))
    assert len(eng.failed_windows) == 1
    failed_seq = eng.failed_windows[0]
    committed_before = eng.next_seq
    assert committed_before == failed_seq
    # retry: failed window re-runs, completed windows untouched
    monkeypatch.setattr(sample, "frame_data", orig)
    out_before = eng.output.copy()
    eng2 = eng  # same engine continues
    res = eng2.run(OfflineIngress(sample.arrivals[eng.ingress_pos:]))
    assert res.stats["committed_windows"] == sample.num_frames
    assert np.array_equal(res.output[:committed_before * sample.hop],
                          out_before[:committed_before * sample.hop])
