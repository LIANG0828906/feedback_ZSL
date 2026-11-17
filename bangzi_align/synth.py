from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from .wavio import write_wav

SAMPLE_RATE = 48_000
BIT_DEPTH = 24
LEAD_IN_SECONDS = 0.5


def _rng(seed: int, *keys: int) -> np.random.Generator:
    sequence = np.random.SeedSequence([int(seed) & 0xFFFFFFFF, *[int(key) & 0xFFFFFFFF for key in keys]])
    return np.random.default_rng(sequence)


def segment_transport_drift(seed: int, segment_index: int) -> int:
    """Deterministic per-segment clock drift, in samples."""
    return int(_rng(seed, 101, segment_index).integers(-180, 181))


def _reference_positions(start: int, end: int, interval: int) -> list[int]:
    first = start + int(round(0.18 * SAMPLE_RATE))
    last = end - int(round(0.12 * SAMPLE_RATE))
    values = list(range(first, last + 1, interval))
    return [int(value) for value in values]


def build_demo_manifest(seed: int = 237) -> dict[str, Any]:
    durations = (2.9, 3.0, 3.2)
    styles = ("慢板", "流水", "垛板")
    intervals = (0.50, 0.25, 0.1875)
    entries_seconds = (2.35, 2.25, 2.35)
    gains = (
        {"bangzi": -10.0, "banhu": -13.0, "yueqin": -19.0, "laosheng": -8.0, "bangqiang": -11.0},
        {"bangzi": -9.0, "banhu": -12.0, "yueqin": -18.0, "laosheng": -8.5, "bangqiang": -10.5},
        {"bangzi": -8.0, "banhu": -11.0, "yueqin": -17.0, "laosheng": -8.0, "bangqiang": -10.0},
    )
    offsets = {
        "bangzi": 311,
        "banhu": -467,
        "yueqin": 823,
        "laosheng": -1097,
        "bangqiang": 1391,
    }
    pans = {
        "bangzi": -0.18,
        "banhu": -0.38,
        "yueqin": 0.42,
        "laosheng": 0.02,
        "bangqiang": 0.20,
    }
    channel_items = [
        {
            "id": channel_id,
            "voice": voice,
            "path": f"audio/{channel_id}.wav",
            "start_offset_samples": offsets[channel_id],
            "pan": pans[channel_id],
        }
        for channel_id, voice in zip(
            ("bangzi", "banhu", "yueqin", "laosheng", "bangqiang"),
            ("梆子", "板胡", "月琴", "老生", "帮腔"),
        )
    ]
    segments: list[dict[str, Any]] = []
    cursor = int(round(LEAD_IN_SECONDS * SAMPLE_RATE))
    for index, (duration, style, interval, entry_seconds, gain_map) in enumerate(
        zip(durations, styles, intervals, entries_seconds, gains)
    ):
        start = cursor
        end = start + int(round(duration * SAMPLE_RATE))
        entry = start + int(round(entry_seconds * SAMPLE_RATE))
        drag_start = max(
            start + int(round(0.1 * SAMPLE_RATE)),
            entry - int(round(0.52 * SAMPLE_RATE)),
        )
        drag_end = min(
            end + int(round(0.08 * SAMPLE_RATE)),
            drag_start + int(round(1.08 * SAMPLE_RATE)),
        )
        segments.append(
            {
                "id": f"seg{index + 1:02d}",
                "ban_style": style,
                "start_sample": start,
                "end_sample": end,
                "bangzi_reference_samples": _reference_positions(
                    start, end, int(round(interval * SAMPLE_RATE))
                ),
                "bangqiang_entry_sample": entry,
                "synthetic_transport_drift_samples": segment_transport_drift(seed, index),
                "separation_window_samples": [
                    entry,
                    min(end, entry + int(round(0.48 * SAMPLE_RATE))),
                ],
                "laosheng_drag_start_sample": drag_start,
                "laosheng_drag_end_sample": drag_end,
                "channel_gains_db": gain_map,
            }
        )
        cursor = end
    return {
        "schema": "bangzi-session/v1",
        "seed": int(seed),
        "audio": {"sample_rate": SAMPLE_RATE, "bit_depth": BIT_DEPTH, "channels": 1},
        "channels": channel_items,
        "segments": segments,
        "crossfade_samples": int(round(0.08 * SAMPLE_RATE)),
        "master": {
            "target_lufs": -16.0,
            "true_peak_max_dbtp": -1.0,
            "block_samples": 4096,
        },
    }


def _add_at(destination: np.ndarray, position: int, event: np.ndarray) -> None:
    if position >= destination.size:
        return
    source_start = 0
    if position < 0:
        source_start = -position
        position = 0
    count = min(destination.size - position, event.size - source_start)
    if count > 0:
        destination[position : position + count] += event[source_start : source_start + count]


def _bangzi_hit(seed: int, event_index: int, amplitude: float) -> np.ndarray:
    rng = _rng(seed, 201, event_index)
    length = int(round(0.055 * SAMPLE_RATE))
    time = np.arange(length, dtype=np.float64) / SAMPLE_RATE
    click = rng.standard_normal(length) * np.exp(-time / 0.0011)
    body = (
        0.62 * np.sin(2.0 * np.pi * 1760.0 * time + 0.31)
        + 0.43 * np.sin(2.0 * np.pi * 3020.0 * time + 1.17)
        + 0.20 * np.sin(2.0 * np.pi * 4080.0 * time + 2.03)
    ) * np.exp(-time / 0.016)
    event = amplitude * (0.28 * click + 0.70 * body)
    event[0] += amplitude * 0.72
    return event


def _stable_note(
    frequency: float,
    duration: float,
    vibrato_hz: float,
    vibrato_cents: float,
    seed: int,
    note_index: int,
    harmonics: tuple[float, ...],
    attack: float,
    release: float,
    glide_from: float | None = None,
) -> np.ndarray:
    rng = _rng(seed, 301, note_index)
    length = max(1, int(round(duration * SAMPLE_RATE)))
    time = np.arange(length, dtype=np.float64) / SAMPLE_RATE
    if glide_from is None or abs(glide_from - frequency) < 1e-9:
        phase = 2.0 * np.pi * frequency * time
    else:
        glide_length = min(length, max(1, int(round(0.045 * SAMPLE_RATE))))
        position = np.clip(np.arange(length, dtype=np.float64) / glide_length, 0.0, 1.0)
        smooth = position * position * (3.0 - 2.0 * position)
        semitone_delta = np.log2(glide_from / frequency)
        instantaneous = frequency * np.power(2.0, semitone_delta * (1.0 - smooth))
        phase = 2.0 * np.pi * np.cumsum(instantaneous) / SAMPLE_RATE
    phase += (vibrato_cents / 1200.0) * (frequency / vibrato_hz) * np.sin(
        2.0 * np.pi * vibrato_hz * time + rng.uniform(0.0, 2.0 * np.pi)
    )
    signal = np.zeros(length, dtype=np.float64)
    for harmonic_index, weight in enumerate(harmonics, start=1):
        signal += weight * np.sin(harmonic_index * phase + 0.13 * harmonic_index)
    attack_samples = max(1, int(round(attack * SAMPLE_RATE)))
    release_samples = max(1, int(round(release * SAMPLE_RATE)))
    envelope = np.ones(length, dtype=np.float64)
    envelope[: min(length, attack_samples)] = np.linspace(
        1.0 / attack_samples, 1.0, min(length, attack_samples)
    )
    if release_samples < length:
        envelope[-release_samples:] *= np.linspace(1.0, 0.0, release_samples) ** 1.7
    return signal * envelope


def _banhu_note(
    seed: int,
    note_index: int,
    frequency: float,
    duration: float,
    glide_from: float | None = None,
) -> np.ndarray:
    return _stable_note(
        frequency,
        duration,
        vibrato_hz=5.4,
        vibrato_cents=18.0,
        seed=seed,
        note_index=note_index,
        harmonics=(0.94, 0.45, 0.27, 0.14, 0.08),
        attack=0.012,
        release=0.055,
        glide_from=glide_from,
    )


def _yueqin_note(
    seed: int,
    note_index: int,
    frequency: float,
    duration: float,
    glide_from: float | None = None,
) -> np.ndarray:
    return _stable_note(
        frequency,
        duration,
        vibrato_hz=4.2,
        vibrato_cents=6.0,
        seed=seed,
        note_index=note_index,
        harmonics=(0.86, 0.34, 0.21, 0.11),
        attack=0.003,
        release=min(0.09, duration * 0.45),
        glide_from=glide_from,
    )


def _laosheng_note(
    seed: int,
    note_index: int,
    frequency: float,
    duration: float,
    glide_from: float | None = None,
) -> np.ndarray:
    return _stable_note(
        frequency,
        duration,
        vibrato_hz=4.8,
        vibrato_cents=33.0,
        seed=seed,
        note_index=note_index,
        harmonics=(0.90, 0.42, 0.34, 0.18, 0.10, 0.05),
        attack=0.020,
        release=0.10,
        glide_from=glide_from,
    )


def _bangqiang_texture(seed: int, note_index: int, frequency: float, duration: float) -> np.ndarray:
    length = max(1, int(round(duration * SAMPLE_RATE)))
    time = np.arange(length, dtype=np.float64) / SAMPLE_RATE
    rng = _rng(seed, 401, note_index)
    signal = np.zeros(length, dtype=np.float64)
    for voice_index, detune in enumerate((-19.0, -7.0, 4.0, 15.0, 26.0)):
        local_frequency = frequency * (2.0 ** (detune / 1200.0))
        phase = 2.0 * np.pi * local_frequency * time
        phase += 0.16 * np.sin(2.0 * np.pi * (5.1 + 0.13 * voice_index) * time + rng.uniform(0, 6.28))
        signal += (
            0.24 * np.sin(phase)
            + 0.13 * np.sin(2.0 * phase + 0.2 * voice_index)
            + 0.07 * np.sin(3.0 * phase + 0.4 * voice_index)
        )
    attack_samples = int(round(0.018 * SAMPLE_RATE))
    release_samples = min(int(round(0.08 * SAMPLE_RATE)), length)
    envelope = np.ones(length, dtype=np.float64)
    envelope[:attack_samples] = np.linspace(1.0 / attack_samples, 1.0, attack_samples)
    envelope[-release_samples:] *= np.linspace(1.0, 0.0, release_samples) ** 1.4
    return signal * envelope / 2.1


def _melody_notes(
    seed: int,
    segment_index: int,
    start: int,
    end: int,
    entry: int,
    note_maker,
    base_frequency: float,
    spacing: float,
    drag_start_sample: int | None = None,
    drag_end_sample: int | None = None,
) -> list[tuple[int, np.ndarray]]:
    rng = _rng(seed, 501, segment_index)
    events: list[tuple[int, np.ndarray]] = []
    position = start + int(round(0.04 * SAMPLE_RATE))
    note_index = segment_index * 20
    previous_frequency: float | None = None
    scale = (0.0, 2.0, 3.0, 5.0, 7.0, 9.0, 10.0, 12.0)
    while position < end:
        degree = int(rng.integers(0, len(scale)))
        frequency = base_frequency * (2.0 ** (scale[degree] / 12.0))
        following = position + int(round(spacing * SAMPLE_RATE))
        duration = min((following - position) / SAMPLE_RATE + 0.08, (end - position) / SAMPLE_RATE + 0.12)
        duration = max(0.10, duration)
        events.append(
            (
                position,
                note_maker(
                    seed,
                    note_index,
                    frequency,
                    duration,
                    glide_from=previous_frequency,
                ),
            )
        )
        previous_frequency = frequency
        note_index += 1
        position = following
    if drag_start_sample is None:
        drag_start = max(
            start + int(round(0.1 * SAMPLE_RATE)),
            entry - int(round(0.52 * SAMPLE_RATE)),
        )
    else:
        drag_start = int(drag_start_sample)
    if drag_end_sample is None:
        drag_end = drag_start + int(round(1.08 * SAMPLE_RATE))
    else:
        drag_end = int(drag_end_sample)
    drag_duration = max(0.35, (drag_end - drag_start) / SAMPLE_RATE)
    events.append(
        (
            drag_start,
            note_maker(
                seed,
                note_index,
                base_frequency * (2.0 ** (-2.0 / 12.0)),
                drag_duration,
                glide_from=previous_frequency,
            ),
        )
    )
    return events


def _render_bangzi(seed: int, manifest: dict[str, Any], length: int) -> np.ndarray:
    output = np.zeros(length, dtype=np.float64)
    state = manifest["channels"][0]
    offset = state["start_offset_samples"]
    hit_counter = 0
    for segment_index, segment in enumerate(manifest["segments"]):
        drift = segment_transport_drift(seed, segment_index)
        positions = [int(value) for value in segment["bangzi_reference_samples"]]
        extras: list[tuple[int, float]] = []
        for left, right in zip(positions, positions[1:]):
            gap = right - left
            if gap > int(round(0.16 * SAMPLE_RATE)):
                extras.append(((left + right) // 2, 0.48))
                if segment["ban_style"] == "垛板" and gap > int(round(0.30 * SAMPLE_RATE)):
                    extras.append((left + gap // 3, 0.34))
                    extras.append((left + (2 * gap) // 3, 0.30))
        combined = [(position, 1.0, index) for index, position in enumerate(positions)]
        combined.extend((position, amplitude, 1000 + index) for index, (position, amplitude) in enumerate(extras))
        combined.sort(key=lambda item: (item[0], item[2]))
        for position, amplitude, local_index in combined:
            event = _bangzi_hit(seed, hit_counter, amplitude)
            _add_at(output, position + offset + drift, event)
            hit_counter += 1
    return output


def _render_banhu(seed: int, manifest: dict[str, Any], length: int) -> np.ndarray:
    output = np.zeros(length, dtype=np.float64)
    channel = manifest["channels"][1]
    offset = channel["start_offset_samples"]
    for segment_index, segment in enumerate(manifest["segments"]):
        drift = segment_transport_drift(seed, segment_index)
        events = _melody_notes(
            seed,
            segment_index,
            segment["start_sample"],
            segment["end_sample"],
            segment["bangqiang_entry_sample"],
            _banhu_note,
            392.0,
            0.42,
            segment.get("laosheng_drag_start_sample"),
            segment.get("laosheng_drag_end_sample"),
        )
        for position, event in events:
            _add_at(output, position + offset + drift, event)
    return output


def _render_yueqin(seed: int, manifest: dict[str, Any], length: int) -> np.ndarray:
    output = np.zeros(length, dtype=np.float64)
    channel = manifest["channels"][2]
    offset = channel["start_offset_samples"]
    for segment_index, segment in enumerate(manifest["segments"]):
        drift = segment_transport_drift(seed, segment_index)
        events = _melody_notes(
            seed,
            segment_index + 10,
            segment["start_sample"],
            segment["end_sample"],
            segment["bangqiang_entry_sample"],
            _yueqin_note,
            261.63,
            0.34,
            segment.get("laosheng_drag_start_sample"),
            segment.get("laosheng_drag_end_sample"),
        )
        for position, event in events:
            _add_at(output, position + offset + drift, event)
    return output


def _render_laosheng(seed: int, manifest: dict[str, Any], length: int) -> np.ndarray:
    output = np.zeros(length, dtype=np.float64)
    channel = manifest["channels"][3]
    offset = channel["start_offset_samples"]
    for segment_index, segment in enumerate(manifest["segments"]):
        drift = segment_transport_drift(seed, segment_index)
        events = _melody_notes(
            seed,
            segment_index + 30,
            segment["start_sample"],
            segment["end_sample"],
            segment["bangqiang_entry_sample"],
            _laosheng_note,
            196.0,
            0.62,
            segment.get("laosheng_drag_start_sample"),
            segment.get("laosheng_drag_end_sample"),
        )
        for position, event in events:
            _add_at(output, position + offset + drift, event)
    return output


def _render_bangqiang(seed: int, manifest: dict[str, Any], length: int) -> np.ndarray:
    output = np.zeros(length, dtype=np.float64)
    channel = manifest["channels"][4]
    offset = channel["start_offset_samples"]
    for segment_index, segment in enumerate(manifest["segments"]):
        drift = segment_transport_drift(seed, segment_index)
        entry = int(segment["bangqiang_entry_sample"])
        duration = (segment["end_sample"] - entry) / SAMPLE_RATE + 0.06
        event = _bangqiang_texture(seed, segment_index, 293.66, duration)
        _add_at(output, entry + offset + drift, event)
    return output


def _normalize_peak(signal: np.ndarray, peak: float = 0.93) -> np.ndarray:
    maximum = float(np.max(np.abs(signal))) if signal.size else 0.0
    if maximum > peak:
        signal = signal * (peak / maximum)
    return signal


def render_demo_sources(manifest: dict[str, Any], output_dir: Path) -> None:
    seed = int(manifest["seed"])
    total_target = max(int(segment["end_sample"]) for segment in manifest["segments"])
    source_length = total_target + int(round(0.30 * SAMPLE_RATE))
    renderers = (
        _render_bangzi,
        _render_banhu,
        _render_yueqin,
        _render_laosheng,
        _render_bangqiang,
    )
    for channel, renderer in zip(manifest["channels"], renderers):
        signal = _normalize_peak(renderer(seed, manifest, source_length))
        write_wav(
            output_dir / channel["path"],
            signal,
            int(manifest["audio"]["sample_rate"]),
            int(manifest["audio"]["bit_depth"]),
        )


def generate_demo(output_dir: str | Path, seed: int = 237, force: bool = False) -> Path:
    root = Path(output_dir)
    manifest_path = root / "manifest.json"
    root.mkdir(parents=True, exist_ok=True)
    manifest = build_demo_manifest(seed)
    render_demo_sources(manifest, root)
    manifest_path.write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    return manifest_path

