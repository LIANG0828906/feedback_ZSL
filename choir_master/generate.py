"""Deterministic synthetic choir material and default manifest factory."""

from __future__ import annotations

import json
import math
import os
from pathlib import Path
from typing import Any

import numpy as np

from .manifest import Project, VoiceSpec, load_project, manifest_digest
from .pcm import write_wav


VOICE_SLUGS = {
    "女高": "sop",
    "女低": "alto",
    "男高": "ten",
    "男低": "bass",
}

DEFAULT_SYNTHESIS = {
    "女高": {"f0_hz": 523.25, "harmonics": 12, "formant_hz": 900.0, "level": 0.16},
    "女低": {"f0_hz": 392.00, "harmonics": 13, "formant_hz": 700.0, "level": 0.17},
    "男高": {"f0_hz": 329.63, "harmonics": 15, "formant_hz": 650.0, "level": 0.18},
    "男低": {"f0_hz": 196.00, "harmonics": 14, "formant_hz": 380.0, "level": 0.20},
}


def _atomic_write_text(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_name(f".{path.name}.tmp-{os.getpid()}")
    temp.write_text(text, encoding="utf-8", newline="\n")
    os.replace(temp, path)


def build_default_manifest(
    *,
    seed: int = 235,
    duration_seconds: float = 8.0,
    block_frames: int = 24000,
    sample_rate: int = 48000,
    bit_depth: int = 24,
    target_loudness_lufs: float = -20.0,
    true_peak_limit_dbtp: float = -1.0,
) -> dict[str, Any]:
    ratios = {"女高": 0.27, "女低": 0.25, "男高": 0.26, "男低": 0.22}
    pans = {"女高": -0.35, "女低": 0.15, "男高": -0.15, "男低": 0.35}
    bands = {
        "女高": [500.0, 2500.0],
        "女低": [330.0, 1600.0],
        "男高": [400.0, 1600.0],
        "男低": [150.0, 900.0],
    }
    microphones: list[dict[str, Any]] = []
    for voice_index, voice in enumerate(("女高", "女低", "男高", "男低")):
        for mic_index, suffix in enumerate(("a", "b")):
            name = f"{VOICE_SLUGS[voice]}_{suffix}"
            delay = 0.0 if mic_index == 0 else 0.42 + voice_index * 0.07
            gain = 0.0 if mic_index == 0 else -3.0 - voice_index * 0.25
            microphones.append(
                {
                    "name": name,
                    "voice": voice,
                    "path": f"audio/{name}.wav",
                    "channels": 1,
                    "downmix_weight": 0.5,
                    "position": {
                        "delay_ms": delay,
                        "gain_db": gain,
                    },
                }
            )
    voices = [
        {
            "name": voice,
            "target_energy_ratio": ratios[voice],
            "breakpoint_band_hz": bands[voice],
            "pan": pans[voice],
            "synthesis": dict(DEFAULT_SYNTHESIS[voice]),
        }
        for voice in ("女高", "女低", "男高", "男低")
    ]
    mutation = mutation_window(seed, duration_seconds)
    return {
        "schema_version": 1,
        "project": "deterministic_choir_master",
        "generator_version": "1.0",
        "seed": int(seed),
        "sample_rate": int(sample_rate),
        "bit_depth": int(bit_depth),
        "duration_seconds": float(duration_seconds),
        "block_frames": int(block_frames),
        "target_loudness_lufs": float(target_loudness_lufs),
        "true_peak_limit_dbtp": float(true_peak_limit_dbtp),
        "voices": voices,
        "microphones": microphones,
        "generation": {
            "mutation_voice": "男高",
            "mutation_intervals_s": [[round(mutation[0], 9), round(mutation[1], 9)]],
            "mutation_band_hz": bands["男高"],
            "mutation_gain_db": -18.0,
            "phrase_envelope_segments": phrase_segments(duration_seconds),
            "phrase_fade_seconds": 0.025,
        },
    }


def phrase_segments(duration: float) -> list[list[float]]:
    fractions = [
        (0.020, 0.130),
        (0.156, 0.256),
        (0.275, 0.494),
        (0.519, 0.631),
        (0.656, 0.819),
        (0.844, 0.975),
    ]
    return [[round(a * duration, 9), round(b * duration, 9)] for a, b in fractions]


def mutation_window(seed: int, duration: float) -> tuple[float, float]:
    start = (0.320 + float(seed % 50) / 1000.0) * duration
    end = start + 0.100 * duration
    return start, end


def _phrase_envelope(t: np.ndarray, duration: float) -> np.ndarray:
    result = np.zeros_like(t)
    fade = 0.025
    for start, end in phrase_segments(duration):
        rise = np.clip((t - start) / fade, 0.0, 1.0)
        fall = np.clip((end - t) / fade, 0.0, 1.0)
        gate = (0.5 - 0.5 * np.cos(np.pi * rise)) * (
            0.5 - 0.5 * np.cos(np.pi * fall)
        )
        result = np.maximum(result, gate)
    return result


def _mutation_alpha(t: np.ndarray, start: float, end: float, fade: float = 0.02) -> np.ndarray:
    rise = np.clip((t - start) / fade, 0.0, 1.0)
    fall = np.clip((end - t) / fade, 0.0, 1.0)
    smooth_rise = 0.5 - 0.5 * np.cos(np.pi * rise)
    smooth_fall = 0.5 - 0.5 * np.cos(np.pi * fall)
    return np.minimum(smooth_rise, smooth_fall)


def synthesize_voice_source(
    voice: VoiceSpec,
    project: Project,
    *,
    mutation_interval: tuple[float, float] | None = None,
    mutation_gain_db: float = -18.0,
) -> np.ndarray:
    """Synthesize the shared source for one voice.

    All randomness is expressed as integer functions of the manifest seed; no
    global or time-seeded RNG is consulted.
    """
    sample_rate = project.sample_rate
    frames = int(round(project.duration_seconds * sample_rate))
    if frames <= 0:
        return np.zeros(0, dtype=np.float64)
    t = np.arange(frames, dtype=np.float64) / sample_rate
    config = DEFAULT_SYNTHESIS[voice.name]
    settings = {**config, **voice.synthesis}
    f0 = float(settings["f0_hz"])
    harmonics = int(settings["harmonics"])
    formant = float(settings["formant_hz"])
    level = float(settings["level"])
    voice_index = ("女高", "女低", "男高", "男低").index(voice.name)
    seed_phase = math.pi * float((project.seed + voice_index * 37) % 720) / 360.0
    vibrato_phase = math.pi * float((project.seed * 3 + voice_index * 19) % 360) / 180.0
    vibrato = 0.0035 * np.sin(2.0 * np.pi * 4.7 * t + vibrato_phase)

    source = np.zeros(frames, dtype=np.float64)
    alpha = np.zeros(frames, dtype=np.float64)
    if mutation_interval is not None:
        alpha = _mutation_alpha(t, mutation_interval[0], mutation_interval[1])
        attenuation = 10.0 ** (mutation_gain_db / 20.0)
    else:
        attenuation = 1.0

    for k in range(1, harmonics + 1):
        frequency = k * f0
        if frequency >= sample_rate / 2:
            break
        formant_weight = 1.0 + 0.55 * math.exp(-((frequency - formant) / (0.55 * formant)) ** 2)
        amplitude = formant_weight / (k ** 0.88)
        phase = seed_phase * k + 0.17 * k * k
        instantaneous_phase = 2.0 * np.pi * k * f0 * t + k * vibrato + phase
        harmonic = amplitude * np.sin(instantaneous_phase)
        if (
            mutation_interval is not None
            and voice.band_low_hz <= frequency <= voice.band_high_hz
        ):
            harmonic *= 1.0 + alpha * (attenuation - 1.0)
        source += harmonic

    envelope = _phrase_envelope(t, project.duration_seconds)
    source *= envelope
    active = envelope > 0.2
    if np.any(active):
        current_rms = float(np.sqrt(np.mean(source[active] ** 2)))
        if current_rms > 0:
            source = source * (level / current_rms)
    return source


def generate_project_files(
    manifest_path: str | Path,
    *,
    check_files: bool = False,
) -> dict[str, Any]:
    project = load_project(manifest_path, check_files=check_files)
    tenor_truth: tuple[float, float] | None = None
    mutation_gain_db = -18.0
    generation = project.manifest.get("generation", {})
    if isinstance(generation, dict):
        gain = generation.get("mutation_gain_db")
        if isinstance(gain, (int, float)) and math.isfinite(float(gain)):
            mutation_gain_db = float(gain)
    # The integer seed is the source of truth for synthesis timing. The
    # manifest's generation block is a reproducible description written by
    # init, not a hidden control channel for the detector.
    tenor_truth = mutation_window(project.seed, project.duration_seconds)

    voice_sources: dict[str, np.ndarray] = {}
    for voice_name in ("女高", "女低", "男高", "男低"):
        voice = project.voices[voice_name]
        mutation = tenor_truth if voice_name == "男高" else None
        voice_sources[voice_name] = synthesize_voice_source(
            voice,
            project,
            mutation_interval=mutation,
            mutation_gain_db=mutation_gain_db,
        )

    energy: dict[str, dict[str, float]] = {}
    written: list[str] = []
    for mic in sorted(project.microphones, key=lambda item: item.name):
        source = voice_sources[mic.voice]
        frames = source.size
        delay_samples = int(round(mic.delay_ms * project.sample_rate / 1000.0))
        if delay_samples >= frames:
            positioned = np.zeros(frames, dtype=np.float64)
        else:
            gain = 10.0 ** (mic.gain_db / 20.0)
            positioned = np.zeros(frames, dtype=np.float64)
            positioned[delay_samples:] = gain * source[: frames - delay_samples]
        write_wav(
            mic.resolved_path,
            positioned[:, None],
            sample_rate=project.sample_rate,
            bit_depth=project.bit_depth,
        )
        written.append(mic.relative_path)
        voice_stats = energy.setdefault(mic.voice, {"energy": 0.0, "frames": 0.0})
        voice_stats["energy"] += float(np.sum(positioned**2))
        voice_stats["frames"] += float(frames)

    voice_energy = {
        name: stats["energy"] / max(1.0, stats["frames"])
        for name, stats in energy.items()
    }
    return {
        "manifest": str(project.manifest_path.name),
        "manifest_sha256": manifest_digest(project.manifest),
        "seed": project.seed,
        "files_written": len(written),
        "mutation_truth_s": [round(tenor_truth[0], 9), round(tenor_truth[1], 9)],
        "voice_mean_square": {
            name: round(float(voice_energy[name]), 9)
            for name in ("女高", "女低", "男高", "男低")
        },
    }


def init_workspace(
    workspace: str | Path,
    *,
    seed: int = 235,
    duration_seconds: float = 8.0,
    block_frames: int = 24000,
    target_loudness_lufs: float = -20.0,
    true_peak_limit_dbtp: float = -1.0,
    bit_depth: int = 24,
) -> Path:
    root = Path(workspace).expanduser().resolve()
    root.mkdir(parents=True, exist_ok=True)
    manifest = build_default_manifest(
        seed=seed,
        duration_seconds=duration_seconds,
        block_frames=block_frames,
        target_loudness_lufs=target_loudness_lufs,
        true_peak_limit_dbtp=true_peak_limit_dbtp,
        bit_depth=bit_depth,
    )
    manifest_path = root / "project.json"
    _atomic_write_text(
        manifest_path,
        json.dumps(manifest, ensure_ascii=False, indent=2) + "\n",
    )
    generate_project_files(manifest_path, check_files=False)
    load_project(manifest_path, check_files=True)
    return manifest_path

