from __future__ import annotations

import copy
from fractions import Fraction

import pytest

from lcft.errors import LCFTError
from lcft.loader import load_bundle
from lcft.timebase import (
    Timebase,
    load_frame_table,
    parse_time_value,
    unify_timebase,
)
from lcft.wav import inspect_wav


def test_unified_integer_timebase_and_frame_intervals(
    sample_dir, sample_raw
) -> None:
    result = unify_timebase(sample_raw, base_dir=sample_dir)
    assert result.timebase.sample_rate == 48_000
    assert result.timebase.ticks_per_second == 48_000
    assert result.frames.frame_rate == 30
    sentence = result.tracks["en"][0]
    assert sentence.start_sample == 0
    assert sentence.frame_start == 0
    assert sentence.frame_end >= 1
    rendered = result.to_dict()
    row = rendered["tracks"]["en"][0]
    assert row["start_ticks"] == row["start_sample"]
    assert row["frame_interval"] == [row["frame_start"], row["frame_end"]]
    assert row["start_timecode"].startswith("00:00:00.")


def test_timecode_and_sample_units_convert_exactly() -> None:
    timebase = Timebase()
    assert parse_time_value(
        "00:00:01.500", timebase, Fraction(30), field="x"
    ) == 72_000
    assert parse_time_value(
        "00:00:01:15", timebase, Fraction(30), field="x"
    ) == 72_000
    assert parse_time_value(
        {"value": 1500, "unit": "ms"}, timebase, Fraction(30), field="x"
    ) == 72_000
    assert parse_time_value(
        120, timebase, Fraction(30), field="x", default_unit="seconds"
    ) == 5_760_000


def test_timecode_out_of_range_locates_field(sample_dir, sample_raw) -> None:
    raw = copy.deepcopy(sample_raw)
    raw["tracks"]["en"]["sentences"][0]["end"] = (
        raw["audio"]["total_samples"] + 1
    )
    with pytest.raises(LCFTError) as caught:
        unify_timebase(raw, base_dir=sample_dir)
    assert caught.value.code == "E_TIMEBASE_INVALID"
    assert caught.value.field == "tracks.en.sentences[0].end"


def test_frame_header_count_conflict(sample_dir, sample_raw) -> None:
    raw = copy.deepcopy(sample_raw)
    raw["frame_table"]["frame_count"] += 1
    with pytest.raises(LCFTError) as caught:
        unify_timebase(raw, base_dir=sample_dir)
    assert caught.value.code == "E_TIMEBASE_INVALID"
    assert caught.value.field == "frame_table.frame_count"
    assert caught.value.details["declared_in_bundle"] == (
        raw["frame_table"]["frame_count"]
    )


def test_frame_payload_truncation_is_rejected(sample_dir, sample_raw) -> None:
    frame_path = sample_dir / "frames.bin"
    data = frame_path.read_bytes()
    with open(frame_path, "rb") as handle:
        original = handle.read()
    try:
        frame_path.write_bytes(data[:-3])
        with pytest.raises(LCFTError) as caught:
            unify_timebase(sample_raw, base_dir=sample_dir)
        assert caught.value.field == "frame_table.frame_count"
    finally:
        frame_path.write_bytes(original)


def test_source_timebase_mismatch_is_rejected(sample_dir, sample_raw) -> None:
    raw = copy.deepcopy(sample_raw)
    raw["tracks"]["en"]["sentences"][0]["timebase"] = {"sample_rate": 44_100}
    with pytest.raises(LCFTError) as caught:
        unify_timebase(raw, base_dir=sample_dir)
    assert caught.value.code == "E_TIMEBASE_INVALID"
    assert caught.value.field == "tracks.en.sentences[0].timebase.sample_rate"


def test_mono_and_5_1_pcm_are_accepted(sample_dir, surround_dir) -> None:
    assert inspect_wav(sample_dir / "audio.wav").channels == 1
    info = inspect_wav(surround_dir / "audio.wav")
    assert info.channels == 6
    assert info.sample_rate == 48_000
    assert info.sample_width == 2
    loaded = load_bundle(surround_dir / "bundle.json")
    assert loaded.normalized.audio.channels == 6

