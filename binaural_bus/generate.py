"""Deterministic multitrack material generation from a validated project."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .project import Project, ReferenceWindow, resolve_channel_path
from .wavio import write_wav


@dataclass(frozen=True)
class GeneratedMaterial:
    actor_direct: dict[str, np.ndarray]
    actor_mics: dict[str, np.ndarray]
    speakers: dict[str, np.ndarray]
    injected_paths: dict[tuple[str, str], dict[str, float]]
    manifest: dict[str, Any]


def stable_seed(*parts: Any) -> int:
    text = "\x1f".join(str(part) for part in parts)
    digest = hashlib.sha256(text.encode("utf-8")).digest()
    return int.from_bytes(digest[:8], "little", signed=False)


def generate_material(project: Project) -> GeneratedMaterial:
    """Create all source tracks before quantization.

    The returned arrays are channel-first float64 and exactly project.frames long.
    """

    n = project.frames
    fs = project.sample_rate
    t = np.arange(n, dtype=np.float64) / fs

    reference_by_actor: dict[str, list[ReferenceWindow]] = {}
    for window in project.crosstalk.reference_windows:
        if window.actor_id is None:
            for actor in project.actors:
                reference_by_actor.setdefault(actor.id, []).append(window)
        else:
            reference_by_actor.setdefault(window.actor_id, []).append(window)

    probe_by_actor: dict[str, list[ReferenceWindow]] = {}
    for window in project.crosstalk.direct_probe_windows:
        if window.actor_id is None:
            for actor in project.actors:
                probe_by_actor.setdefault(actor.id, []).append(window)
        else:
            probe_by_actor.setdefault(window.actor_id, []).append(window)

    speakers: dict[str, np.ndarray] = {}
    for speaker in project.speakers:
        source = _speaker_program(project, speaker.id, speaker.azimuth_deg, speaker.elevation_deg)
        speaker_probe_windows: list[ReferenceWindow] = []
        for actor in project.actors:
            speaker_probe_windows.extend(probe_by_actor.get(actor.id, []))
        if speaker_probe_windows:
            mask = _probe_silence_mask(
                speaker_probe_windows,
                n,
                fs,
                pad_seconds=project.crosstalk.max_delay_ms / 1000.0 + 0.008,
            )
            source *= mask
        speakers[speaker.id] = source

    actor_direct: dict[str, np.ndarray] = {}
    actor_mics: dict[str, np.ndarray] = {}
    injected_paths: dict[tuple[str, str], dict[str, float]] = {}

    feedback_by_actor: dict[str, list[Any]] = {}
    for path in project.crosstalk.feedback:
        feedback_by_actor.setdefault(path.actor_id, []).append(path)

    for actor in project.actors:
        direct = _actor_program(project, actor.id, t)
        windows = reference_by_actor.get(actor.id, [])
        if windows:
            direct *= _direct_silence_mask(windows, n, fs)
        actor_direct[actor.id] = direct

        mic = direct.copy()
        for path in feedback_by_actor.get(actor.id, []):
            feedback = project.speaker_by_id(path.speaker_id)
            delay_samples = int(round(path.delay_ms * fs / 1000.0))
            source = speakers[path.speaker_id]
            if delay_samples >= n:
                delayed = np.zeros_like(source)
            elif delay_samples == 0:
                delayed = source.copy()
            else:
                delayed = np.zeros_like(source)
                delayed[delay_samples:] = source[:-delay_samples]
            gain_linear = 10.0 ** (path.gain_db / 20.0)
            mic += gain_linear * delayed
            injected_paths[(actor.id, path.speaker_id)] = {
                "gain_db": path.gain_db,
                "gain_linear": gain_linear,
                "delay_samples": float(delay_samples),
                "delay_ms": 1000.0 * delay_samples / fs,
                "speaker_azimuth_deg": feedback.azimuth_deg,
                "speaker_elevation_deg": feedback.elevation_deg,
            }
        peak = float(np.max(np.abs(mic))) if mic.size else 0.0
        if peak > 0.95:
            # Keep the declared crosstalk gain-to-direct ratio intact by scaling
            # both components, not by changing the injected gain.
            scale = 0.95 / peak
            direct *= scale
            mic *= scale
            actor_direct[actor.id] = direct
            for key in injected_paths:
                if key[0] == actor.id:
                    injected_paths[key]["global_scale"] = scale
        actor_mics[actor.id] = mic

    manifest = {
        "schema_version": 1,
        "project_id": project.project_id,
        "project_digest": project.content_digest(),
        "sample_rate": project.sample_rate,
        "bit_depth": project.bit_depth,
        "frames": project.frames,
        "seed": project.seed,
        "actors": [
            {
                "id": actor.id,
                "channel": Path(actor.channel).name,
                "azimuth_deg": actor.azimuth_deg,
                "elevation_deg": actor.elevation_deg,
                "truth": _stem(actor.channel) + ".direct.wav",
            }
            for actor in project.actors
        ],
        "speakers": [
            {
                "id": speaker.id,
                "channel": Path(speaker.channel).name,
                "azimuth_deg": speaker.azimuth_deg,
                "elevation_deg": speaker.elevation_deg,
            }
            for speaker in project.speakers
        ],
        "crosstalk_paths": [
            {
                "actor_id": actor_id,
                "speaker_id": speaker_id,
                **values,
            }
            for (actor_id, speaker_id), values in sorted(injected_paths.items())
        ],
    }
    return GeneratedMaterial(actor_direct, actor_mics, speakers, injected_paths, manifest)


def write_generated_material(
    project: Project,
    output_dir: str | Path,
    *,
    write_truth: bool = True,
) -> GeneratedMaterial:
    """Generate, quantize, and write deterministic WAV inputs."""

    output_dir = Path(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    material = generate_material(project)

    for actor in project.actors:
        samples = material.actor_mics[actor.id].reshape(1, -1)
        write_wav(
            resolve_channel_path(output_dir, actor.channel),
            samples,
            project.sample_rate,
            project.bit_depth,
        )
    for speaker in project.speakers:
        samples = material.speakers[speaker.id].reshape(1, -1)
        write_wav(
            resolve_channel_path(output_dir, speaker.channel),
            samples,
            project.sample_rate,
            project.bit_depth,
        )
    if write_truth:
        truth_dir = output_dir / "truth"
        truth_dir.mkdir(parents=True, exist_ok=True)
        for actor in project.actors:
            write_wav(
                truth_dir / (_stem(actor.channel) + ".direct.wav"),
                material.actor_direct[actor.id].reshape(1, -1),
                project.sample_rate,
                project.bit_depth,
            )

    manifest_path = output_dir / "generation_manifest.json"
    manifest_path.write_text(
        json.dumps(material.manifest, ensure_ascii=True, indent=2) + "\n",
        encoding="utf-8",
    )
    return material


def _actor_program(project: Project, actor_id: str, t: np.ndarray) -> np.ndarray:
    rng = np.random.default_rng(stable_seed(project.seed, actor_id, "actor-direct-v1"))
    n = t.size
    fs = project.sample_rate
    f0_base = 105.0 + 65.0 * rng.random()
    vibrato_hz = 4.2 + 1.8 * rng.random()
    vibrato_depth = 0.008 + 0.012 * rng.random()
    harmonic_count = 8
    harmonic_weights = np.array(
        [1.0, 0.52, 0.35, 0.24, 0.18, 0.13, 0.09, 0.06], dtype=np.float64
    )
    weights = harmonic_weights.copy()
    weights += rng.uniform(-0.02, 0.02, size=harmonic_count)
    weights = np.maximum(weights, 0.015)

    f0 = f0_base * (1.0 + vibrato_depth * np.sin(2.0 * np.pi * vibrato_hz * t))
    phase = 2.0 * np.pi * np.cumsum(f0) / fs
    vowel = np.zeros(n, dtype=np.float64)
    for index, weight in enumerate(weights, start=1):
        vowel += weight * np.sin(index * phase + rng.uniform(0.0, 2.0 * np.pi))
    vowel *= 0.22 / np.sum(weights)

    envelope = np.zeros(n, dtype=np.float64)
    plosive = np.zeros(n, dtype=np.float64)
    period = 1.18
    cursor = 0.08 + 0.10 * rng.random()
    while cursor < t[-1] if n else False:
        for syllable_index in range(2):
            onset = cursor + 0.46 * syllable_index
            if onset >= n / fs:
                break
            burst_len = min(int(round(0.024 * fs)), n - int(round(onset * fs)))
            if burst_len > 0:
                start = int(round(onset * fs))
                local = np.arange(burst_len) / fs
                burst_env = np.exp(-local / 0.007) * (1.0 - np.exp(-local / 0.0008))
                noise = rng.standard_normal(burst_len)
                kernel = np.ones(5, dtype=np.float64) / 5.0
                noise = np.convolve(noise, kernel, mode="same")
                plosive[start : start + burst_len] += 0.26 * burst_env * noise

            vowel_start = onset + 0.012
            vowel_len = min(int(round(0.31 * fs)), max(0, n - int(round(vowel_start * fs))))
            if vowel_len > 0:
                start = int(round(vowel_start * fs))
                local = np.arange(vowel_len)
                attack = np.minimum(local / max(1, int(0.025 * fs)), 1.0)
                release = np.minimum(
                    (vowel_len - 1 - local) / max(1, int(0.055 * fs)), 1.0
                )
                shape = np.minimum(attack, release)
                shape = 0.5 - 0.5 * np.cos(np.pi * np.clip(shape, 0.0, 1.0))
                envelope[start : start + vowel_len] = np.maximum(
                    envelope[start : start + vowel_len], shape
                )
        cursor += period

    direct = envelope * vowel + plosive
    direct = _smooth_limit(direct, 0.88)
    return direct


def _speaker_program(
    project: Project,
    speaker_id: str,
    azimuth_deg: float,
    elevation_deg: float,
) -> np.ndarray:
    rng = np.random.default_rng(
        stable_seed(
            project.seed,
            speaker_id,
            f"{azimuth_deg:.9f}",
            f"{elevation_deg:.9f}",
            "speaker-return-v1",
        )
    )
    n = project.frames
    fs = project.sample_rate
    t = np.arange(n, dtype=np.float64) / fs

    noise = rng.standard_normal(n)
    # A short deterministic FIR gives the return a speech-like low-frequency tilt
    # while retaining enough broadband content for unambiguous delay estimation.
    kernel = np.array([0.08, 0.14, 0.20, 0.24, 0.20, 0.14], dtype=np.float64)
    noise = np.convolve(noise, kernel, mode="same")
    noise *= 0.14 / max(1e-12, float(np.std(noise)))

    f0 = 135.0 + 95.0 * rng.random()
    phase = 2.0 * np.pi * f0 * t + rng.uniform(0.0, 2.0 * np.pi)
    tone = (
        0.055 * np.sin(phase)
        + 0.026 * np.sin(2.03 * phase + 0.3)
        + 0.012 * np.sin(3.07 * phase + 1.1)
    )

    envelope = 0.45 + 0.55 * (
        0.5 + 0.5 * np.sin(2.0 * np.pi * (0.83 + 0.11 * rng.random()) * t + rng.random() * 6.28)
    )
    envelope *= 0.75 + 0.25 * np.sin(2.0 * np.pi * 3.7 * t + rng.random() * 6.28)
    source = envelope * (noise + tone)
    return _smooth_limit(source, 0.90)


def _direct_silence_mask(
    windows: list[ReferenceWindow], n: int, fs: int
) -> np.ndarray:
    mask = np.ones(n, dtype=np.float64)
    fade = max(1, int(round(0.004 * fs)))
    for window in windows:
        start = max(0, int(math.floor(window.start_seconds * fs)))
        end = min(n, int(math.ceil(window.end_seconds * fs)))
        mask[start:end] = 0.0
        for index in range(1, min(fade, start) + 1):
            value = 0.5 - 0.5 * math.cos(math.pi * index / fade)
            mask[start - index] = min(mask[start - index], value)
        for index in range(1, min(fade, n - end) + 1):
            value = 0.5 - 0.5 * math.cos(math.pi * index / fade)
            mask[end - 1 + index] = min(mask[end - 1 + index], value)
    return mask


def _probe_silence_mask(
    windows: list[ReferenceWindow], n: int, fs: int, *, pad_seconds: float
) -> np.ndarray:
    mask = np.ones(n, dtype=np.float64)
    fade = max(1, int(round(0.004 * fs)))
    pad = max(0, int(round(pad_seconds * fs)))
    for window in windows:
        start = max(0, int(math.floor(window.start_seconds * fs)) - pad)
        end = min(n, int(math.ceil(window.end_seconds * fs)))
        mask[start:end] = 0.0
        for index in range(1, min(fade, start) + 1):
            value = 0.5 - 0.5 * math.cos(math.pi * index / fade)
            mask[start - index] = min(mask[start - index], value)
        for index in range(1, min(fade, n - end) + 1):
            value = 0.5 - 0.5 * math.cos(math.pi * index / fade)
            mask[end - 1 + index] = min(mask[end - 1 + index], value)
    return mask


def _smooth_limit(signal: np.ndarray, ceiling: float) -> np.ndarray:
    peak = float(np.max(np.abs(signal))) if signal.size else 0.0
    if peak > ceiling:
        signal = signal * (ceiling / peak)
    return signal


def _stem(channel: str) -> str:
    return Path(channel).stem

