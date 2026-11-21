import numpy as np

from loudmeter.blocks import (k_weight, measure_block, reslice_diff,
                              slice_blocks, BlockWindow)
from loudmeter.config import MeterConfig


def test_slice_blocks_exact_and_remainder():
    w = slice_blocks(48000, 19200)
    assert [(x.start, x.end) for x in w] == [
        (0, 19200), (19200, 38400), (38400, 48000)]
    w = slice_blocks(50000, 19200)
    assert w[-1].end == 50000 and len(w) == 3


def test_reslice_diff():
    d = reslice_diff(96000, 19200, 9600)
    assert d["old_block_count"] == 5
    assert d["new_block_count"] == 10
    assert d["block_count_delta"] == 5
    assert d["max_window_offset_shift"] > 0
    same = reslice_diff(96000, 19200, 19200)
    assert same["block_count_delta"] == 0
    assert same["max_window_offset_shift"] == 0


def test_k_weighted_energy_sine():
    cfg = MeterConfig()
    n = 19200
    t = np.arange(n) / 48000.0
    amp = 0.1
    pcm = np.tile((amp * np.sin(2 * np.pi * 1000 * t))[:, None], (1, 6))
    m = measure_block("a", BlockWindow(0, 0, n), 0, pcm,
                      cfg.channel_weights, cfg.oversample_factor)
    # unweighted mean square = amp^2/2; weights sum of squares = 3 + 2*1.41^2;
    # the K-weighting shelf adds ~+0.7 dB at 1 kHz (ratio ~1.17).
    expect = (amp ** 2 / 2) * (3 + 2 * 1.41 ** 2)
    ratio = m.k_weighted_energy / expect
    assert 1.15 < ratio < 1.20
    assert m.sync_marker == 0
    assert len(m.content_hash) == 64


def test_true_peak_reference():
    # Windowed 6 kHz burst peaking halfway between samples: inter-sample
    # true peak = 0.5 (-6.02 dBTP), sample peaks clearly lower.
    n = 4800
    t_c = (n // 2 + 0.5) / 48000.0
    up_t = np.arange(n * 4) / (48000.0 * 4)
    env = np.cos(np.pi * (up_t - t_c) / (n / 48000.0)) ** 2
    env[np.abs(up_t - t_c) > (n / 48000.0) / 2] = 0.0
    up = 0.5 * env * np.cos(2 * np.pi * 6000.0 * (up_t - t_c))
    base = up[::4]
    pcm = np.tile(base[:, None], (1, 6))
    m = measure_block("tp", BlockWindow(0, 0, n), 0, pcm,
                      MeterConfig().channel_weights, 4)
    assert abs(m.true_peak_dbtp - 20 * np.log10(0.5)) < 0.3
    assert 20 * np.log10(np.abs(base).max()) < m.true_peak_dbtp - 0.3


def test_content_hash_sensitive_to_content():
    n = 960
    a = np.zeros((n, 6))
    b = np.zeros((n, 6)); b[0, 0] = 1e-6
    cfg = MeterConfig()
    ma = measure_block("a", BlockWindow(0, 0, n), 0, a, cfg.channel_weights, 4)
    mb = measure_block("a", BlockWindow(0, 0, n), 0, b, cfg.channel_weights, 4)
    assert ma.content_hash != mb.content_hash
