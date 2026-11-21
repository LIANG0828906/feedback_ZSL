from __future__ import annotations

import struct
import wave
from pathlib import Path

import pytest

from subtitle_localizer.errors import BusinessError, E_TIMEBASE_INVALID
from subtitle_localizer.models import SentenceInput, SubtitleAsset
from subtitle_localizer.timebase import (
    TICKS_PER_SECOND,
    format_timecode,
    parse_timecode,
    read_frame_table,
    read_wav_info,
    ticks_to_samples,
    validate_media_and_asset,
)

from .helpers import make_case


def test_timecode_and_sample_conversion_is_exact() -> None:
    ticks = parse_timecode("01:02:03.456789")
    assert ticks % 6 == 0
    assert parse_timecode(format_timecode(ticks)) == ticks
    assert TICKS_PER_SECOND == 6_000_000
    assert ticks_to_samples(parse_timecode("00:00:01.000000")) == 48_000


def test_wav_and_frame_table_use_common_timebase(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path)
    asset = SubtitleAsset.load(case_dir / "subtitles.json")
    wav = read_wav_info(case_dir / "program.wav")
    frames = read_frame_table(case_dir / "frames.ftbl")
    timings = validate_media_and_asset(asset, wav, frames)
    first = timings[0]
    assert wav.sample_rate == 48_000
    assert wav.channels == 1
    assert first.orig_start_sample == 48_000
    assert first.frame_start == 24
    assert first.frame_end == frames.frame_for_sample(first.orig_end_sample)


def test_six_channel_wav_is_supported(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path, channels=6)
    wav = read_wav_info(case_dir / "program.wav")
    assert wav.channels == 6
    assert wav.block_align == 12


def test_wrong_sample_rate_returns_timebase_error(tmp_path: Path) -> None:
    path = tmp_path / "bad.wav"
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(1)
        handle.setsampwidth(2)
        handle.setframerate(44_100)
        handle.writeframes(b"\x00\x00" * 100)
    with pytest.raises(BusinessError) as caught:
        read_wav_info(path)
    assert caught.value.code == E_TIMEBASE_INVALID
    assert caught.value.context["field"] == "wav.sample_rate"


def test_out_of_range_timecode_locates_sentence_and_field(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path)
    asset = SubtitleAsset.load(case_dir / "subtitles.json")
    first = asset.sentences[0]
    bad = first.__class__(
        id=first.id,
        text=first.text,
        start=first.start,
        end="00:01:00.000000",
        speaker=first.speaker,
        cps=first.cps,
    )
    malformed = asset.replace(sentences=(bad, *asset.sentences[1:]))
    with pytest.raises(BusinessError) as caught:
        validate_media_and_asset(
            malformed,
            read_wav_info(case_dir / "program.wav"),
            read_frame_table(case_dir / "frames.ftbl"),
        )
    assert caught.value.code == E_TIMEBASE_INVALID
    assert caught.value.context["sentence_id"] == "S1"
    assert caught.value.context["field"] == "end"


def test_frame_table_rejects_non_monotonic_records(tmp_path: Path) -> None:
    path = tmp_path / "bad.ftbl"
    header = b"FTBL0001" + struct.pack("<IIIII", 1, 24, 1, 2, 0)
    path.write_bytes(header + struct.pack("<QQ", 0, 0))
    with pytest.raises(BusinessError) as caught:
        read_frame_table(path)
    assert caught.value.code == E_TIMEBASE_INVALID
    assert caught.value.context["field"] == "frames.positions"

