"""Deterministic local sample generator for mono and 5.1 verification."""

from __future__ import annotations

import hashlib
import random
import wave
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .canonical import atomic_write_json, canonical_bytes, sha256_bytes
from .engine import layout_track
from .estimator import count_readable_units
from .errors import LCFTError
from .params import LayoutParams
from .timebase import write_frame_table

VOCABULARY = {
    "zh": ["字幕", "语音", "描述", "排版", "时间轴", "间隙", "哈希", "快照", "增量", "预算", "并行", "调度", "恢复", "本地化"],
    "en": ["subtitle", "speech", "description", "layout", "timeline", "gap", "hash", "snapshot", "incremental", "budget", "parallel", "scheduler", "recovery", "localization"],
    "fr": ["sous-titre", "parole", "description", "mise-en-page", "chronologie", "intervalle", "empreinte", "instantane", "increment", "budget", "parallele", "ordonnanceur", "reprise", "localisation"],
    "ja": ["字幕", "音声", "説明", "配置", "時間軸", "間隔", "ハッシュ", "スナップショット", "差分", "予算", "並列", "スケジューラ", "復元", "ローカライズ"],
}


def _text(rng: random.Random, language: str, words: int) -> str:
    vocabulary = VOCABULARY.get(language, VOCABULARY["en"])
    return " ".join(rng.choice(vocabulary) for _ in range(words))


def _write_silence_like_wav(
    path: Path,
    *,
    total_samples: int,
    channels: int,
    seed: int,
) -> str:
    """Stream deterministic 16-bit PCM with a fixed integer waveform."""

    path.parent.mkdir(parents=True, exist_ok=True)
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(channels)
        handle.setsampwidth(2)
        handle.setframerate(48_000)
        chunk = 48_000
        rng = random.Random(seed ^ 0x5A17)
        base_periods = [rng.choice([73, 97, 131, 173]) for _ in range(channels)]
        gains = [rng.choice([600, 900, 1200, 1500]) for _ in range(channels)]
        for offset in range(0, total_samples, chunk):
            count = min(chunk, total_samples - offset)
            indices = np.arange(offset, offset + count, dtype=np.int64)
            output = np.empty((count, channels), dtype=np.int16)
            for channel in range(channels):
                period = base_periods[channel]
                gain = gains[channel]
                phase = (indices * (channel + 1) + seed + channel * 17) % period
                wave_value = ((phase * 2 * gain) // period) - gain
                output[:, channel] = wave_value.astype(np.int16)
            handle.writeframes(output.astype("<i2", copy=False).tobytes())
    return sha256_bytes(path.read_bytes())


def _duration_for(
    rng: random.Random,
    *,
    sentence_count: int,
    total_samples: int | None,
    sample_rate: int,
) -> int:
    if total_samples is not None:
        return total_samples
    cursor = 0
    for _ in range(sentence_count):
        speech = rng.randint(int(1.8 * sample_rate), int(3.8 * sample_rate))
        requested_gap = rng.choice(
            [
                int(0.08 * sample_rate),
                int(0.12 * sample_rate),
                int(0.32 * sample_rate),
                int(0.72 * sample_rate),
                int(1.00 * sample_rate),
            ]
        )
        gap = min(
            requested_gap,
            max(0, slot - int(Fraction(6, 5) * sample_rate)),
        )
        cursor += speech + gap
    return cursor + sample_rate


def _speech_track(
    rng: random.Random,
    language: str,
    *,
    count: int,
    total_samples: int,
    sample_rate: int,
    contention_permille: int,
    read_speed_min: int,
    read_speed_max: int,
) -> tuple[list[dict[str, Any]], list[tuple[int, int]]]:
    if count <= 0:
        return [], []
    gap_budget = max(0, total_samples - sample_rate)
    weights = [rng.randint(50, 160) for _ in range(count)]
    total_weight = sum(weights)
    slot_samples = max(count, gap_budget // total_weight)
    remaining = gap_budget
    cursor = 0
    sentences: list[dict[str, Any]] = []
    windows: list[tuple[int, int]] = []
    current_speaker = "SPK_A"
    speaker_run = 0
    for index in range(count):
        if index == count - 1:
            slot = remaining
        else:
            planned = max(int(1.2 * sample_rate), weights[index] * slot_samples)
            slot = min(planned, max(int(1.2 * sample_rate), remaining - int(1.2 * sample_rate)))
        slot = max(2, slot)
        gap = rng.choice(
            [
                int(0.08 * sample_rate),
                int(0.08 * sample_rate),
                int(0.20 * sample_rate),
                int(0.40 * sample_rate),
            ]
        )
        start = cursor
        if index == count - 1:
            speech_end = total_samples
        else:
            speech_end = min(total_samples, start + max(1, slot - gap))
        if speech_end <= cursor:
            speech_end = min(total_samples, cursor + max(1, sample_rate // 2))
        if index < count - 1 and index % 5 == rng.randint(0, 4):
            speaker_run += 1
        if speaker_run >= rng.randint(3, 6):
            current_speaker = rng.choice(["SPK_A", "SPK_B", "SPK_C"])
            speaker_run = 0
        speed = rng.randint(read_speed_min, read_speed_max)
        units_per_word = 2 if language in ("zh", "ja") else 6
        available_units = max(
            1,
            ((speech_end - start) * speed * 3 // 4) // sample_rate,
        )
        words = max(1, min(9, available_units // units_per_word))
        priority = 3
        if contention_permille >= 500 and index % 7 == 0:
            priority = 2
        sentence = {
            "id": f"{language}-s{index:04d}",
            "text": _text(rng, language, words),
            "start": start,
            "end": speech_end,
            "speaker": current_speaker,
            "max_read_speed": speed,
            "priority": priority,
        }
        sentences.append(sentence)
        windows.append((start, speech_end))
        cursor = speech_end + gap
        remaining = total_samples - cursor
        if remaining <= 0:
            break
    return sentences, windows


def _descriptions(
    rng: random.Random,
    language: str,
    *,
    count: int,
    total_samples: int,
    sample_rate: int,
    gaps: list[tuple[int, int]],
    slots: list[tuple[int, int, int]],
    description_speed: Fraction,
    description_min_seconds: Fraction,
    contention_permille: int,
) -> list[dict[str, Any]]:
    result: list[dict[str, Any]] = []
    source_gaps = gaps or [(0, total_samples)]
    safe_slots = slots or [
        (low, high - low, 0) for low, high in source_gaps
    ]
    chosen_slots: list[int] = []
    slot_cursor = 0
    minimum_duration = int(description_min_seconds * sample_rate)
    short_words = {
        "zh": ["画面", "场景", "声音"],
        "ja": ["映像", "场景", "音声"],
        "fr": ["note", "plan", "scene"],
        "en": ["note", "scene", "sound"],
    }.get(language, ["note", "scene", "sound"])
    for index in range(count):
        text = rng.choice(short_words)
        units = count_readable_units(text)
        ideal = (units * sample_rate * description_speed.denominator + description_speed.numerator - 1) // description_speed.numerator
        duration = max(minimum_duration, ideal)
        if (
            index % 2 == 1
            and chosen_slots
            and rng.randint(0, 999) < contention_permille
        ):
            slot_index = chosen_slots[-1]
        else:
            slot_index = slot_cursor % len(safe_slots)
            slot_cursor += 1
        chosen_slots.append(slot_index)
        low, free_length, _slack = safe_slots[slot_index]
        if free_length >= duration:
            anchor = low + rng.randint(0, max(0, free_length - duration))
        else:
            # Let the arbiter prove high-priority placement or reject the
            # lower-priority partner; this is the intentional contention.
            anchor = low
        end = min(total_samples, anchor + duration)
        if end <= anchor:
            end = min(total_samples, anchor + 1)
        # Alternating priorities make paired contention decidable in favor of
        # the first description without equal-priority ambiguity.
        priority = 100 - 2 * (index // 2) if index % 2 == 0 else 1
        result.append(
            {
                "id": f"desc-{language}-d{index:04d}",
                "text": text,
                "start": anchor,
                "end": end,
                "speaker": "",
                "priority": priority,
            }
        )
    result.sort(key=lambda item: (item["start"], item["id"]))
    return result


def _description_slots(
    sentences: list[dict[str, Any]],
    *,
    total_samples: int,
    sample_rate: int,
    gap_ticks: int,
    speech_min_seconds: Fraction,
    description_min_seconds: Fraction,
    description_speed: Fraction,
) -> list[tuple[int, int, int]]:
    slots: list[tuple[int, int, int]] = []
    minimum_description = int(description_min_seconds * sample_rate)
    for index in range(len(sentences) - 1):
        previous = sentences[index]
        current = sentences[index + 1]
        low = previous["end"] + gap_ticks
        high = current["start"] - gap_ticks
        free_length = max(0, high - low)
        units = count_readable_units(previous["text"])
        speed = Fraction(previous.get("max_read_speed", 14))
        required = max(
            int(speech_min_seconds * sample_rate),
            (units * sample_rate * speed.denominator + speed.numerator - 1)
            // speed.numerator,
        )
        slack = max(0, previous["end"] - previous["start"] - required)
        if free_length + slack >= minimum_description:
            slots.append((low, free_length, slack))
    return slots


def generate_sample(
    out_dir: str | Path,
    *,
    seed: int = 20261122,
    sentence_count: int = 48,
    languages: Iterable[str] = ("zh", "en"),
    channels: int = 1,
    frame_rate: Fraction | int = 30,
    description_density_permille: int = 160,
    contention_permille: int = 350,
    read_speed_min: int = 9,
    read_speed_max: int = 17,
    duration_seconds: str | int | None = None,
    params: LayoutParams | dict[str, Any] | None = None,
) -> dict[str, Any]:
    if sentence_count < 1:
        raise ValueError("sentence_count must be positive")
    if channels not in (1, 6):
        raise ValueError("channels must be 1 or 6")
    if read_speed_min < 1 or read_speed_max < read_speed_min:
        raise ValueError("read speed range must satisfy 1 <= min <= max")
    language_list = sorted(set(str(language) for language in languages))
    if not language_list:
        raise ValueError("at least one language is required")
    if description_density_permille < 0 or contention_permille < 0:
        raise ValueError("density and contention must be non-negative")
    rate = Fraction(frame_rate)
    if rate <= 0:
        raise ValueError("frame_rate must be positive")
    sample_rate = 48_000
    rng = random.Random(seed)
    total_samples: int | None = None
    if duration_seconds is not None:
        total_samples = int(Fraction(str(duration_seconds)) * sample_rate)
        minimum_timeline = sentence_count * int(Fraction(32, 25) * sample_rate)
        if total_samples < minimum_timeline:
            raise ValueError(
                "duration_seconds is too short for the requested sentence count "
                "and 1.2 s minimum display time plus 0.08 s gaps"
            )
    total_samples = _duration_for(
        rng,
        sentence_count=sentence_count,
        total_samples=total_samples,
        sample_rate=sample_rate,
    )
    frame_period = Fraction(rate.denominator * sample_rate, rate.numerator)
    timestamps: list[int] = []
    frame_index = 0
    while True:
        value = (Fraction(frame_index) * frame_period)
        timestamp = (value.numerator * 2 + value.denominator) // (
            value.denominator * 2
        )
        if timestamp >= total_samples:
            break
        timestamps.append(timestamp)
        frame_index += 1
    if not timestamps:
        timestamps = [0]

    destination = Path(out_dir)
    destination.mkdir(parents=True, exist_ok=True)
    audio_path = destination / "audio.wav"
    frames_path = destination / "frames.bin"
    audio_sha256 = _write_silence_like_wav(
        audio_path,
        total_samples=total_samples,
        channels=channels,
        seed=seed,
    )
    frame_table = write_frame_table(
        frames_path,
        timestamps,
        sample_rate=sample_rate,
        frame_rate=rate,
    )
    tracks: dict[str, Any] = {}
    windows_by_language: dict[str, list[tuple[int, int]]] = {}
    for language in language_list:
        per_language_rng = random.Random((seed << 8) ^ int(hashlib.sha256(language.encode()).hexdigest()[:16], 16))
        sentences, windows = _speech_track(
            per_language_rng,
            language,
            count=sentence_count,
            total_samples=total_samples,
            sample_rate=sample_rate,
            contention_permille=contention_permille,
            read_speed_min=read_speed_min,
            read_speed_max=read_speed_max,
        )
        tracks[language] = {
            "language": language,
            "default_max_read_speed": 14,
            "sentences": sentences,
        }
        windows_by_language[language] = windows
    description_count = max(
        0, (sentence_count * description_density_permille + 999) // 1000
    )
    layout_params = params if isinstance(params, LayoutParams) else LayoutParams.from_dict(params)
    layout_params.validate()
    description_rng = random.Random(seed ^ 0xD35C)
    host_windows = windows_by_language[language_list[0]]
    gap_ticks = int(Fraction(2, 25) * sample_rate)
    source_gaps: list[tuple[int, int]] = []
    cursor = 0
    for start, end in sorted(host_windows):
        free_end = start - gap_ticks
        if free_end > cursor:
            source_gaps.append((cursor, free_end))
        cursor = max(cursor, end + gap_ticks)
    if total_samples > cursor:
        source_gaps.append((cursor, total_samples))
    description_slots = _description_slots(
        tracks[language_list[0]]["sentences"],
        total_samples=total_samples,
        sample_rate=sample_rate,
        gap_ticks=gap_ticks,
        speech_min_seconds=layout_params.min_display_seconds,
        description_min_seconds=layout_params.description_min_seconds,
        description_speed=layout_params.description_read_speed,
    )
    descriptions = _descriptions(
        description_rng,
        language_list[0],
        count=description_count,
        total_samples=total_samples,
        sample_rate=sample_rate,
        gaps=source_gaps,
        slots=description_slots,
        description_speed=layout_params.description_read_speed,
        description_min_seconds=layout_params.description_min_seconds,
        contention_permille=contention_permille,
    )
    bundle: dict[str, Any] = {
        "schema_version": 1,
        "seed": seed,
        "shape": {
            "sentence_count": sentence_count,
            "languages": language_list,
            "channels": channels,
            "sample_rate": sample_rate,
            "frame_rate": int(rate) if rate.denominator == 1 else f"{rate.numerator}/{rate.denominator}",
            "description_density_permille": description_density_permille,
            "contention_permille": contention_permille,
            "read_speed_min": read_speed_min,
            "read_speed_max": read_speed_max,
        },
        "timebase": {
            "unit": "sample",
            "sample_rate": sample_rate,
            "ticks_per_second": sample_rate,
        },
        "audio": {
            "path": "audio.wav",
            "sample_rate": sample_rate,
            "channels": channels,
            "sample_width": 2,
            "total_samples": total_samples,
            "sha256": audio_sha256,
        },
        "frame_table": {
            "path": "frames.bin",
            "frame_rate": int(rate) if rate.denominator == 1 else f"{rate.numerator}/{rate.denominator}",
            "frame_count": frame_table.frame_count,
            "sha256": frame_table.sha256,
        },
        "tracks": tracks,
        "descriptions": descriptions,
        "default_speech_priority": 3,
        "default_description_priority": 2,
    }
    bundle_path = destination / "bundle.json"
    from .timebase import unify_timebase

    description_track = language_list[0]
    removed_description_ids: list[str] = []
    while True:
        bundle["descriptions"] = descriptions
        atomic_write_json(bundle_path, bundle)
        normalized = unify_timebase(bundle, base_dir=destination)
        reports: dict[str, dict[str, Any]] = {}
        try:
            for language in language_list:
                reports[language] = layout_track(
                    f"speech-{language}",
                    language,
                    normalized.tracks[language],
                    normalized.descriptions if language == description_track else tuple(),
                    normalized,
                    layout_params,
                    published_version=1,
                    param_version=1,
                )
            break
        except LCFTError as exc:
            if exc.code != "E_DESC_CONFLICT":
                raise
            conflicting = exc.details.get("description_id")
            remaining = [
                item for item in descriptions if item.get("id") != conflicting
            ]
            if len(remaining) == len(descriptions):
                raise
            removed_description_ids.append(str(conflicting))
            descriptions = remaining
    baseline_hashes: dict[str, str] = {
        language: reports[language]["track_hash"] for language in language_list
    }
    for language in language_list:
        atomic_write_json(destination / f"baseline-{language}.json", reports[language])
    bundle["baseline_hashes"] = baseline_hashes
    atomic_write_json(bundle_path, bundle)
    for language in language_list:
        atomic_write_json(
            destination / f"sentences-{language}.json",
            {
                "language": language,
                "sentences": tracks[language]["sentences"],
            },
        )
    manifest = {
        "schema_version": 1,
        "seed": seed,
        "bundle": "bundle.json",
        "bundle_sha256": sha256_bytes(canonical_bytes(bundle) + b"\n"),
        "audio_sha256": audio_sha256,
        "frame_table_sha256": frame_table.sha256,
        "baseline_hashes": baseline_hashes,
        "sentence_count": sentence_count,
        "languages": language_list,
        "description_count": len(descriptions),
        "requested_description_count": description_count,
        "removed_conflict_description_ids": removed_description_ids,
        "channels": channels,
        "read_speed_min": read_speed_min,
        "read_speed_max": read_speed_max,
        "total_samples": total_samples,
    }
    atomic_write_json(destination / "manifest.json", manifest)
    return manifest

