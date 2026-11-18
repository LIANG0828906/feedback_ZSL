"""Deterministic synthesis of the six opera sound-scene channels.

Every random draw is derived from the single integer seed declared in the cue
sheet via ``numpy.random.default_rng`` with explicit child keys, so repeated
generations with identical parameters are byte-for-byte identical.
"""

from __future__ import annotations

import os

import numpy as np

from . import (
    PART_BANGU,
    PART_BANGQIANG,
    PART_BED,
    PART_DALUO,
    PART_XIAOLUO,
    PART_YUEQIN,
)


def _rng(seed, tag):
    return np.random.default_rng(np.random.SeedSequence([int(seed), hash_tag(tag)]))


def hash_tag(tag):
    h = 1469598103934665603
    for byte in tag.encode("utf-8"):
        h ^= byte
        h = (h * 1099511628211) & 0xFFFFFFFFFFFFFFFF
    return h


def _time_axis(n, sr):
    return np.arange(n, dtype=np.float64) / sr


def _add_decaying(signal, sr, onset, freqs, amps, decay, attack=0.002,
                  initial_phase=0.0):
    n = signal.shape[0]
    start = int(round(onset * sr))
    if start >= n:
        return
    t0 = np.arange(n - start, dtype=np.float64) / sr
    env = np.exp(-t0 / max(decay, 1e-6))
    a = min(int(round(attack * sr)), env.size)
    if a > 1:
        env[:a] *= np.linspace(0.0, 1.0, a)
    tone = np.zeros_like(t0)
    for k, (f, amp) in enumerate(zip(freqs, amps)):
        phase = initial_phase + k * 0.37
        tone += amp * np.sin(2.0 * np.pi * f * t0 + phase)
    signal[start:] += env * tone


def _bandpassed_noise(n, rng, low_hz, high_hz, sr):
    noise = rng.standard_normal(n)
    spec = np.fft.rfft(noise)
    freqs = np.fft.rfftfreq(n, 1.0 / sr)
    mask = (freqs >= low_hz) & (freqs <= high_hz)
    spec *= mask
    out = np.fft.irfft(spec, n)
    peak = np.max(np.abs(out))
    if peak > 0:
        out /= peak
    return out


def _add_noise_burst(signal, sr, onset, burst_seconds, decay, rng,
                     low_hz, high_hz, amp):
    n = signal.shape[0]
    start = int(round(onset * sr))
    if start >= n:
        return
    length = min(int(round(burst_seconds * sr)), n - start)
    shaped = _bandpassed_noise(length, rng, low_hz, high_hz, sr)
    t = np.arange(length, dtype=np.float64) / sr
    env = np.exp(-t / max(decay, 1e-6))
    a = min(max(int(round(0.001 * sr)), 1), length)
    env[:a] *= np.linspace(0.0, 1.0, a)
    signal[start : start + length] += amp * env * shaped


def synth_daluo(n, sr, ref_times, rng, jitter=0):
    signal = np.zeros(n)
    ratios = (1.0, 1.42, 1.93, 2.41, 2.81, 3.47)
    base = 196.0
    freqs = [base * r for r in ratios]
    amps = [1.0, 0.62, 0.45, 0.33, 0.21, 0.13]
    for i, t in enumerate(ref_times):
        onset = t + jitter / sr
        _add_decaying(
            signal, sr, onset, freqs, amps, decay=0.9,
            attack=0.002, initial_phase=rng.uniform(0, 2 * np.pi),
        )
    return 0.55 * signal


def synth_xiaoluo(n, sr, ref_times, rng, jitter=0):
    signal = np.zeros(n)
    ratios = (1.0, 1.51, 2.12, 2.76, 3.39)
    base = 880.0
    freqs = [base * r for r in ratios]
    amps = [1.0, 0.55, 0.36, 0.22, 0.12]
    for t in ref_times:
        onset = t + jitter / sr
        _add_decaying(
            signal, sr, onset, freqs, amps, decay=0.28,
            attack=0.0015, initial_phase=rng.uniform(0, 2 * np.pi),
        )
    return 0.4 * signal


def synth_bangu(n, sr, ref_times, rng, jitter=0):
    signal = np.zeros(n)
    for t in ref_times:
        onset = t + jitter / sr
        _add_noise_burst(
            signal, sr, onset, burst_seconds=0.06, decay=0.012,
            rng=rng, low_hz=2500.0, high_hz=9000.0, amp=0.5,
        )
    return signal


def synth_bangqiang(n, sr, ref_times, rng):
    signal = np.zeros(n)
    base_freqs = (320.0, 358.0, 302.0, 379.0, 338.0)
    harmonics = (1.0, 2.0, 3.0, 4.0)
    harm_amps = (0.5, 0.28, 0.14, 0.07)
    t = _time_axis(n, sr)
    vibrato = 4.5 * np.sin(2 * np.pi * 5.0 * t)
    for i, anchor in enumerate(ref_times):
        f0 = base_freqs[i % len(base_freqs)]
        start = int(round(anchor * sr))
        end = min(
            n,
            start
            + int(round(1.15 * sr))
            - (i * 137) % 257,
        )
        if end <= start:
            continue
        idx = np.arange(start, end)
        local_t = (idx - start) / sr
        phase_acc = 2 * np.pi * f0 * local_t + (
            2 * np.pi * 4.5
            * (local_t - np.sin(2 * np.pi * 5.0 * local_t) / (2 * np.pi * 5.0))
        )
        note = np.zeros_like(local_t)
        for h, amp in zip(harmonics, harm_amps):
            note += amp * np.sin(h * phase_acc + 0.11 * h)
        attack = min(int(0.06 * sr), note.size)
        release = min(int(0.35 * sr), note.size)
        env = np.ones_like(local_t)
        if attack > 1:
            env[:attack] = np.linspace(0.0, 1.0, attack)
        if release > 1:
            env[-release:] *= np.linspace(1.0, 0.0, release)
        signal[start:end] += 0.4 * env * note
    return signal


def synth_yueqin(n, sr, ref_times, rng):
    signal = np.zeros(n)
    arpeggio = (261.63, 329.63, 392.0, 523.25, 392.0, 329.63)
    for i, anchor in enumerate(ref_times):
        for k, f in enumerate(arpeggio):
            onset = anchor + k * 0.09
            _add_decaying(
                signal, sr, onset, [f, 2 * f, 3 * f],
                [0.5, 0.22, 0.09], decay=0.32, attack=0.003,
                initial_phase=rng.uniform(0, 2 * np.pi),
            )
    return 0.3 * signal


def synth_bed(n, rng):
    return rng.standard_normal(n) * (10.0 ** (-50.0 / 20.0))


def build_clean_sources(cue):
    """Synthesize the six pre-injection signals (dict keyed by part name)."""
    sr = cue.sample_rate
    n = cue.total_samples
    signals = {}
    jitter = {ch.part: ch.jitter_samples for ch in cue.channels}
    signals[PART_DALUO] = synth_daluo(
        n, sr, cue.reference_times, _rng(cue.seed, PART_DALUO),
        jitter.get(PART_DALUO, 0),
    )
    signals[PART_XIAOLUO] = synth_xiaoluo(
        n, sr, cue.reference_times, _rng(cue.seed, PART_XIAOLUO),
        jitter.get(PART_XIAOLUO, 0),
    )
    signals[PART_BANGU] = synth_bangu(
        n, sr, cue.reference_times, _rng(cue.seed, PART_BANGU),
        jitter.get(PART_BANGU, 0),
    )
    signals[PART_BANGQIANG] = synth_bangqiang(
        n, sr, cue.reference_times, _rng(cue.seed, PART_BANGQIANG)
    )
    signals[PART_YUEQIN] = synth_yueqin(
        n, sr, cue.reference_times, _rng(cue.seed, PART_YUEQIN)
    )
    signals[PART_BED] = synth_bed(n, _rng(cue.seed, PART_BED))
    return signals


def apply_crosstalk(signals, cue):
    """Inject the declared source->victim crosstalk inside declared windows."""
    sr = cue.sample_rate
    n = cue.total_samples
    for ch in cue.channels:
        if not ch.crosstalk_source:
            continue
        source = signals[ch.crosstalk_source]
        victim = signals[ch.part]
        gain = 10.0 ** (ch.crosstalk_gain_db / 20.0)
        win_start = int(round(ch.crosstalk_window[0] * sr))
        win_end = min(n, int(round(ch.crosstalk_window[1] * sr)))
        mask = np.zeros(n)
        mask[win_start:win_end] = 1.0
        fade = min(int(0.005 * sr), max((win_end - win_start) // 4, 1))
        if fade > 1:
            ramp = np.linspace(0.0, 1.0, fade)
            mask[win_start : win_start + fade] = ramp
            mask[win_end - fade : win_end] = ramp[::-1]
        victim += gain * source * mask


def generate_channels(cue, base_dir=None):
    """Generate and persist the six WAV files; return the float signals."""
    from .wavio import write_wav

    signals = build_clean_sources(cue)
    apply_crosstalk(signals, cue)
    for ch in cue.channels:
        path = ch.path
        if base_dir and not os.path.isabs(path):
            path = os.path.join(base_dir, path)
        os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
        write_wav(path, signals[ch.part], cue.sample_rate, cue.bit_depth)
    return signals
