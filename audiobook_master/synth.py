"""Deterministic synthetic multi-batch audiobook packages."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .config import load_manifest_plan, load_rule_config
from .util import sha256_file, stable_seed
from .wavio import read_wav, write_wav


@dataclass(slots=True, frozen=True)
class BatchProfile:
    level: float
    drive: float
    noise_level: float
    transient_level: float
    brightness: float


BATCH_PROFILES: dict[str, BatchProfile] = {
    "light": BatchProfile(
        level=0.060, drive=1.0, noise_level=0.0025, transient_level=0.52, brightness=1.0
    ),
    "quiet": BatchProfile(
        level=0.055, drive=1.0, noise_level=0.0025, transient_level=0.48, brightness=1.0
    ),
    "normal": BatchProfile(
        level=0.105, drive=1.1, noise_level=0.0030, transient_level=0.68, brightness=1.0
    ),
    "reference": BatchProfile(
        level=0.105, drive=1.1, noise_level=0.0030, transient_level=0.68, brightness=1.0
    ),
    "compressed": BatchProfile(
        level=0.175, drive=3.8, noise_level=0.0060, transient_level=0.98, brightness=1.15
    ),
    "heavy_compressed": BatchProfile(
        level=0.175, drive=4.5, noise_level=0.0060, transient_level=0.995, brightness=1.20
    ),
    "mic_b": BatchProfile(
        level=0.085, drive=1.25, noise_level=0.0040, transient_level=0.64, brightness=0.75
    ),
    "alternate_mic": BatchProfile(
        level=0.085, drive=1.25, noise_level=0.0040, transient_level=0.64, brightness=0.75
    ),
}


def generate_packages(
    rules_path: str | Path,
    manifest_path: str | Path,
) -> dict[str, Any]:
    """Generate every manifest package and return stable package statistics."""

    rules_path = Path(rules_path)
    manifest_path = Path(manifest_path)
    rules = load_rule_config(rules_path)
    manifest = load_manifest_plan(manifest_path, rules)
    outputs: list[dict[str, Any]] = []
    for chapter in manifest.chapters:
        for package in chapter.packages:
            sample_rate = package.declared_sample_rate or rules.sample_rate
            frames = package.expected_frames
            seed = stable_seed(
                "audiobook-package-v1",
                chapter.rule.id,
                package.index,
                package.batch,
                package.report_file,
            )
            samples = _synthesize_package(
                frames=frames,
                sample_rate=sample_rate,
                channels=package.channels,
                batch=package.batch,
                seed=seed,
            )
            bit_depth = package.declared_bit_depth or rules.bit_depth
            write_wav(
                package.path,
                samples,
                sample_rate=sample_rate,
                bit_depth=bit_depth,
            )
            audio = read_wav(package.path)
            outputs.append(
                {
                    "chapter": chapter.rule.id,
                    "file": package.report_file,
                    "batch": package.batch,
                    "sample_rate": sample_rate,
                    "channels": audio.info.channels,
                    "frames": audio.info.frames,
                    "effective_samples": audio.info.frames,
                    "duration_seconds": audio.info.frames / sample_rate,
                    "peak_dbfs": _peak_dbfs(audio.samples),
                    "true_peak_dbtp": _true_peak(audio.samples),
                    "sha256": sha256_file(package.path),
                }
            )
    return {
        "ok": True,
        "status": "generated",
        "packages": outputs,
    }


def _synthesize_package(
    *,
    frames: int,
    sample_rate: int,
    channels: int,
    batch: str,
    seed: int,
) -> np.ndarray:
    profile = BATCH_PROFILES.get(batch, _profile_for_unknown_batch(batch))
    time = np.arange(frames, dtype=np.float64) / sample_rate
    duration = frames / sample_rate
    base = np.zeros(frames, dtype=np.float64)
    phrase_period = 1.35
    phrase_length = 0.92
    phrase_index = 0
    while True:
        phrase_start = 0.08 + phrase_index * phrase_period
        if phrase_start >= duration:
            break
        length = min(phrase_length, duration - phrase_start)
        if length > 0.02:
            local = time[: int(round(length * sample_rate))]
            local_time = local - phrase_start
            start_frame = int(round(phrase_start * sample_rate))
            end_frame = start_frame + local.shape[0]
            envelope = _raised_cosine_gate(
                local_time, length, attack=0.035, release=0.12
            )
            f0 = 190.0 + 22.0 * math_sin(phrase_index * 0.73)
            f1 = 720.0 + 180.0 * math_sin(phrase_index * 0.41)
            sweep_rate = max(0.1, (f1 - f0) / max(length, 1e-9))
            phase = 2.0 * np.pi * (
                f0 * local_time + 0.5 * sweep_rate * local_time * local_time
            )
            sweep = (
                0.58 * np.sin(phase)
                + 0.20 * np.sin(2.01 * phase)
                + 0.09 * np.sin(3.03 * phase)
            )
            base[start_frame:end_frame] += sweep * envelope
        phrase_index += 1

    noise_generator = np.random.default_rng(seed ^ 0x9E3779B97F4A7C15)
    noise = noise_generator.standard_normal((frames, channels))
    # A gentle one-pole colouring keeps the deterministic noise from sounding
    # like a full-band digital hiss while retaining a measurable quiet floor.
    for channel in range(channels):
        coloured = np.empty(frames, dtype=np.float64)
        state = 0.0
        coefficient = 0.82
        for index in range(frames):
            state = coefficient * state + (1.0 - coefficient) * noise[index, channel]
            coloured[index] = state
        noise[:, channel] = coloured
    noise *= profile.noise_level / max(float(np.std(noise)), 1e-9)

    transient_phase = np.random.default_rng(seed ^ 0xD1B54A32D192ED03)
    transient = np.zeros(frames, dtype=np.float64)
    event_time = 0.105
    frequency = 1450.0 * profile.brightness
    while event_time < duration:
        start = int(round(event_time * sample_rate))
        length = min(int(round(0.050 * sample_rate)), frames - start)
        if length > 0:
            local_time = np.arange(length, dtype=np.float64) / sample_rate
            decay = np.exp(-local_time / 0.009)
            attack = np.minimum(1.0, local_time / 0.0008)
            click = (
                np.sin(2.0 * np.pi * frequency * local_time) * 0.82
                + 0.18 * transient_phase.standard_normal(length)
            )
            transient[start : start + length] += (
                profile.transient_level * decay * attack * click
            )
        event_time += phrase_period

    body = base
    if abs(profile.drive - 1.0) > 1e-9:
        body = np.tanh(profile.drive * body) / np.tanh(profile.drive)
    body *= profile.level
    transient_scale = max(float(np.max(np.abs(transient))), 1e-9)
    mono = body + transient / transient_scale * profile.transient_level
    if channels == 1:
        mixed = mono[:, None] + noise[:, :1]
    else:
        mixed = np.empty((frames, channels), dtype=np.float64)
        for channel in range(channels):
            delayed = np.roll(mono, channel * max(1, int(0.0007 * sample_rate)))
            if channel:
                delayed[: channel * max(1, int(0.0007 * sample_rate))] = 0.0
            mixed[:, channel] = delayed * (1.0 - 0.08 * channel)
        mixed += noise

    if frames < int(1.5 * sample_rate):
        silence_frames = min(int(round(0.05 * sample_rate)), frames // 10)
        fade_frames = min(int(round(0.015 * sample_rate)), frames // 10)
    else:
        silence_frames = min(int(round(0.25 * sample_rate)), frames // 5)
        fade_frames = min(int(round(0.05 * sample_rate)), frames // 5)
    if fade_frames > 0:
        ramp = np.sin(
            np.linspace(0.0, 0.5 * np.pi, fade_frames, dtype=np.float64)
        ) ** 2
        mixed[:silence_frames] = 0.0
        mixed[-silence_frames:] = 0.0
        mixed[silence_frames : silence_frames + fade_frames] *= ramp[:, None]
        mixed[
            -silence_frames - fade_frames : -silence_frames
        ] *= ramp[::-1, None]

    # Deterministic safety clip belongs to the source media; the mastering
    # limiter is still exercised whenever normalization raises these samples.
    return np.clip(mixed, -0.995, 0.995)


def _profile_for_unknown_batch(batch: str) -> BatchProfile:
    seed = stable_seed("batch-profile", batch)
    return BatchProfile(
        level=0.075 + (seed & 0xFF) / 255.0 * 0.045,
        drive=1.0 + ((seed >> 8) & 0xFF) / 255.0 * 2.0,
        noise_level=0.0025 + ((seed >> 16) & 0xFF) / 255.0 * 0.0025,
        transient_level=0.50 + ((seed >> 24) & 0xFF) / 255.0 * 0.40,
        brightness=0.85 + ((seed >> 32) & 0xFF) / 255.0 * 0.30,
    )


def _raised_cosine_gate(
    local_time: np.ndarray,
    length: float,
    *,
    attack: float,
    release: float,
) -> np.ndarray:
    attack = min(attack, length / 2.0)
    release = min(release, length / 2.0)
    envelope = np.ones_like(local_time)
    if attack > 0.0:
        mask = local_time < attack
        envelope[mask] = 0.5 - 0.5 * np.cos(
            np.pi * local_time[mask] / attack
        )
    if release > 0.0:
        mask = local_time > length - release
        envelope[mask] = 0.5 + 0.5 * np.cos(
            np.pi * (local_time[mask] - (length - release)) / release
        )
    return np.clip(envelope, 0.0, 1.0)


def math_sin(value: float) -> float:
    return float(np.sin(value))


def _peak_dbfs(samples: np.ndarray) -> float:
    peak = float(np.max(np.abs(samples))) if samples.size else 0.0
    if peak <= 0.0:
        return -120.0
    return max(-120.0, 20.0 * float(np.log10(peak)))


def _true_peak(samples: np.ndarray) -> float:
    from .dsp import true_peak_dbtp

    return true_peak_dbtp(samples)
