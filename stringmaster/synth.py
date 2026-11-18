"""Seeded string-quartet score and deterministic microphone rendering."""

from __future__ import annotations

import math
import os
from dataclasses import dataclass
from typing import Any

import numpy as np

from .audio import atomic_write_json, fractional_delay, sha256_file, write_wav
from .manifest import INSTRUMENTS, ManifestContext


@dataclass(frozen=True)
class NoteEvent:
    start_seconds: float
    end_seconds: float
    frequency_hz: float
    stroke_count: int

    def as_dict(self) -> dict[str, float | int]:
        return {
            "start_seconds": self.start_seconds,
            "end_seconds": self.end_seconds,
            "frequency_hz": self.frequency_hz,
            "stroke_count": self.stroke_count,
        }


@dataclass(frozen=True)
class MutationEvent:
    start_seconds: float
    end_seconds: float
    low_hz: float
    high_hz: float
    component_frequencies_hz: tuple[float, ...]
    amplitude: float

    def as_dict(self) -> dict[str, Any]:
        return {
            "instrument": "第一小提琴",
            "start_seconds": self.start_seconds,
            "end_seconds": self.end_seconds,
            "low_hz": self.low_hz,
            "high_hz": self.high_hz,
            "component_frequencies_hz": list(self.component_frequencies_hz),
            "amplitude": self.amplitude,
        }


@dataclass(frozen=True)
class Score:
    seed: int
    duration_seconds: float
    notes: dict[str, tuple[NoteEvent, ...]]
    mutations: tuple[MutationEvent, ...]

    def active_intervals(self, instrument: str) -> tuple[tuple[float, float], ...]:
        return tuple((note.start_seconds, note.end_seconds) for note in self.notes[instrument])

    def as_dict(self) -> dict[str, Any]:
        first_last: dict[str, dict[str, float]] = {}
        timelines: dict[str, list[dict[str, float | int]]] = {}
        for instrument in INSTRUMENTS:
            notes = self.notes[instrument]
            timelines[instrument] = [note.as_dict() for note in notes]
            first_last[instrument] = {
                "start_seconds": notes[0].start_seconds if notes else 0.0,
                "end_seconds": notes[-1].end_seconds if notes else 0.0,
            }
        return {
            "seed": self.seed,
            "duration_seconds": self.duration_seconds,
            "instrument_start_end": first_last,
            "instrument_timelines": timelines,
            "mutation_intervals": [event.as_dict() for event in self.mutations],
        }


_REGISTERS = {
    "第一小提琴": (523.2511, 587.3295, 659.2551, 783.9909, 880.0),
    "第二小提琴": (392.0, 440.0, 493.8833, 523.2511, 587.3295),
    "中提琴": (220.0, 246.9417, 293.6648, 329.6276, 349.2282),
    "大提琴": (110.0, 123.4708, 146.8324, 164.8138, 174.6141),
}


def _rng(seed: int, index: int, purpose: int) -> np.random.Generator:
    sequence = np.random.SeedSequence(
        [int(seed) & 0xFFFFFFFF, (int(seed) >> 32) & 0xFFFFFFFF, index, purpose]
    )
    return np.random.Generator(np.random.PCG64(sequence))


def _build_timeline(
    seed: int,
    instrument_index: int,
    instrument: str,
    duration: float,
) -> tuple[NoteEvent, ...]:
    rng = _rng(seed, instrument_index, 11)
    register = _REGISTERS[instrument]
    cursor = 0.045 + 0.018 * instrument_index + float(rng.uniform(0.0, 0.045))
    notes: list[NoteEvent] = []
    while cursor < duration - 0.20:
        rest = float(rng.uniform(0.060, 0.140))
        start = cursor + rest
        desired = float(rng.uniform(0.40, 0.82))
        end = min(duration - 0.045, start + desired)
        if end - start < 0.22:
            break
        note_index = int(rng.integers(0, len(register)))
        frequency = float(register[note_index])
        stroke_count = 2 if (end - start > 0.58 and rng.random() < 0.55) else 1
        notes.append(
            NoteEvent(
                start_seconds=round(start, 9),
                end_seconds=round(end, 9),
                frequency_hz=round(frequency, 9),
                stroke_count=stroke_count,
            )
        )
        cursor = end
    if not notes:
        start = min(0.10, duration * 0.1)
        end = max(start + 0.20, duration * 0.7)
        notes.append(
            NoteEvent(
                start_seconds=round(start, 9),
                end_seconds=round(end, 9),
                frequency_hz=register[0],
                stroke_count=1,
            )
        )
    return tuple(notes)


def _derive_mutations(
    context: ManifestContext,
    notes: tuple[NoteEvent, ...],
) -> tuple[MutationEvent, ...]:
    rng = _rng(context.seed, 101, 29)
    candidates = [
        note
        for note in notes
        if note.end_seconds - note.start_seconds >= 0.58
        and note.start_seconds < context.duration_seconds - 0.65
    ]
    target_count = 3 if context.duration_seconds >= 10.0 else 2
    if len(candidates) < target_count:
        candidates = sorted(
            notes,
            key=lambda note: note.end_seconds - note.start_seconds,
            reverse=True,
        )
    target_count = min(target_count, len(candidates), max(1, int(context.duration_seconds // 2)))
    if target_count <= 0:
        return ()

    order = np.arange(len(candidates))
    rng.shuffle(order)
    chosen: list[NoteEvent] = []
    for raw_index in order:
        note = candidates[int(raw_index)]
        if any(
            not (
                note.end_seconds <= existing.start_seconds - 0.18
                or note.start_seconds >= existing.end_seconds + 0.18
            )
            for existing in chosen
        ):
            continue
        chosen.append(note)
        if len(chosen) >= target_count:
            break
    chosen.sort(key=lambda note: note.start_seconds)

    band_width = context.band_high_hz - context.band_low_hz
    mutations: list[MutationEvent] = []
    for note in chosen:
        playable = note.end_seconds - note.start_seconds
        duration = min(0.44, max(0.24, playable - 0.30))
        earliest = note.start_seconds + 0.15
        latest = note.end_seconds - 0.13 - duration
        if latest < earliest:
            latest = earliest
        start = float(rng.uniform(earliest, latest))
        start = min(start, context.duration_seconds - duration - 0.04)
        end = start + duration
        component_count = 3
        frequencies = tuple(
            round(
                context.band_low_hz
                + band_width * (0.18 + 0.62 * (index + 0.35) / component_count),
                9,
            )
            for index in range(component_count)
        )
        mutations.append(
            MutationEvent(
                start_seconds=round(start, 9),
                end_seconds=round(end, 9),
                low_hz=context.band_low_hz,
                high_hz=context.band_high_hz,
                component_frequencies_hz=frequencies,
                amplitude=0.200,
            )
        )
    return tuple(mutations)


def derive_score(context: ManifestContext) -> Score:
    notes = {
        instrument: _build_timeline(
            context.seed,
            index,
            instrument,
            context.duration_seconds,
        )
        for index, instrument in enumerate(INSTRUMENTS)
    }
    mutations = _derive_mutations(context, notes["第一小提琴"])
    return Score(
        seed=context.seed,
        duration_seconds=context.duration_seconds,
        notes=notes,
        mutations=mutations,
    )


def _bow_envelope(local_time: np.ndarray, duration: float, stroke_count: int) -> np.ndarray:
    attack = np.clip(local_time / 0.020, 0.0, 1.0)
    attack = np.sin(0.5 * np.pi * attack) ** 2
    release_start = max(0.0, duration - 0.055)
    release = np.clip((duration - local_time) / 0.055, 0.0, 1.0)
    release = np.sin(0.5 * np.pi * release) ** 2
    envelope = attack * release
    if stroke_count > 1:
        for stroke in range(1, stroke_count):
            center = duration * stroke / stroke_count
            dip = np.exp(-0.5 * ((local_time - center) / 0.018) ** 2)
            envelope *= 1.0 - 0.24 * dip
    breathe = 1.0 + 0.018 * np.sin(2.0 * np.pi * 0.43 * local_time + 0.7)
    return envelope * breathe


def _render_note(
    note: NoteEvent,
    sample_rate: int,
    instrument_index: int,
    seed: int,
    total_frames: int,
) -> np.ndarray:
    rng = _rng(seed, instrument_index * 31 + int(note.start_seconds * 1000), 47)
    start = int(round(note.start_seconds * sample_rate))
    end = min(total_frames, int(round(note.end_seconds * sample_rate)))
    length = max(0, end - start)
    if length == 0:
        return np.zeros(total_frames, dtype=np.float64)
    local_time = np.arange(length, dtype=np.float64) / sample_rate
    duration = length / sample_rate
    envelope = _bow_envelope(local_time, duration, note.stroke_count)

    vibrato_rate = float(rng.uniform(4.6, 6.2))
    vibrato_depth = float(rng.uniform(0.0055, 0.0105))
    vibrato_phase = float(rng.uniform(0.0, 2.0 * np.pi))
    vibrato_ramp = np.clip((local_time - 0.070) / 0.12, 0.0, 1.0)
    phase = (
        2.0 * np.pi * note.frequency_hz * local_time
        + note.frequency_hz
        * vibrato_depth
        / vibrato_rate
        * (1.0 - np.cos(2.0 * np.pi * vibrato_rate * local_time + vibrato_phase))
        * vibrato_ramp
    )

    harmonic_count = 8 if instrument_index < 2 else 7
    harmonic_rolloff = 1.18 if instrument_index == 0 else 1.72 + 0.06 * instrument_index
    amplitude = (0.145, 0.150, 0.170, 0.190)[instrument_index]
    tone = np.zeros(length, dtype=np.float64)
    for harmonic in range(1, harmonic_count + 1):
        if note.frequency_hz * harmonic >= sample_rate * 0.46:
            break
        weight = 1.0 / (harmonic**harmonic_rolloff)
        if harmonic == 1:
            weight *= 1.16
        harmonic_phase = float(rng.uniform(0.0, 2.0 * np.pi))
        tone += weight * np.sin(harmonic * phase + harmonic_phase)
    tone /= max(1.0, np.max(np.abs(tone)))

    stroke_times = np.linspace(0.0, duration, note.stroke_count + 1)[:-1]
    transient = np.zeros(length, dtype=np.float64)
    for index, stroke_time in enumerate(stroke_times):
        offset = int(round(stroke_time * sample_rate))
        transient_length = min(length - offset, int(0.016 * sample_rate))
        if transient_length <= 0:
            continue
        noise = rng.normal(0.0, 1.0, transient_length)
        decay = np.exp(-np.arange(transient_length) / (0.0032 * sample_rate))
        transient[offset : offset + transient_length] += 0.040 * noise * decay / (index + 1)

    body = amplitude * envelope * tone + transient
    result = np.zeros(total_frames, dtype=np.float64)
    result[start:end] = body
    return result


def _add_mutation(
    source: np.ndarray,
    mutation: MutationEvent,
    sample_rate: int,
    seed: int,
    mutation_index: int,
) -> None:
    rng = _rng(seed, mutation_index, 71)
    start = int(round(mutation.start_seconds * sample_rate))
    end = min(source.shape[0], int(round(mutation.end_seconds * sample_rate)))
    length = max(0, end - start)
    if length == 0:
        return
    time = np.arange(length, dtype=np.float64) / sample_rate
    component = np.zeros(length, dtype=np.float64)
    for index, frequency in enumerate(mutation.component_frequencies_hz):
        phase = float(rng.uniform(0.0, 2.0 * np.pi))
        component += np.sin(2.0 * np.pi * frequency * time + phase + 0.37 * index)
    component /= math.sqrt(len(mutation.component_frequencies_hz))
    fade = min(int(0.010 * sample_rate), max(1, length // 4))
    envelope = np.ones(length, dtype=np.float64)
    ramp = np.linspace(0.0, 1.0, fade, endpoint=False)
    envelope[:fade] = ramp * ramp * (3.0 - 2.0 * ramp)
    envelope[-fade:] = envelope[:fade][::-1]
    source[start:end] += mutation.amplitude * envelope * component


def synthesize_tracks(context: ManifestContext) -> tuple[dict[str, np.ndarray], Score]:
    score = derive_score(context)
    total_frames = int(round(context.duration_seconds * context.sample_rate))
    mono: dict[str, np.ndarray] = {}
    for instrument_index, instrument in enumerate(INSTRUMENTS):
        signal = np.zeros(total_frames, dtype=np.float64)
        for note in score.notes[instrument]:
            signal += _render_note(
                note,
                context.sample_rate,
                instrument_index,
                context.seed,
                total_frames,
            )
        if instrument == "第一小提琴":
            for index, mutation in enumerate(score.mutations):
                _add_mutation(signal, mutation, context.sample_rate, context.seed, index)
        mono[instrument] = signal

    tracks: dict[str, np.ndarray] = {}
    for track in context.tracks:
        source = mono[track.instrument]
        azimuth = math.radians(track.azimuth_deg)
        lateral = 0.22 * math.sin(azimuth)
        path_left = max(0.01, track.distance_m + lateral)
        path_right = max(0.01, track.distance_m - lateral)
        delay_left = path_left / 343.0 * context.sample_rate
        delay_right = path_right / 343.0 * context.sample_rate
        distance_gain = 0.45 / (0.45 + max(0.05, track.distance_m))
        orientation_gain = 0.72 + 0.28 * math.cos(azimuth)
        left_gain = distance_gain * orientation_gain * math.sqrt(0.5 + 0.35 * math.sin(azimuth))
        right_gain = distance_gain * orientation_gain * math.sqrt(0.5 - 0.35 * math.sin(azimuth))
        delayed = fractional_delay(source[:, None], delay_left)
        stereo = np.zeros((total_frames, 2), dtype=np.float64)
        stereo[:, 0] = delayed[:, 0] * left_gain
        delayed = fractional_delay(source[:, None], delay_right)
        stereo[:, 1] = delayed[:, 0] * right_gain
        tracks[track.instrument] = stereo
    return tracks, score


def generate_project(context: ManifestContext) -> dict[str, Any]:
    tracks, score = synthesize_tracks(context)
    track_report: list[dict[str, Any]] = []
    for track in context.tracks:
        data = tracks[track.instrument]
        write_wav(track.abs_path, data, context.sample_rate, context.bit_depth)
        track_report.append(
            {
                "instrument": track.instrument,
                "path": track.abs_path,
                "frames": int(data.shape[0]),
                "channels": int(data.shape[1]),
                "bit_depth": context.bit_depth,
                "sha256": sha256_file(track.abs_path),
            }
        )
    truth_path = os.path.join(context.manifest_dir, "score_truth.json")
    truth = score.as_dict()
    truth["manifest_digest"] = context.digest
    truth["tracks"] = track_report
    atomic_write_json(truth_path, truth)
    return truth
