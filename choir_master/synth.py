from __future__ import annotations

import io
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .errors import MasterError
from .manifest import (
    VOICE_LABELS_ZH,
    VOICE_ORDER,
    LoadedAudio,
    ProjectManifest,
    load_manifest,
)
from .util import atomic_write_json, canonical_json_bytes, sha256_bytes, sha256_file


@dataclass(frozen=True)
class VoiceProfile:
    windows: tuple[tuple[float, float], ...]
    notes_hz: tuple[float, ...]
    formants_hz: tuple[float, ...]


VOICE_PROFILES: dict[str, VoiceProfile] = {
    "soprano": VoiceProfile(
        windows=(
            (0.25, 1.15),
            (1.45, 2.15),
            (2.55, 3.45),
            (3.85, 4.45),
            (4.75, 5.65),
            (5.95, 6.80),
            (7.00, 7.70),
        ),
        notes_hz=(659.25, 698.46, 783.99, 659.25, 880.00, 783.99, 659.25),
        formants_hz=(850.0, 1250.0, 2900.0),
    ),
    "alto": VoiceProfile(
        windows=(
            (0.45, 1.35),
            (1.65, 2.45),
            (2.80, 3.65),
            (4.00, 4.75),
            (5.05, 5.85),
            (6.15, 7.05),
            (7.30, 7.85),
        ),
        notes_hz=(440.00, 493.88, 523.25, 440.00, 392.00, 523.25, 440.00),
        formants_hz=(720.0, 1120.0, 2650.0),
    ),
    "tenor": VoiceProfile(
        windows=((0.45, 1.55), (1.95, 3.45), (3.85, 5.55), (5.95, 7.55)),
        notes_hz=(329.63, 349.23, 440.00, 329.63),
        formants_hz=(520.0, 980.0, 2550.0),
    ),
    "bass": VoiceProfile(
        windows=(
            (0.30, 1.00),
            (1.25, 2.10),
            (2.40, 3.20),
            (3.55, 4.35),
            (4.65, 5.45),
            (5.80, 6.60),
            (6.90, 7.65),
        ),
        notes_hz=(196.00, 220.00, 174.61, 196.00, 174.61, 220.00, 196.00),
        formants_hz=(430.0, 820.0, 2350.0),
    ),
}

DEFAULT_TRANSITION_EVENTS = ((2.10, 2.86), (4.16, 4.92))


def _raised_cosine_gate(
    length: int,
    sample_rate: int,
    start_s: float,
    end_s: float,
    ramp_s: float = 0.03,
) -> np.ndarray:
    gate = np.zeros(length, dtype=np.float64)
    start = max(0, int(round(start_s * sample_rate)))
    end = min(length, int(round(end_s * sample_rate)))
    if end <= start:
        return gate
    ramp = max(1, int(round(ramp_s * sample_rate)))
    n = end - start
    local = np.ones(n, dtype=np.float64)
    attack = min(ramp, n)
    release = min(ramp, n)
    local[:attack] = 0.5 - 0.5 * np.cos(np.linspace(0.0, np.pi, attack, endpoint=False))
    local[n - release :] = (
        0.5
        + 0.5
        * np.cos(np.linspace(0.0, np.pi, release, endpoint=False))
    )
    gate[start:end] = local
    return gate


def _formant_weight(frequency: float, formants: tuple[float, ...]) -> float:
    value = 0.24
    for index, center in enumerate(formants):
        width = (180.0, 260.0, 420.0)[min(index, 2)]
        strength = (1.0, 0.58, 0.34)[min(index, 2)]
        value += strength * math.exp(-0.5 * ((frequency - center) / width) ** 2)
    return value


def transition_high_gain(
    manifest: ProjectManifest,
    length: int,
    attenuation_db: float = 26.0,
    ramp_s: float = 0.03,
) -> np.ndarray:
    gain = np.ones(length, dtype=np.float64)
    floor = 10.0 ** (-attenuation_db / 20.0)
    for start_s, end_s in DEFAULT_TRANSITION_EVENTS:
        start = int(round(start_s * manifest.sample_rate))
        end = int(round(end_s * manifest.sample_rate))
        if start >= length:
            continue
        end = min(end, length)
        if end <= start:
            continue
        ramp = max(1, int(round(ramp_s * manifest.sample_rate)))
        segment = np.full(end - start, floor, dtype=np.float64)
        attack = min(ramp, end - start)
        release = min(ramp, end - start)
        segment[:attack] = 1.0 - (1.0 - floor) * (
            0.5 - 0.5 * np.cos(np.linspace(0.0, np.pi, attack, endpoint=False))
        )
        segment[end - start - release :] = floor + (1.0 - floor) * (
            0.5 - 0.5 * np.cos(np.linspace(0.0, np.pi, release, endpoint=False))
        )
        gain[start:end] = np.minimum(gain[start:end], segment)
    return gain


def synthesize_voice_tracks(
    manifest: ProjectManifest,
    voice_name: str,
    *,
    attenuation_db: float = 26.0,
) -> dict[str, np.ndarray]:
    """Recompute a voice and all of its microphone tracks from manifest parameters."""

    voice = manifest.voice(voice_name)
    profile = VOICE_PROFILES[voice_name]
    sample_rate = manifest.sample_rate
    length = manifest.duration_samples
    if length <= 0:
        raise ValueError("duration_samples must be positive")
    voice_index = VOICE_ORDER.index(voice_name)
    rng = np.random.default_rng(manifest.seed + voice_index * 100_003)

    time = np.arange(length, dtype=np.float64) / sample_rate
    f0 = np.zeros(length, dtype=np.float64)
    gate = np.zeros(length, dtype=np.float64)
    note_phase = np.zeros(length, dtype=np.float64)
    for note_index, ((start_s, end_s), note_hz) in enumerate(
        zip(profile.windows, profile.notes_hz)
    ):
        note_gate = _raised_cosine_gate(length, sample_rate, start_s, end_s)
        gate = np.maximum(gate, note_gate)
        start = int(round(start_s * sample_rate))
        end = min(length, int(round(end_s * sample_rate)))
        if end > start:
            f0[start:end] = note_hz
            note_phase[start:end] = float(note_index)

    vibrato_depth = 0.0026 + 0.0002 * voice_index
    vibrato_rate = 4.7 + 0.17 * voice_index
    f0 = np.where(
        f0 > 0,
        f0 * (1.0 + vibrato_depth * np.sin(2.0 * np.pi * vibrato_rate * time + 0.3 * voice_index)),
        profile.notes_hz[0],
    )
    phase = 2.0 * np.pi * np.cumsum(f0) / sample_rate
    amplitude = gate * (0.96 + 0.04 * np.sin(2.0 * np.pi * 0.61 * time + voice_index))

    low = np.zeros(length, dtype=np.float64)
    high = np.zeros(length, dtype=np.float64)
    max_harmonic = int((sample_rate * 0.46) / min(profile.notes_hz))
    for harmonic in range(1, max_harmonic + 1):
        harmonic_frequency = profile.notes_hz[0] * harmonic
        highest_note_frequency = max(profile.notes_hz) * harmonic
        harmonic_phase = float(rng.uniform(0.0, 2.0 * np.pi))
        formant = _formant_weight(harmonic_frequency, profile.formants_hz)
        harmonic_amplitude = formant / (harmonic ** 1.08)
        component = harmonic_amplitude * np.sin(harmonic * phase + harmonic_phase)
        # Split by the highest note in the profile so that a harmonic cannot
        # leak into the transition band on a higher note through the low-band
        # branch of this fixed split.
        if highest_note_frequency >= manifest.transition_band_hz[0]:
            high += component
        else:
            low += component

    source = amplitude * low
    if voice_name == "tenor":
        source = source + amplitude * high * transition_high_gain(
            manifest, length, attenuation_db=attenuation_db
        )
    else:
        source = source + amplitude * high

    peak = float(np.max(np.abs(source)))
    if peak <= 1e-12:
        raise MasterError("E_SYNTHESIS_FAILED", "Synthesized voice is silent.")
    source *= 0.36 / peak

    tracks: dict[str, np.ndarray] = {}
    for track in voice.tracks:
        distance = track.position.distance_m
        azimuth = abs(track.position.azimuth_deg)
        delay_seconds = distance / 343.0 + azimuth * 2.0e-6
        delay = int(round(delay_seconds * sample_rate))
        gain = 1.0 / (1.0 + 0.30 * distance) * (1.0 - 0.0012 * azimuth)
        delayed = np.zeros(length, dtype=np.float64)
        if delay < length:
            delayed[delay:] = source[: length - delay] * gain
        tracks[track.track_id] = delayed
    return tracks


def generate_audio_from_manifest(
    manifest: ProjectManifest,
    *,
    force: bool = False,
) -> dict[str, Any]:
    expected_subtype = {16: "PCM_16", 24: "PCM_24", 32: "PCM_32"}[manifest.bit_depth]
    written: dict[str, str] = {}
    for voice in manifest.voices:
        generated = synthesize_voice_tracks(manifest, voice.voice)
        for track in voice.tracks:
            track.path.parent.mkdir(parents=True, exist_ok=True)
            if track.path.exists() and not force:
                raise MasterError(
                    "E_OUTPUT_EXISTS",
                    "Audio output already exists; use --force to overwrite.",
                    path=str(track.path),
                )
            sf.write(
                str(track.path),
                generated[track.track_id],
                manifest.sample_rate,
                subtype=expected_subtype,
                format="WAV",
            )
            written[track.track_id] = sha256_file(track.path)
    return {"status": "generated", "track_sha256": written}


def _pcm24_roundtrip(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    buffer = io.BytesIO()
    sf.write(buffer, samples, sample_rate, subtype="PCM_24", format="WAV")
    buffer.seek(0)
    data, _ = sf.read(buffer, dtype="float64", always_2d=False)
    return np.asarray(data, dtype=np.float64)


def verify_reference_tracks(manifest_path: str | Path) -> dict[str, Any]:
    manifest = load_manifest(manifest_path)
    results: dict[str, Any] = {}
    all_match = True
    for voice in manifest.voices:
        reference = next(track for track in voice.tracks if track.reference)
        expected = synthesize_voice_tracks(manifest, voice.voice)[reference.track_id]
        expected = _pcm24_roundtrip(expected, manifest.sample_rate)
        actual, _ = sf.read(str(reference.path), dtype="float64", always_2d=False)
        actual = np.asarray(actual, dtype=np.float64)
        match = actual.shape == expected.shape and np.array_equal(actual, expected)
        all_match = all_match and bool(match)
        results[voice.voice] = {
            "reference_track": reference.track_id,
            "path": str(reference.path),
            "samples": int(len(actual)),
            "sha256": sha256_file(reference.path),
            "independent_recompute_match": bool(match),
        }
    return {
        "status": "ok" if all_match else "failed",
        "all_reference_tracks_match": bool(all_match),
        "voices": results,
    }


def transition_truth(manifest: ProjectManifest) -> dict[str, Any]:
    return {
        "voice": "tenor",
        "band_hz": list(manifest.transition_band_hz),
        "attenuation_db": 26.0,
        "intervals": [
            {
                "voice": "tenor",
                "start_s": float(start),
                "end_s": float(end),
            }
            for start, end in DEFAULT_TRANSITION_EVENTS
        ],
    }


def default_manifest(
    *,
    seed: int = 2532026,
    sample_rate: int = 48_000,
    bit_depth: int = 24,
    duration_samples: int = 384_000,
    target_loudness_lufs: float = -16.0,
    true_peak_limit_dbtp: float = -1.0,
    block_size: int = 8192,
) -> dict[str, Any]:
    definitions = {
        "soprano": {
            "ratio": 0.26,
            "weights": [0.82, 0.57],
            "mics": ((1.10, -25.0), (1.55, 20.0)),
        },
        "alto": {
            "ratio": 0.31,
            "weights": [0.72, 0.69],
            "mics": ((1.95, -18.0), (2.40, 22.0)),
        },
        "tenor": {
            "ratio": 0.24,
            "weights": [0.65, 0.76],
            "mics": ((1.30, -22.0), (1.12, 28.0)),
        },
        "bass": {
            "ratio": 0.19,
            "weights": [0.54, 0.84],
            "mics": ((1.70, -20.0), (2.05, 18.0)),
        },
    }
    voices = []
    for name in VOICE_ORDER:
        definition = definitions[name]
        track_base = f"audio/{name}"
        voices.append(
            {
                "voice": name,
                "label_zh": VOICE_LABELS_ZH[name],
                "target_energy_ratio": definition["ratio"],
                "downmix_weights": definition["weights"],
                "tracks": [
                    {
                        "id": f"{name}_m{mic_index + 1}",
                        "path": f"{track_base}_m{mic_index + 1}.wav",
                        "mic_position": {
                            "distance_m": distance,
                            "azimuth_deg": azimuth,
                        },
                        "reference": mic_index == 0,
                    }
                    for mic_index, (distance, azimuth) in enumerate(definition["mics"])
                ],
            }
        )
    return {
        "schema_version": 1,
        "project": "deterministic-choir-master-demo",
        "seed": int(seed),
        "sample_rate": int(sample_rate),
        "bit_depth": int(bit_depth),
        "channels": 1,
        "duration_samples": int(duration_samples),
        "transition_band_hz": [1200.0, 3000.0],
        "passaggio_hz": 329.63,
        "target_loudness_lufs": float(target_loudness_lufs),
        "true_peak_limit_dbtp": float(true_peak_limit_dbtp),
        "block_size": int(block_size),
        "voices": voices,
    }


def create_demo_project(
    directory: str | Path,
    *,
    force: bool = True,
    **manifest_overrides: Any,
) -> dict[str, Any]:
    output_dir = Path(directory).expanduser().resolve()
    output_dir.mkdir(parents=True, exist_ok=True)
    manifest_path = output_dir / "project.json"
    raw = default_manifest(**manifest_overrides)
    atomic_write_json(manifest_path, raw)
    manifest = load_manifest(manifest_path)
    generation = generate_audio_from_manifest(manifest, force=force)
    truth = transition_truth(manifest)
    truth["manifest_digest"] = sha256_bytes(canonical_json_bytes(raw))
    truth_path = output_dir / "truth.json"
    atomic_write_json(truth_path, truth)
    verification = verify_reference_tracks(manifest_path)
    report = {
        "status": "ok" if verification["all_reference_tracks_match"] else "failed",
        "manifest": str(manifest_path),
        "truth": str(truth_path),
        "manifest_digest": manifest.digest,
        "generation": generation,
        "reference_verification": verification,
    }
    atomic_write_json(output_dir / "generation_report.json", report)
    return report


def load_generated_truth(manifest_path: str | Path) -> dict[str, Any]:
    manifest_file = Path(manifest_path).expanduser().resolve()
    truth_path = manifest_file.parent / "truth.json"
    import json

    with truth_path.open("r", encoding="utf-8") as handle:
        return json.load(handle)

