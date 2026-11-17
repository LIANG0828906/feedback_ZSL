"""Deterministic multitrack material synthesis.

Given a project directory, an integer seed and a song-segment id this module
synthesises instrument-like channel signals (sustained bass, transient drums,
harmonic-rich melodies), injects narrowband resonance components that follow
the low-frequency envelope into the channels declared as ``feedback`` role,
writes a monitor reference track and the project JSON.  The same parameters
always produce byte-identical files.

A ``ground_truth`` directory with the clean (pre-injection) signals and the
injection manifest is written alongside for verification purposes.
"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import soundfile as sf

from .dsp import bandpass_zero_phase, moving_rms

SAMPLE_RATE = 48000
SUBTYPE = "PCM_24"

# Resonance peaks of the venue's dynamic-microphone batch (Hz).  Chosen so
# that a +/-10 Hz guard band never collides with any synthesised partial.
MIC_RESONANCES = (96.0, 190.0, 240.0, 310.0)

BASS_NOTES = (55.0, 65.41, 73.42, 82.41)
MELODY_NOTES = (220.0, 261.63, 293.66, 329.63, 392.0, 440.0)


def resonance_frequency(song_segment: int) -> float:
    """The hum fundamental depends on the song-segment id."""
    base = MIC_RESONANCES[song_segment % len(MIC_RESONANCES)]
    return base + 1.5 * ((song_segment // len(MIC_RESONANCES)) % 2)


def _envelope(n: int, sr: int, attack: float, release: float) -> np.ndarray:
    env = np.ones(n)
    a = max(1, int(attack * sr))
    r = max(1, int(release * sr))
    env[: min(a, n)] = np.linspace(0.0, 1.0, min(a, n), endpoint=False)
    tail = np.linspace(1.0, 0.0, min(r, n), endpoint=True)
    env[n - len(tail):] = np.minimum(env[n - len(tail):], tail)
    return env


def _place(sig: np.ndarray, start: int, frag: np.ndarray) -> None:
    end = min(len(sig), start + len(frag))
    if end > start:
        sig[start:end] += frag[: end - start]


def synth_bass(rng: np.random.Generator, n: int, sr: int) -> np.ndarray:
    sig = np.zeros(n)
    note_len = int(0.5 * sr)
    t = np.arange(note_len) / sr
    pos = 0
    while pos < n:
        f = BASS_NOTES[rng.integers(0, len(BASS_NOTES))]
        env = _envelope(note_len, sr, 0.01, 0.12)
        frag = env * (np.sin(2 * np.pi * f * t) + 0.06 * np.sin(4 * np.pi * f * t))
        _place(sig, pos, 0.5 * frag)
        pos += note_len
    return sig


def synth_drums(rng: np.random.Generator, n: int, sr: int) -> np.ndarray:
    sig = np.zeros(n)
    beat = int(0.5 * sr)
    thump_len = int(0.18 * sr)
    t = np.arange(thump_len) / sr
    pos = int(0.25 * sr)
    while pos + thump_len < n:
        frag = np.exp(-t * 22.0) * np.sin(2 * np.pi * 60.0 * t)
        noise_len = int(0.03 * sr)
        noise = rng.standard_normal(noise_len) * np.exp(-np.arange(noise_len) / sr * 160.0)
        frag[:noise_len] += 0.35 * noise
        _place(sig, pos, 0.6 * frag)
        pos += beat
    return sig


def synth_melody(rng: np.random.Generator, n: int, sr: int, brightness: float = 1.0) -> np.ndarray:
    sig = np.zeros(n)
    pos = 0
    while pos < n:
        dur_s = float(rng.uniform(0.35, 0.7))
        m = int(dur_s * sr)
        f = MELODY_NOTES[rng.integers(0, len(MELODY_NOTES))]
        t = np.arange(m) / sr
        env = _envelope(m, sr, 0.02, 0.15)
        frag = np.zeros(m)
        for h in range(1, 6):
            frag += (brightness / h ** 2) * np.sin(2 * np.pi * f * h * t)
        _place(sig, pos, 0.25 * env * frag)
        pos += m
    return sig


def synth_pad(rng: np.random.Generator, n: int, sr: int) -> np.ndarray:
    t = np.arange(n) / sr
    sig = np.zeros(n)
    for f in (220.0, 277.18, 329.63):
        trem = 0.75 + 0.25 * np.sin(2 * np.pi * 0.5 * t + rng.uniform(0, 2 * np.pi))
        sig += 0.08 * trem * (np.sin(2 * np.pi * f * t) + 0.3 * np.sin(4 * np.pi * f * t))
    return sig


def _assert_resonance_clear(f_res: float, guard_hz: float = 10.0) -> None:
    partials = set(BASS_NOTES)
    partials.update(2 * f for f in BASS_NOTES)
    partials.add(60.0)  # drum thump
    partials.update(MELODY_NOTES)
    partials.update((220.0, 277.18, 329.63))  # pad fundamentals
    for p in partials:
        if abs(p - f_res) < guard_hz:
            raise AssertionError(f"resonance {f_res} Hz collides with partial {p} Hz")


def generate_project(out_dir: str | Path, seed: int = 7, song_segment: int = 2,
                     duration: float = 6.0, sr: int = SAMPLE_RATE) -> Path:
    """Synthesise the whole project deterministically; returns project path."""
    out = Path(out_dir)
    audio = out / "audio"
    truth = out / "ground_truth"
    audio.mkdir(parents=True, exist_ok=True)
    truth.mkdir(parents=True, exist_ok=True)

    n = int(round(duration * sr))
    f_res = resonance_frequency(song_segment)
    _assert_resonance_clear(f_res)

    rng = np.random.default_rng(seed)
    instruments = [
        ("kick", "direct", "drums", synth_drums(np.random.default_rng(rng.integers(2 ** 31)), n, sr)),
        ("bass", "direct", "bass", synth_bass(np.random.default_rng(rng.integers(2 ** 31)), n, sr)),
        ("keys", "direct", "melody", synth_melody(np.random.default_rng(rng.integers(2 ** 31)), n, sr, 1.0)),
        ("pad", "direct", "pad", synth_pad(np.random.default_rng(rng.integers(2 ** 31)), n, sr)),
        ("gtr", "feedback", "melody", synth_melody(np.random.default_rng(rng.integers(2 ** 31)), n, sr, 0.7)),
        ("vox", "feedback", "melody", synth_melody(np.random.default_rng(rng.integers(2 ** 31)), n, sr, 0.5)),
    ]

    # Global low-frequency envelope of the direct channels: the stage rumble
    # that pumps the vibration-coupled feedback.
    low_mix = np.zeros(n)
    for name, role, _, sig in instruments:
        if role == "direct":
            low_mix += sig
    low = bandpass_zero_phase(low_mix, sr, 20.0, 120.0, numtaps=2049)
    env = moving_rms(low, sr, 0.15)
    env_max = float(env.max())
    if env_max > 0:
        env /= env_max

    t = np.arange(n) / sr
    ref_component = 0.5 * env * np.sin(2 * np.pi * f_res * t)

    inj_rng = np.random.default_rng(seed + 99991)
    injected: dict[str, dict] = {}
    channels_meta = []
    for name, role, instrument, clean in instruments:
        sig = clean
        if role == "feedback":
            gain = float(inj_rng.uniform(0.08, 0.16))
            sig = clean + gain * ref_component
            injected[name] = {"gain": gain, "fundamental_hz": f_res}
        sf.write(str(audio / f"{name}.wav"), sig, sr, subtype=SUBTYPE)
        sf.write(str(truth / f"{name}.wav"), clean, sr, subtype=SUBTYPE)
        channels_meta.append(
            {"name": name, "role": role, "instrument": instrument,
             "file": f"audio/{name}.wav", "channels": 1}
        )

    sf.write(str(audio / "ref_monitor.wav"), ref_component, sr, subtype=SUBTYPE)
    channels_meta.append(
        {"name": "ref_monitor", "role": "reference", "instrument": "reference",
         "file": "audio/ref_monitor.wav", "channels": 1}
    )

    weights = {"kick": 0.18, "bass": 0.20, "keys": 0.16, "pad": 0.12, "gtr": 0.08, "vox": 0.06}
    project = {
        "sample_rate": sr,
        "duration_seconds": duration,
        "seed": seed,
        "song_segment": song_segment,
        "channels": channels_meta,
        "downmix": {"weights": weights, "compensation": "normalize"},
        "loudness_target_lufs": -16.0,
        "loudness_tolerance_lu": 0.5,
        "true_peak_ceiling_dbtp": -1.0,
        "allowed_false_positives": 1,
        "min_attenuation_db": 20.0,
        "block_size": 48000,
    }
    project_path = out / "project.json"
    project_path.write_text(json.dumps(project, indent=2, sort_keys=True), encoding="utf-8")

    truth_manifest = {
        "seed": seed,
        "song_segment": song_segment,
        "fundamental_hz": f_res,
        "injected_channels": injected,
        "clean_files": {name: f"ground_truth/{name}.wav" for name, _, _, _ in instruments},
    }
    (truth / "manifest.json").write_text(
        json.dumps(truth_manifest, indent=2, sort_keys=True), encoding="utf-8"
    )
    return project_path
