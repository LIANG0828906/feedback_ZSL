"""切片产物生成与逐片内容哈希（本地确定性实现）。

重采样采用相位对齐的线性插值（无随机抖动、无墙钟输入），增益使用 Q8
定点乘法，边界由共享微秒时基与量化刻度整除得到。同档位同参数版本重复
生成的序列化字节逐字节一致，内容哈希仅由切片内容序列化产生。
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import Callable

import numpy as np

from .parameters import TierParams
from .timebase import TICKS_PER_SECOND, SourceTimebase
from .util import bytes_hash, canonical_json, content_hash

RESAMPLE_ALGO = "linear-v1"


@dataclass(frozen=True)
class SliceArtifact:
    """单个切片产物及其在共享源时基上的覆盖范围。"""

    tier: str
    param_version: int
    slice_index: int
    source_start_sample: int
    source_end_sample: int  # 不含端点
    source_start_tick: int
    source_end_tick: int
    target_rate: int
    target_sample_count: int
    frame_ids: tuple[int, ...]
    audio: np.ndarray  # int16, shape=(target_sample_count, channels)
    audio_hash: str
    content_hash_value: str

    def content_payload(self) -> dict:
        """参与内容哈希的完整序列化载荷。

        不含任何墙钟时间戳、文件路径，也不含参数版本号——版本号是版本
        模型的元数据，回退到相同参数时同一切片内容必须哈希一致。
        """
        return {
            "algo": RESAMPLE_ALGO,
            "tier": self.tier,
            "slice_index": self.slice_index,
            "source_start_sample": self.source_start_sample,
            "source_end_sample": self.source_end_sample,
            "source_start_tick": self.source_start_tick,
            "source_end_tick": self.source_end_tick,
            "target_rate": self.target_rate,
            "target_sample_count": self.target_sample_count,
            "frame_ids": list(self.frame_ids),
            "channels": int(self.audio.shape[1]),
            "audio_hex": self.audio.astype("<i2").tobytes().hex(),
            "audio_hash": self.audio_hash,
        }

    def manifest_row(self, graph_version: int) -> dict:
        """写入索引清单的行（内容哈希独立于图谱版本，保证可比对）。"""
        return {
            "tier": self.tier,
            "slice_index": self.slice_index,
            "param_version": self.param_version,
            "graph_version": graph_version,
            "source_start_sample": self.source_start_sample,
            "source_end_sample": self.source_end_sample,
            "source_start_tick": self.source_start_tick,
            "source_end_tick": self.source_end_tick,
            "target_rate": self.target_rate,
            "target_sample_count": self.target_sample_count,
            "frame_ids": list(self.frame_ids),
            "audio_hash": self.audio_hash,
            "content_hash": self.content_hash_value,
        }


def quantized_slice_bounds(
    source: SourceTimebase, params: TierParams
) -> list[tuple[int, int]]:
    """按量化刻度与切片跨度划分共享时基，返回 (start_tick, end_tick) 列表。

    所有边界都吸附到 ``quantum_ticks`` 网格；素材末尾不足一个跨度时，
    以素材实际时长收尾，并校验末尾仍落在网格上（否则由跨档校验报告偏移）。
    """

    duration = source.duration_ticks
    quantum = params.quantum_ticks
    span = params.slice_span_ticks
    bounds: list[tuple[int, int]] = []
    cursor = 0
    while cursor < duration:
        end = min(cursor + span, duration)
        bounds.append((cursor, end))
        cursor = end
    return bounds


def linear_resample(
    source_audio: np.ndarray,
    source_rate: int,
    target_rate: int,
    source_start_tick: int,
    source_end_tick: int,
) -> np.ndarray:
    """相位对齐线性插值重采样，返回 int16 缓冲。

    输出第 ``k`` 个样本对应的源时间为
    ``start_tick + k * TICKS_PER_SECOND / target_rate``，
    源位置以分数样本表达，端点钳制在区间内，全程确定性。
    """

    if source_audio.size == 0:
        channels = source_audio.shape[1] if source_audio.ndim == 2 else 1
        return np.zeros((0, channels), dtype="<i2")

    duration_ticks = source_end_tick - source_start_tick
    n_out = int(
        (Fraction(duration_ticks) * target_rate) // TICKS_PER_SECOND
    )
    if n_out == 0:
        return np.zeros((0, source_audio.shape[1]), dtype="<i2")

    src = source_audio.astype(np.float64)
    n_src = src.shape[0]
    # 输出样本 k 相对区间起点的源样本偏移（分数）。
    phase = (
        np.arange(n_out, dtype=np.float64)
        * TICKS_PER_SECOND
        / target_rate
        * source_rate
        / TICKS_PER_SECOND
    )
    base = (
        float(
            Fraction(source_start_tick)
            * source_rate
            / TICKS_PER_SECOND
        )
    )
    positions = base + phase
    idx0 = np.floor(positions).astype(np.int64)
    idx0 = np.clip(idx0, 0, n_src - 1)
    idx1 = np.clip(idx0 + 1, 0, n_src - 1)
    frac = (positions - np.floor(positions)).reshape(-1, 1)
    out = src[idx0] * (1.0 - frac) + src[idx1] * frac
    return np.rint(out).astype(np.int16)


def apply_gain(audio: np.ndarray, gain_q8: int) -> np.ndarray:
    """Q8 定点增益：乘后四舍五入并饱和到 int16。"""

    if audio.size == 0:
        return audio.copy()
    scaled = np.rint(audio.astype(np.float64) * (gain_q8 / 256.0))
    return np.clip(scaled, -32768, 32767).astype(np.int16)


def frames_in_window(
    source: SourceTimebase, start_tick: int, end_tick: int
) -> tuple[int, ...]:
    """返回与微秒窗口有交集的帧 ID（按共享时基判定）。"""
    ids = [
        iv.frame_id
        for iv in source.intervals
        if iv.start_tick < end_tick and iv.end_tick > start_tick
    ]
    return tuple(ids)


def generate_slice(
    source: SourceTimebase,
    tier: str,
    param_version: int,
    params: TierParams,
    slice_index: int,
    start_tick: int,
    end_tick: int,
) -> SliceArtifact:
    """生成单个切片并计算仅依赖内容的哈希。"""

    start_sample = source.sample_at_tick(start_tick)
    end_sample = source.sample_at_tick(end_tick)
    window = source.samples[start_sample:end_sample]
    resampled = linear_resample(
        window,
        source.sample_rate,
        params.target_rate,
        start_tick,
        end_tick,
    )
    gained = apply_gain(resampled, params.gain_q8)
    frame_ids = frames_in_window(source, start_tick, end_tick)
    audio_hash = bytes_hash(gained.astype("<i2").tobytes())
    artifact = SliceArtifact(
        tier=tier,
        param_version=param_version,
        slice_index=slice_index,
        source_start_sample=start_sample,
        source_end_sample=end_sample,
        source_start_tick=start_tick,
        source_end_tick=end_tick,
        target_rate=params.target_rate,
        target_sample_count=int(gained.shape[0]),
        frame_ids=frame_ids,
        audio=gained,
        audio_hash=audio_hash,
        content_hash_value="",
    )
    digest = content_hash(artifact.content_payload())
    object.__setattr__(artifact, "content_hash_value", digest)
    return artifact


def generate_all(
    source: SourceTimebase,
    tier: str,
    param_version: int,
    params: TierParams,
    should_cancel: Callable[[], bool] | None = None,
) -> list[SliceArtifact]:
    """生成该档位当前参数版本下的全部切片。"""

    artifacts: list[SliceArtifact] = []
    for index, (start_tick, end_tick) in enumerate(
        quantized_slice_bounds(source, params)
    ):
        if should_cancel and should_cancel():
            raise CancelledError(tier, param_version, index)
        artifacts.append(
            generate_slice(
                source, tier, param_version, params, index, start_tick, end_tick
            )
        )
    return artifacts


class CancelledError(RuntimeError):
    """协作式取消信号（区别于异常失败，保留已提交状态）。"""

    def __init__(self, tier: str, param_version: int, slice_index: int) -> None:
        super().__init__(
            f"档位 {tier} v{param_version} 在切片 {slice_index} 被协作式取消"
        )
        self.tier = tier
        self.param_version = param_version
        self.slice_index = slice_index


def serialize_artifact(artifact: SliceArtifact) -> bytes:
    """切片文件字节布局：规范化 JSON（与内容哈希同源）。"""
    return canonical_json(artifact.content_payload())
