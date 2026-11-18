from __future__ import annotations

import math
from pathlib import Path
from typing import Any

import numpy as np

from .errors import BusinessError, BusinessFailure, EXIT_VALIDATION
from .manifest import Manifest, TrackSpec, load_manifest
from .util import atomic_write_json, sample_round
from .wavfile import write_wav


def _seed_for(seed: int, track_index: int, salt: int) -> np.random.Generator:
    sequence = np.random.SeedSequence([int(seed), int(track_index), int(salt)])
    return np.random.Generator(np.random.PCG64(sequence))


def _channelize(mono: np.ndarray, channels: int, pan: float) -> np.ndarray:
    if channels == 1:
        return mono[:, None]
    angle = (float(np.clip(pan, -1.0, 1.0)) + 1.0) * math.pi / 4.0
    return np.stack((mono * math.cos(angle), mono * math.sin(angle),), axis=1)


def _formant_weight(frequency: float, center: float, width: float) -> float:
    return math.exp(-0.5 * ((frequency - center) / width) ** 2)


def _add_plosive(
    destination: np.ndarray,
    onset: int,
    sample_rate: int,
    gain: float,
    rng: np.random.Generator,
) -> None:
    length = min(destination.shape[0] - onset, int(round(0.011 * sample_rate)))
    if length <= 0:
        raise ValueError("plosive does not fit in track")
    time = np.arange(length, dtype=np.float64) / sample_rate
    envelope = np.exp(-time / 0.0022)
    noise = rng.standard_normal(length)
    bright = np.diff(noise, prepend=noise[0])
    thump = np.sin(2.0 * math.pi * 82.0 * time)
    burst = (0.50 * bright + 0.32 * noise + 0.38 * thump) * envelope
    peak = float(np.max(np.abs(burst)))
    if peak <= 0:
        burst[0] = 1.0
        peak = 1.0
    burst = burst / peak * gain
    # A prominent first sample gives an unambiguous sample-accurate transient anchor.
    if abs(burst[0]) < gain * 0.95:
        burst[0] = math.copysign(gain * 0.98, burst[0] or 1.0)
    destination[onset : onset + length] += burst


def _add_vowel(
    destination: np.ndarray,
    onset: int,
    sample_rate: int,
    gain: float,
    fundamental: float,
    rng: np.random.Generator,
) -> None:
    length = min(destination.shape[0] - onset, int(round(0.225 * sample_rate)))
    if length <= 0:
        raise ValueError("vowel does not fit in track")
    time = np.arange(length, dtype=np.float64) / sample_rate
    attack = min(length, int(round(0.012 * sample_rate)))
    envelope = np.ones(length, dtype=np.float64)
    if attack > 0:
        phase = np.arange(attack, dtype=np.float64) / max(attack - 1, 1)
        envelope[:attack] = 0.5 * (1.0 - np.cos(math.pi * phase))
    decay_from = max(attack, int(round(0.070 * sample_rate)))
    if decay_from < length:
        tail = np.arange(length - decay_from, dtype=np.float64)
        envelope[decay_from:] *= np.exp(-tail / (0.075 * sample_rate))

    signal = np.zeros(length, dtype=np.float64)
    phases = rng.uniform(0.0, 2.0 * math.pi, size=10)
    harmonic_count = min(10, int((sample_rate / 2.0 - 100.0) // fundamental))
    energy = 0.0
    for harmonic in range(1, harmonic_count + 1):
        frequency = fundamental * harmonic
        weight = 1.0 / harmonic**1.15
        weight *= 0.45 + _formant_weight(frequency, 720.0, 190.0) + 0.55 * _formant_weight(
            frequency, 1250.0, 260.0
        )
        weight *= 0.80 + 0.20 * math.sin(2.0 * math.pi * 3.2 * harmonic / 10.0)
        signal += weight * np.sin(2.0 * math.pi * frequency * time + phases[harmonic - 1])
        energy += weight * weight
    signal /= math.sqrt(max(energy, 1e-12))
    signal *= envelope * gain
    destination[onset : onset + length] += signal


def _synthesize_dialogue(track: TrackSpec, manifest: Manifest, track_index: int) -> np.ndarray:
    sample_rate = manifest.sample_rate
    duration = float(track.synth["local_duration_seconds"])
    frames = sample_round(duration, sample_rate)
    if frames <= 0:
        raise ValueError("dialogue duration produced no samples")
    rng = _seed_for(manifest.seed, track_index, 1)
    mono = np.zeros(frames, dtype=np.float64)
    jitter = int(track.synth.get("jitter_samples", 0))
    burst_gain = float(track.synth.get("burst_gain", 0.50))
    vowel_gain = float(track.synth.get("vowel_gain", 0.25))
    fundamental = float(track.synth.get("f0_hz", 145.0))
    cue_indices = list(track.synth["cue_indices"])

    for cue_index in cue_indices:
        cue_sample = manifest.dialogue_points_samples[cue_index]
        local_onset = cue_sample - track.offset_samples + jitter
        if local_onset < 0 or local_onset >= frames:
            raise ValueError(f"dialogue cue {cue_index} falls outside local track")
        _add_plosive(mono, local_onset, sample_rate, burst_gain, rng)
        vowel_onset = local_onset + max(1, sample_round(0.004, sample_rate))
        if vowel_onset < frames:
            _add_vowel(mono, vowel_onset, sample_rate, vowel_gain, fundamental, rng)

    mono *= float(track.synth.get("gain", 1.0))
    if np.max(np.abs(mono)) >= 1.0:
        mono /= np.max(np.abs(mono)) / 0.95
    return _channelize(mono, track.channels, float(track.synth.get("pan", 0.0)))


def _synthesize_narration(track: TrackSpec, manifest: Manifest, track_index: int) -> np.ndarray:
    sample_rate = manifest.sample_rate
    duration = float(track.synth["local_duration_seconds"])
    frames = sample_round(duration, sample_rate)
    if frames <= 0:
        raise ValueError("narration duration produced no samples")
    rng = _seed_for(manifest.seed, track_index, 2)
    mono = np.zeros(frames, dtype=np.float64)
    jitter = int(track.synth.get("jitter_samples", 0))
    fundamental = float(track.synth.get("f0_hz", 178.0))
    gain = float(track.synth.get("gain", 0.34))

    for segment in track.synth["segments"]:
        cue_index = int(segment["cue_index"])
        segment_duration = float(segment["duration_seconds"])
        start = manifest.dialogue_points_samples[cue_index] - track.offset_samples + jitter
        length = sample_round(segment_duration, sample_rate)
        if start < 0 or start + length > frames:
            raise ValueError(f"narration segment at cue {cue_index} falls outside local track")

        time = np.arange(length, dtype=np.float64) / sample_rate
        # A short fricative-like onset serves as both speech articulation and sync anchor.
        onset_length = min(length, int(round(0.030 * sample_rate)))
        fricative = rng.standard_normal(onset_length)
        fricative = np.diff(fricative, prepend=fricative[0])
        fricative *= np.exp(-np.arange(onset_length) / max(1.0, 0.009 * sample_rate))
        fricative /= max(float(np.max(np.abs(fricative))), 1e-12)
        fricative *= 0.30 * gain
        if abs(fricative[0]) < 0.28 * gain:
            fricative[0] = math.copysign(0.30 * gain, fricative[0] or 1.0)
        mono[start : start + onset_length] += fricative

        syllable = 0.34 * gain
        envelope = np.ones(length, dtype=np.float64)
        attack = min(length, int(round(0.008 * sample_rate)))
        if attack:
            phase = np.arange(attack, dtype=np.float64) / max(attack - 1, 1)
            envelope[:attack] = 0.5 * (1.0 - np.cos(math.pi * phase))
        modulation = 0.76 + 0.24 * np.sin(2.0 * math.pi * 3.6 * time + 0.4)
        envelope *= modulation
        fade = min(length, int(round(0.040 * sample_rate)))
        if fade:
            phase = np.arange(fade, dtype=np.float64) / max(fade - 1, 1)
            envelope[-fade:] *= 0.5 * (1.0 + np.cos(math.pi * phase))

        phases = rng.uniform(0.0, 2.0 * math.pi, size=8)
        voice = np.zeros(length, dtype=np.float64)
        energy = 0.0
        harmonic_count = min(8, int((sample_rate / 2.0 - 100.0) // fundamental))
        for harmonic in range(1, harmonic_count + 1):
            frequency = fundamental * harmonic
            weight = 0.8 / harmonic**1.05
            weight *= 0.4 + _formant_weight(frequency, 900.0, 240.0) + 0.6 * _formant_weight(
                frequency, 2100.0, 430.0
            )
            voice += weight * np.sin(2.0 * math.pi * frequency * time + phases[harmonic - 1])
            energy += weight * weight
        voice /= math.sqrt(max(energy, 1e-12))
        mono[start : start + length] += voice * envelope * syllable

    if np.max(np.abs(mono)) >= 1.0:
        mono /= np.max(np.abs(mono)) / 0.95
    return _channelize(mono, track.channels, float(track.synth.get("pan", 0.0)))


def _synthesize_music(track: TrackSpec, manifest: Manifest, track_index: int) -> np.ndarray:
    sample_rate = manifest.sample_rate
    duration = float(track.synth["duration_seconds"])
    frames = sample_round(duration, sample_rate)
    if frames <= 0:
        raise ValueError("music duration produced no samples")
    rng = _seed_for(manifest.seed, track_index, 3)
    time = np.arange(frames, dtype=np.float64) / sample_rate
    chord = [float(value) for value in track.synth.get("chord_hz", [130.81, 164.81, 196.0, 246.94])]
    gain = float(track.synth.get("gain", 0.22))
    width = float(np.clip(track.synth.get("width", 0.08), 0.0, 1.0))
    phase = rng.uniform(0.0, 2.0 * math.pi, size=len(chord))
    side_phase = rng.uniform(0.0, 2.0 * math.pi, size=len(chord))

    mid = np.zeros(frames, dtype=np.float64)
    side = np.zeros(frames, dtype=np.float64)
    for index, frequency in enumerate(chord):
        amplitude = 0.9 + 0.1 * np.sin(2.0 * math.pi * (0.17 + index * 0.03) * time + index)
        mid += amplitude * (
            np.sin(2.0 * math.pi * frequency * time + phase[index])
            + 0.22 * np.sin(2.0 * math.pi * frequency * 2.0 * time + phase[index] * 0.5)
        )
        side += 0.16 * np.sin(2.0 * math.pi * (frequency * 1.007) * time + side_phase[index])
    mid /= max(float(np.max(np.abs(mid))), 1e-12)
    side /= max(float(np.max(np.abs(side))), 1e-12)
    right = (mid - width * side) / math.sqrt(1.0 + width * width)
    left = (mid + width * side) / math.sqrt(1.0 + width * width)
    stereo = np.stack((left, right), axis=1)
    fade = min(frames // 2, int(round(float(track.synth.get("fade_seconds", 0.060)) * sample_rate)))
    if fade > 0:
        phase = np.arange(fade, dtype=np.float64) / max(fade - 1, 1)
        ramp = 0.5 * (1.0 - np.cos(math.pi * phase))
        stereo[:fade] *= ramp[:, None]
        stereo[-fade:] *= ramp[::-1, None]
    stereo *= gain
    if track.channels == 1:
        stereo = 0.5 * (stereo[:, 0] + stereo[:, 1])[:, None]
    if np.max(np.abs(stereo)) >= 1.0:
        stereo /= np.max(np.abs(stereo)) / 0.95
    return stereo


def synthesize_manifest(manifest_path: Path) -> dict[str, Any]:
    parsed = load_manifest(manifest_path, validate_audio=False)
    manifest = parsed.manifest
    errors: list[BusinessError] = []
    summaries: list[dict[str, Any]] = []
    for track_index, track in enumerate(manifest.tracks):
        try:
            if track.kind == "dialogue":
                samples = _synthesize_dialogue(track, manifest, track_index)
            elif track.kind == "narration":
                samples = _synthesize_narration(track, manifest, track_index)
            else:
                samples = _synthesize_music(track, manifest, track_index)
        except (KeyError, TypeError, ValueError) as exc:
            errors.append(
                BusinessError(
                    "E_SYNTHESIS_PARAM",
                    f"cannot synthesize track: {exc}",
                    track.name,
                )
            )
            continue
        write_wav(track.path, samples, manifest.sample_rate, manifest.bit_depth)
        summaries.append(
            {
                "track": track.name,
                "kind": track.kind,
                "path": track.relative_path,
                "sample_rate": manifest.sample_rate,
                "channels": track.channels,
                "bit_depth": manifest.bit_depth,
                "frames": int(samples.shape[0]),
                "offset_seconds": round(track.offset_seconds, 9),
                "offset_samples": track.offset_samples,
            }
        )
    if errors:
        raise BusinessFailure(errors, EXIT_VALIDATION)

    report = {
        "schema": "mastering-synthesis/v1",
        "manifest_digest": manifest.digest,
        "seed": manifest.seed,
        "sample_rate": manifest.sample_rate,
        "bit_depth": manifest.bit_depth,
        "tracks": summaries,
    }
    synthesis_report = manifest.output_dir / "synthesis_report.json"
    atomic_write_json(synthesis_report, report)
    return report
