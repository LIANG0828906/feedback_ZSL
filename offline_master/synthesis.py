"""Deterministic WAV stem synthesis.

Every stem is generated from the integer seed declared in the manifest via a
private :class:`numpy.random.Generator` (PCG64). Re-running with identical
parameters produces byte-identical WAV files. The declared start offset and the
optional group phase injection (polarity / fractional delay) are fully
recoverable from the manifest and applied here so later pipeline stages can be
tested against known truth.
"""

from __future__ import annotations

import os

import numpy as np
import soundfile as sf

from .dsp import frac_delay

SUBTYPE_BY_DEPTH = {16: "PCM_16", 24: "PCM_24", 32: "PCM_32"}


def _rng(seed: int) -> np.random.Generator:
    return np.random.default_rng(np.random.SeedSequence(seed))


def _pluck(rng: np.random.Generator, n: int, sr: int, freq: float, decay: float) -> np.ndarray:
    t = np.arange(n) / sr
    harmonic = np.zeros(n)
    for partial, amplitude in enumerate((1.0, 0.5, 0.28, 0.15), start=1):
        phase = rng.uniform(0.0, 2.0 * np.pi)
        harmonic += amplitude * np.sin(2.0 * np.pi * freq * partial * t + phase)
    envelope = np.exp(-decay * t)
    return harmonic * envelope


def _dialogue(rng: np.random.Generator, n: int, sr: int) -> np.ndarray:
    """Vowel-like harmonic phrases separated by plosive (p/b) bursts."""
    out = np.zeros(n)
    phrase_step = int(0.72 * sr)
    cursor = int(0.08 * sr)
    phrase_index = 0
    while cursor < n - phrase_step // 2:
        vowel_f0 = float(rng.uniform(110.0, 150.0))
        # Plosive burst: short broadband impulse followed by a low pressure thump.
        burst_len = int(0.012 * sr)
        burst = rng.standard_normal(burst_len) * np.hanning(burst_len)
        thump_len = int(0.03 * sr)
        tt = np.arange(thump_len) / sr
        thump = 0.8 * np.sin(2 * np.pi * 90 * tt) * np.exp(-90 * tt)
        onset = cursor
        out[onset:onset + burst_len] += 0.55 * burst
        end_thump = min(n, onset + thump_len)
        out[onset:end_thump] += thump[: end_thump - onset]
        # Voiced vowel body with harmonic stack and slowly varying pitch.
        body_len = int(rng.uniform(0.22, 0.34) * sr)
        body_len = min(body_len, n - onset)
        t = np.arange(body_len) / sr
        vibrato = 1.0 + 0.015 * np.sin(2 * np.pi * 5 * t)
        phase_acc = 2 * np.pi * vowel_f0 * np.cumsum(vibrato) / sr
        body = np.zeros(body_len)
        for partial, amplitude in enumerate((1.0, 0.55, 0.32, 0.18, 0.1), start=1):
            body += amplitude * np.sin(partial * phase_acc + rng.uniform(0, 2 * np.pi))
        env = np.minimum(1.0, t / 0.02) * np.exp(-3.5 * t)
        out[onset:onset + body_len] += 0.4 * body * env
        cursor += phrase_step
        phrase_index += 1
    return out


def _footstep(rng: np.random.Generator, n: int, sr: int) -> np.ndarray:
    """Heel-strike + toe-strike transient pairs on a regular gait."""
    out = np.zeros(n)
    step_period = int(0.6 * sr)
    # Gait starts at 0.1 s so heel strikes coincide with the shared rhythmic
    # thump grid used by phase-correction groups.
    cursor = int(0.1 * sr)
    while cursor < n - int(0.1 * sr):
        heel_len = int(0.02 * sr)
        t = np.arange(heel_len) / sr
        heel = rng.standard_normal(heel_len) * np.exp(-180 * t)
        heel += 0.9 * np.sin(2 * np.pi * 120 * t) * np.exp(-160 * t)
        toe_start = cursor + int(0.07 * sr)
        toe_len = int(0.015 * sr)
        t2 = np.arange(toe_len) / sr
        toe = rng.standard_normal(toe_len) * np.exp(-220 * t2)
        toe += 0.6 * np.sin(2 * np.pi * 180 * t2) * np.exp(-200 * t2)
        if cursor + heel_len <= n:
            out[cursor:cursor + heel_len] += 0.8 * heel
        if toe_start + toe_len <= n:
            out[toe_start:toe_start + toe_len] += 0.55 * toe
        cursor += step_period
    return out


def _cloth(rng: np.random.Generator, n: int, sr: int) -> np.ndarray:
    """Short broadband friction-noise bursts, one per gait cycle."""
    out = np.zeros(n)
    period = int(0.6 * sr)
    # First friction burst starts in the second gait cycle, keeping every
    # shared rhythmic thump onset free of broadband friction contamination.
    cursor = int(1.60 * sr)
    while cursor < n - int(0.05 * sr):
        length = int(rng.uniform(0.05, 0.09) * sr)
        length = min(length, n - cursor)
        noise = rng.standard_normal(length)
        # Rough spectral shaping toward broadband cloth "shhh" energy.
        shaped = noise - 0.35 * np.roll(noise, 1)
        shaped[0] = noise[0]
        env = np.hanning(length) ** 1.5
        out[cursor:cursor + length] += 0.5 * shaped * env
        cursor += period
    return out


def _ambient(rng: np.random.Generator, n: int, sr: int) -> np.ndarray:
    """Continuous low-level bed: slow LFO tones plus filtered airy noise."""
    t = np.arange(n) / sr
    bed = 0.05 * np.sin(2 * np.pi * 55 * t + 0.7)
    bed += 0.035 * np.sin(2 * np.pi * 82.4 * t + 1.9)
    bed += 0.025 * np.sin(2 * np.pi * 0.13 * t)
    noise = rng.standard_normal(n)
    kernel = np.ones(64) / 64.0
    airy = np.convolve(noise, kernel, mode="same")
    return bed + 0.04 * airy


SYNTHESIZERS = {
    "dialogue": _dialogue,
    "footstep": _footstep,
    "cloth": _cloth,
    "ambient": _ambient,
}

TYPE_GAIN = {
    "dialogue": 0.55,
    "footstep": 0.70,
    "cloth": 0.60,
    "ambient": 0.35,
}


def rhythm_thump(n: int, sr: int, start_sample: int) -> np.ndarray:
    """Shared deterministic 60 Hz rhythmic thump used by phase-correction groups.

    It contains no randomness: footstep and cloth stems in the same group embed
    the identical waveform, so the relative delay/polarity estimator has
    recoverable low-frequency truth to lock onto.
    """
    out = np.zeros(n)
    period = int(0.6 * sr)
    cursor = start_sample
    while 0 <= cursor < n:
        length = int(0.12 * sr)
        t = np.arange(length) / sr
        # Fast attack gives every grouped stem a sharp, detectable onset; the
        # exponential decay preserves the 20-120 Hz low-band energy.
        attack = np.minimum(1.0, t / 0.0005)
        low = np.sin(2 * np.pi * 60 * t) * attack * np.exp(-28 * t)
        # A single-sample impulse marks the exact emission sample; it is the
        # unambiguous window peak on every grouped stem regardless of polarity.
        click = np.zeros(length)
        click[0] = 1.0
        thump = 0.5 * low + 1.0 * click
        end = min(n, cursor + length)
        if cursor >= 0:
            out[cursor:end] += thump[: end - cursor]
        cursor += period
    return out


def synthesize_stem(track: dict, sr: int, bit_depth: int, duration_s: float,
                    base_dir: str) -> str:
    """Synthesize one manifest track to its declared WAV path."""
    rng = _rng(int(track["seed"]))
    n = int(round(duration_s * sr))
    material = track["material_type"]
    signal = SYNTHESIZERS[material](rng, n, sr)

    group = track.get("phase_group")
    if group:
        injection = track.get("phase_inject", {})
        thump = 0.9 * rhythm_thump(n, sr, int(round(0.1 * sr)))
        signal += thump
        delay = float(injection.get("delay_samples", 0.0))
        if delay:
            signal = frac_delay(signal, delay)
            # Re-quantize each emission marker onto its nearest integer sample
            # so transient detection locks to a single exact sample, while the
            # tonal body retains the injected fractional phase shift.
            marker_positions = np.arange(
                int(round(0.1 * sr)), n, int(0.6 * sr)
            )
            for marker in marker_positions:
                base = int(np.floor(marker + delay))
                shifted = int(np.ceil(marker + delay - 1e-12))
                if 0 <= shifted < n:
                    for clear_index in (base, base + 1):
                        if 0 <= clear_index < n:
                            signal[clear_index] = 0.0
                    signal[shifted] = 0.9
        if int(injection.get("polarity", 1)) < 0:
            signal = -signal

    offset = int(round(float(track["start_offset_s"]) * sr))
    placed = np.zeros(n + offset, dtype=np.float64)
    placed[offset:offset + n] = signal

    # Fixed per-type gain (no peak normalization): group members keep their
    # designed low-frequency level relationships and onset detectability.
    placed *= TYPE_GAIN[material]
    peak = float(np.max(np.abs(placed)))
    if peak > 0.99:
        placed *= 0.99 / peak

    path = os.path.join(base_dir, track["file"])
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    sf.write(path, placed.astype(np.float64), sr, subtype=SUBTYPE_BY_DEPTH[bit_depth])
    return path


def synthesize_project(manifest: dict, base_dir: str) -> list[str]:
    """Synthesize every stem declared by the manifest."""
    return [
        synthesize_stem(
            track,
            manifest["sample_rate"],
            manifest["bit_depth"],
            manifest["duration_s"],
            base_dir,
        )
        for track in manifest["tracks"]
    ]
