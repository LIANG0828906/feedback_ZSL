"""Deterministic multi-microphone stage recording synthesis.

Everything is derived from the manifest seed, the per-microphone seed and the
action segment timings. Re-running generation with the same manifest produces
byte-identical WAV files and an injection-truth sidecar used by the tests.
"""

from __future__ import annotations

import hashlib
import json
import os

import numpy as np
import soundfile as sf

from .errors import BusinessCode, StageNoiseError
from .manifest import Manifest


def _rng(seed: int, *parts: str) -> np.random.Generator:
    digest = hashlib.sha256(
        "|".join([str(seed), *parts]).encode("utf-8")).digest()
    return np.random.default_rng(np.frombuffer(digest, dtype=np.uint32))


def _envelope(n: int, rng: np.random.Generator, sr: int,
              syllable_hz: float) -> np.ndarray:
    """Slow speech-like amplitude envelope (always > 0)."""
    t = np.arange(n) / sr
    env = 0.55 + 0.45 * np.sin(2 * np.pi * syllable_hz * t
                               + rng.uniform(0, 2 * np.pi))
    env *= 0.8 + 0.2 * np.sin(2 * np.pi * syllable_hz / 3.1 * t
                              + rng.uniform(0, 2 * np.pi))
    return np.clip(env, 0.05, 1.0)


def _dialogue(n: int, rng: np.random.Generator, sr: int,
              base_freq: float) -> np.ndarray:
    """Speech-like signal: three formant tones above the mechanical band."""
    t = np.arange(n) / sr
    vibrato = 1.0 + 0.012 * np.sin(2 * np.pi * 5.5 * t
                                   + rng.uniform(0, 2 * np.pi))
    f0 = base_freq * vibrato
    phase = 2 * np.pi * np.cumsum(f0) / sr
    sig = (1.0 * np.sin(phase)
           + 0.55 * np.sin(2.0 * phase + 0.7)
           + 0.30 * np.sin(3.0 * phase + 1.9))
    sig *= _envelope(n, rng, sr, rng.uniform(2.8, 4.2))
    sig += 0.05 * rng.standard_normal(n)
    return 0.16 * sig.astype(np.float64)


def _accompaniment(n: int, rng: np.random.Generator, sr: int,
                   root_hz: float) -> np.ndarray:
    """Band-like accompaniment: chord tones plus gentle overtones."""
    t = np.arange(n) / sr
    ratios = (1.0, 1.2, 1.5, 2.0)
    weights = (1.0, 0.7, 0.5, 0.25)
    sig = np.zeros(n)
    for ratio, weight in zip(ratios, weights):
        freq = root_hz * ratio
        vibrato = 1.0 + 0.004 * np.sin(2 * np.pi * 4.7 * t
                                       + rng.uniform(0, 2 * np.pi))
        phase = 2 * np.pi * freq * vibrato * t
        sig += weight * np.sin(phase + rng.uniform(0, 2 * np.pi))
    sig /= np.max(np.abs(sig))
    sig *= 0.11 * (0.85 + 0.15 * np.sin(2 * np.pi * 0.5 * t))
    return sig.astype(np.float64)


def _background(n: int, rng: np.random.Generator) -> np.ndarray:
    white = rng.standard_normal(n)
    pink = np.cumsum(white)
    pink /= max(np.std(pink), 1e-12)
    return 0.006 * white + 0.004 * pink


def _mechanical(n: int, rng: np.random.Generator, sr: int,
                center_hz: float) -> np.ndarray:
    """Squeak: band-limited low frequency tone + narrow-band filtered noise."""
    t = np.arange(n) / sr
    wobble = center_hz * (1.0 + 0.03 * np.sin(2 * np.pi * 7.0 * t))
    phase = 2 * np.pi * np.cumsum(wobble) / sr
    tone = np.sin(phase) + 0.4 * np.sin(2 * phase + 1.3)
    noise = rng.standard_normal(n)
    kernel = np.exp(-np.linspace(0.0, 1.0, 64) / 0.25)
    kernel /= kernel.sum()
    rumble = np.convolve(noise, kernel, mode="same")
    rumble *= np.sin(2 * np.pi * 11.0 * t + rng.uniform(0, 2 * np.pi))
    creak = tone + 0.6 * rumble
    fade_len = min(int(0.05 * sr), n // 4)
    if fade_len > 0:
        creak[:fade_len] *= np.linspace(0.0, 1.0, fade_len)
        creak[-fade_len:] *= np.linspace(1.0, 0.0, fade_len)
    return creak


def generate(manifest: Manifest) -> dict:
    sr = manifest.sample_rate
    n_total = manifest.total_samples
    margin_samples = int(round(manifest.injection_margin_ms / 1000.0 * sr))
    truth = {
        "seed": manifest.seed,
        "sample_rate_hz": sr,
        "total_samples": n_total,
        "noise_reference_band_hz": list(manifest.noise_band_hz),
        "microphones": [],
    }
    for mic in manifest.mics:
        rng = _rng(manifest.seed, mic.name, str(mic.seed))
        dialogue_base = float(rng.uniform(280.0, 340.0))
        accomp_root = float(rng.uniform(261.0, 300.0))
        signal = (_dialogue(n_total, _rng(manifest.seed, mic.name, "dialogue"),
                            sr, dialogue_base)
                  + _accompaniment(n_total,
                                   _rng(manifest.seed, mic.name, "music"),
                                   sr, accomp_root)
                  + _background(n_total,
                                _rng(manifest.seed, mic.name, "background")))
        injections = []
        band_low, band_high = manifest.noise_band_hz
        for action in manifest.actions:
            gain = action.position_gains.get(mic.position, 0.0)
            if gain <= 0.0:
                continue
            noise_rng = _rng(manifest.seed, mic.name, action.segment_id,
                             "mechanical")
            center_hz = float(noise_rng.uniform(band_low + 12.0,
                                                band_high - 12.0))
            start = max(0, action.start_sample - margin_samples)
            end = min(n_total, action.end_sample + margin_samples)
            creak = _mechanical(end - start, noise_rng, sr, center_hz)
            signal[start:end] += 0.22 * gain * creak
            injections.append({
                "segment_id": action.segment_id,
                "mechanism": action.mechanism,
                "injected": True,
                "start_sample": int(start),
                "end_sample": int(end),
                "start_time": round(start / sr, 6),
                "end_time": round(end / sr, 6),
                "center_frequency_hz": round(center_hz, 4),
                "position_gain": gain,
            })
        peak = float(np.max(np.abs(signal)))
        if peak > 0.95:
            signal *= 0.95 / peak
        out_path = os.path.join(manifest.source_dir, mic.path)
        os.makedirs(os.path.dirname(out_path), exist_ok=True)
        sf.write(out_path, signal, sr, subtype=mic.subtype)
        truth["microphones"].append({
            "mic_name": mic.name,
            "position": mic.position,
            "dialogue_base_hz": round(dialogue_base, 4),
            "accompaniment_root_hz": round(accomp_root, 4),
            "injections": injections,
        })
    truth_path = os.path.join(manifest.source_dir, "injection_truth.json")
    with open(truth_path, "w", encoding="utf-8") as handle:
        json.dump(truth, handle, sort_keys=False, indent=2)
        handle.write("\n")
    truth["truth_path"] = truth_path
    return truth


def verify_wav_headers(manifest: Manifest) -> None:
    """Validate that on-disk WAVs match every declared header property."""
    from .manifest import SUPPORTED_SUBTYPES

    for mic in manifest.mics:
        path = os.path.join(manifest.source_dir, mic.path)
        if not os.path.isfile(path):
            raise StageNoiseError(BusinessCode.FILE_MISSING,
                                  f"wav missing for mic '{mic.name}'",
                                  mic=mic.name, details={"path": mic.path})
        try:
            info = sf.info(path)
        except (RuntimeError, OSError) as exc:
            raise StageNoiseError(BusinessCode.FILE_UNREADABLE,
                                  f"cannot read wav: {exc}", mic=mic.name)
        if info.samplerate != mic.sample_rate:
            raise StageNoiseError(
                BusinessCode.SAMPLE_RATE_MISMATCH,
                f"actual rate {info.samplerate} != declared {mic.sample_rate}",
                mic=mic.name,
                details={"actual_hz": info.samplerate,
                         "declared_hz": mic.sample_rate})
        if info.channels != mic.channels:
            raise StageNoiseError(
                BusinessCode.CHANNEL_COUNT_MISMATCH,
                f"actual channels {info.channels} != declared {mic.channels}",
                mic=mic.name,
                details={"actual_channels": info.channels,
                         "declared_channels": mic.channels})
        if info.subtype != mic.subtype or \
                SUPPORTED_SUBTYPES.get(info.subtype) != mic.bit_depth:
            raise StageNoiseError(
                BusinessCode.BIT_DEPTH_MISMATCH,
                f"actual subtype {info.subtype} != declared {mic.subtype}",
                mic=mic.name,
                details={"actual_subtype": info.subtype,
                         "declared_subtype": mic.subtype,
                         "declared_bit_depth": mic.bit_depth})
