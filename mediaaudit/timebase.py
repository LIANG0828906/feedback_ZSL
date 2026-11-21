"""源素材读取与统一整数时基。

主时基采用整数微秒（``TICKS_PER_SECOND = 1_000_000``），样本索引为
源采样率下的整数样本号。样本↔微秒的换算通过 :class:`fractions.Fraction`
完成，全程不引入浮点误差；除不尽时按“向下取整 + 可复现舍入策略”处理，
保证同一路素材在所有档位上看到的边界完全一致。
"""

from __future__ import annotations

import json
import wave
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import numpy as np

from .errors import AuditError, ErrorCode

TICKS_PER_SECOND = 1_000_000
MIN_SAMPLE_RATE = 16_000
MAX_SAMPLE_RATE = 48_000
VALID_CHANNELS = (1, 2)
SAMPLE_WIDTH_BYTES = 2


def seconds_to_ticks(seconds: float | int) -> int:
    """秒 → 整数微秒，使用舍入到最近偶数（确定性、无平台依赖）。"""
    return int(Fraction(seconds).limit_denominator(10**12) * TICKS_PER_SECOND)


def ticks_to_seconds(ticks: int) -> Fraction:
    return Fraction(ticks, TICKS_PER_SECOND)


@dataclass(frozen=True)
class FrameInterval:
    """单帧在共享整数时基上的样本区间与微秒区间。"""

    frame_id: int
    start_sample: int
    end_sample: int  # 不含端点
    start_tick: int
    end_tick: int  # 不含端点

    @property
    def sample_count(self) -> int:
        return self.end_sample - self.start_sample

    def to_dict(self) -> dict:
        return {
            "frame_id": self.frame_id,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "start_tick": self.start_tick,
            "end_tick": self.end_tick,
        }


@dataclass
class SourceTimebase:
    """解析后的源素材：PCM 缓冲、采样率与逐帧样本区间。"""

    wav_path: Path
    frames_path: Path
    sample_rate: int
    channels: int
    sample_count: int
    samples: np.ndarray  # shape=(sample_count, channels)，int16
    frame_times_ticks: list[int]
    intervals: list[FrameInterval] = field(default_factory=list)

    @property
    def duration_ticks(self) -> int:
        return seconds_to_ticks(Fraction(self.sample_count, self.sample_rate))

    @property
    def tick_per_sample(self) -> Fraction:
        return Fraction(TICKS_PER_SECOND, self.sample_rate)

    def sample_at_tick(self, tick: int) -> int:
        """微秒 → 源样本号（向下取整，钳制到合法范围）。"""
        idx = (Fraction(tick) * self.sample_rate) // TICKS_PER_SECOND
        return max(0, min(self.sample_count, int(idx)))

    def tick_of_sample(self, sample: int) -> int:
        """源样本号 → 微秒（向下取整）。"""
        return int(Fraction(sample) * self.tick_per_sample)

    def to_dict(self) -> dict:
        return {
            "wav_path": str(self.wav_path),
            "frames_path": str(self.frames_path),
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "sample_count": self.sample_count,
            "duration_ticks": self.duration_ticks,
            "frame_count": len(self.intervals),
            "intervals": [iv.to_dict() for iv in self.intervals],
        }


def _fail(message: str, location: tuple[str, ...], details: dict | None = None) -> AuditError:
    return AuditError(ErrorCode.E_SOURCE_INVALID, message, location, details)


def read_pcm_wav(path: str | Path) -> tuple[np.ndarray, int, int]:
    """读取 16-bit PCM WAV，返回 (样本数组[sample,ch], 采样率, 声道数)。

    头字段与数据长度矛盾、采样率/声道/位深非法时抛出
    :class:`AuditError`（``E_SOURCE_INVALID``）并定位首个非法字段。
    """

    wav_path = Path(path)
    if not wav_path.is_file():
        raise _fail("WAV 文件不存在", ("wav", "path"), {"path": str(wav_path)})
    try:
        with wave.open(str(wav_path), "rb") as wf:
            channels = wf.getnchannels()
            sample_width = wf.getsampwidth()
            sample_rate = wf.getframerate()
            n_frames = wf.getnframes()
            raw = wf.readframes(n_frames)
            header_data_len = wf.getnframes() * wf.getnchannels() * wf.getsampwidth()
    except wave.Error as exc:
        raise _fail(f"WAV 头解析失败: {exc}", ("wav", "header")) from exc

    if sample_width != SAMPLE_WIDTH_BYTES:
        raise _fail(
            f"仅支持 16-bit PCM，实际位深 {sample_width * 8}-bit",
            ("wav", "sampwidth"),
            {"sampwidth_bytes": sample_width},
        )
    if channels not in VALID_CHANNELS:
        raise _fail(
            f"仅支持单声道或双声道，实际声道数 {channels}",
            ("wav", "nchannels"),
            {"nchannels": channels},
        )
    if not MIN_SAMPLE_RATE <= sample_rate <= MAX_SAMPLE_RATE:
        raise _fail(
            f"采样率 {sample_rate} 超出 16000–48000 Hz",
            ("wav", "framerate"),
            {"framerate": sample_rate},
        )

    expected_bytes = n_frames * channels * SAMPLE_WIDTH_BYTES
    # wave 库可能按实际数据纠正帧数，因此以头声明字节数与实际字节双重校验。
    header_declared = header_data_len
    if len(raw) != expected_bytes or header_declared != len(raw):
        raise _fail(
            "WAV 头声明的数据长度与实际字节数矛盾",
            ("wav", "data_length"),
            {
                "header_bytes": header_declared,
                "frame_declared_bytes": expected_bytes,
                "actual_bytes": len(raw),
                "n_frames": n_frames,
            },
        )

    samples = np.frombuffer(raw, dtype="<i2").reshape(n_frames, channels).copy()
    return samples, sample_rate, channels


def read_frame_times(path: str | Path) -> list[int]:
    """读取 JSON 帧时刻描述并换算为整数微秒。

    接受 ``{"frames": [{"frame_id": 0, "time_s": 0.0}, ...]}`` 或
    ``[{"frame_id": 0, "time_s": 0.0}, ...]``。帧时刻必须严格单调递增。
    """

    frames_path = Path(path)
    if not frames_path.is_file():
        raise _fail("JSON 帧描述不存在", ("frames", "path"), {"path": str(frames_path)})
    try:
        payload = json.loads(frames_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise _fail(
            f"JSON 解析失败: {exc.msg}",
            ("frames", "json"),
            {"line": exc.lineno, "column": exc.colno},
        ) from exc

    if isinstance(payload, dict) and "frames" in payload:
        raw_frames = payload["frames"]
    elif isinstance(payload, list):
        raw_frames = payload
    else:
        raise _fail("帧描述顶层必须是数组或含 frames 字段的对象", ("frames", "root"))
    if not isinstance(raw_frames, list) or not raw_frames:
        raise _fail("帧列表为空或类型非法", ("frames", "frames"))

    ticks: list[int] = []
    prev: int | None = None
    for idx, item in enumerate(raw_frames):
        loc = ("frames", f"frames[{idx}]", "time_s")
        if not isinstance(item, dict):
            raise _fail("帧条目必须是对象", loc)
        if "time_s" not in item:
            raise _fail("帧条目缺少 time_s 字段", loc)
        value = item["time_s"]
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            raise _fail("time_s 必须是数值类型", loc, {"value": repr(value)})
        if value < 0:
            raise _fail("time_s 不允许为负数", loc, {"value": value})
        tick = seconds_to_ticks(value)
        if prev is not None and tick <= prev:
            raise _fail(
                "帧时刻必须严格单调递增",
                loc,
                {"previous_tick": prev, "current_tick": tick},
            )
        ticks.append(tick)
        prev = tick
    return ticks


def build_intervals(
    sample_rate: int, sample_count: int, frame_ticks: list[int]
) -> list[FrameInterval]:
    """把帧微秒时刻换算为共享时基上的逐帧样本区间。

    帧 ``i`` 覆盖 ``[time_i, time_{i+1})``；最后一帧延伸到素材末尾。
    """

    boundaries = list(frame_ticks)
    duration_ticks = seconds_to_ticks(Fraction(sample_count, sample_rate))
    if boundaries[-1] >= duration_ticks:
        raise _fail(
            "首个帧时刻已超出素材时长",
            ("frames", f"frames[{len(boundaries) - 1}]", "time_s"),
            {"tick": boundaries[-1], "duration_tick": duration_ticks},
        )
    boundaries.append(duration_ticks)

    intervals: list[FrameInterval] = []
    for frame_id in range(len(frame_ticks)):
        start_tick = boundaries[frame_id]
        end_tick = boundaries[frame_id + 1]
        start_sample = (
            Fraction(start_tick) * sample_rate
        ) // TICKS_PER_SECOND
        end_sample = (Fraction(end_tick) * sample_rate) // TICKS_PER_SECOND
        end_sample = min(sample_count, max(start_sample, int(end_sample)))
        intervals.append(
            FrameInterval(
                frame_id=frame_id,
                start_sample=int(start_sample),
                end_sample=int(end_sample),
                start_tick=start_tick,
                end_tick=end_tick,
            )
        )
    return intervals


def load_source(wav_path: str | Path, frames_path: str | Path) -> SourceTimebase:
    """解析 WAV + JSON 帧描述并输出统一整数时基。"""

    samples, sample_rate, channels = read_pcm_wav(wav_path)
    frame_ticks = read_frame_times(frames_path)
    intervals = build_intervals(sample_rate, samples.shape[0], frame_ticks)
    return SourceTimebase(
        wav_path=Path(wav_path),
        frames_path=Path(frames_path),
        sample_rate=sample_rate,
        channels=channels,
        sample_count=samples.shape[0],
        samples=samples,
        frame_times_ticks=frame_ticks,
        intervals=intervals,
    )
