"""要求 1：源素材读取与统一整数时基。"""

from __future__ import annotations

import json
import wave
from pathlib import Path

import numpy as np
import pytest

from mediaaudit.errors import ErrorCode
from mediaaudit.timebase import (
    TICKS_PER_SECOND,
    build_intervals,
    load_source,
    read_frame_times,
    read_pcm_wav,
    seconds_to_ticks,
)


def _write_wav(path: Path, rate: int, channels: int, frames: int, raw: bytes | None = None) -> None:
    with wave.open(str(path), "wb") as wf:
        wf.setnchannels(channels)
        wf.setsampwidth(2)
        wf.setframerate(rate)
        if raw is None:
            raw = np.zeros((frames, channels), dtype="<i2").tobytes()
        wf.writeframes(raw)


def _write_frames(path: Path, times: list[float]) -> None:
    path.write_text(
        json.dumps({"frames": [{"frame_id": i, "time_s": t} for i, t in enumerate(times)]})
    )


def test_loads_mono_and_stereo_and_maps_intervals(tmp_path: Path) -> None:
    wav = tmp_path / "s.wav"
    frames_json = tmp_path / "f.json"
    _write_wav(wav, 16000, 1, 16000)
    _write_frames(frames_json, [0.0, 0.25, 0.5])
    source = load_source(wav, frames_json)
    assert source.sample_rate == 16000
    assert source.sample_count == 16000
    assert [iv.start_sample for iv in source.intervals] == [0, 4000, 8000]
    assert source.intervals[-1].end_sample == 16000
    assert source.intervals[0].start_tick == 0

    wav2 = tmp_path / "s2.wav"
    _write_wav(wav2, 48000, 2, 48000)
    source2 = load_source(wav2, frames_json)
    assert source2.samples.shape == (48000, 2)
    assert source2.intervals[1].start_sample == 12000


def test_integer_ticks_no_float_drift() -> None:
    assert seconds_to_ticks(0.25) == 250_000
    intervals = build_intervals(44100, 44100, [0, seconds_to_ticks(0.1)])
    assert intervals[0].end_sample == 4410
    assert intervals[0].end_tick == 100_000


def test_rejects_sample_rate_out_of_range(tmp_path: Path) -> None:
    wav = tmp_path / "bad.wav"
    _write_wav(wav, 8000, 1, 800)
    with pytest.raises(Exception) as exc:
        read_pcm_wav(wav)
    assert exc.value.code == ErrorCode.E_SOURCE_INVALID
    assert exc.value.location == ("wav", "framerate")


def test_rejects_header_data_length_conflict(tmp_path: Path) -> None:
    wav = tmp_path / "conflict.wav"
    # 手工构造 data 块大小与实际字节矛盾的 WAV（wave 库会自动纠正帧数，
    # 因此直接写二进制头，声明 100 帧却只放 50 帧数据）。
    import struct

    data_blob = b"\x00\x00" * 50
    header = b"RIFF" + struct.pack("<I", 36 + len(data_blob)) + b"WAVE"
    fmt = b"fmt " + struct.pack("<IHHIIHH", 16, 1, 1, 16000, 32000, 2, 16)
    data_header = b"data" + struct.pack("<I", 200)  # 声明 100 帧 * 2 字节
    wav.write_bytes(header + fmt + data_header + data_blob)
    with pytest.raises(Exception) as exc:
        read_pcm_wav(wav)
    assert exc.value.code == ErrorCode.E_SOURCE_INVALID
    assert exc.value.location == ("wav", "data_length")


def test_rejects_non_monotonic_frame_times(tmp_path: Path) -> None:
    frames_json = tmp_path / "f.json"
    _write_frames(frames_json, [0.0, 0.5, 0.4])
    with pytest.raises(Exception) as exc:
        read_frame_times(frames_json)
    assert exc.value.code == ErrorCode.E_SOURCE_INVALID
    assert exc.value.location == ("frames", "frames[2]", "time_s")


def test_rejects_missing_time_field(tmp_path: Path) -> None:
    frames_json = tmp_path / "f.json"
    frames_json.write_text(json.dumps({"frames": [{"frame_id": 0}]}))
    with pytest.raises(Exception) as exc:
        read_frame_times(frames_json)
    assert exc.value.code == ErrorCode.E_SOURCE_INVALID
    assert "time_s" in exc.value.location


def test_tick_per_sample_ratio() -> None:
    wav = Path("/dev/null")  # 仅校验纯函数，无需文件
    intervals = build_intervals(48000, 48000, [0, TICKS_PER_SECOND // 100])
    assert intervals[0].sample_count == 480
