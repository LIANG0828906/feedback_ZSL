"""逐段质量统计与逐样本一致性比对。"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from ..errors import WorkbenchError


def _db(ratio: float) -> float:
    return float(10.0 * np.log10(max(ratio, 1e-20)))


@dataclass(frozen=True)
class SegmentStats:
    window_index: int
    snr_before_db: float
    snr_after_db: float
    snr_gain_db: float
    residual_std: float


def segment_stats(
    noisy: np.ndarray,
    restored: np.ndarray,
    clean: np.ndarray,
    silence_ranges: list[tuple[int, int]],
    window_index: int,
) -> SegmentStats:
    """基于干净参考信号计算 SNR 提升；静音段计算噪声基底残留标准差。

    所有数组均为当前窗口切片（长度相同）；静音区间使用窗口内局部偏移。
    """

    def snr(observed: np.ndarray) -> float:
        signal_power = float(np.mean(clean ** 2))
        noise = observed - clean
        noise_power = float(np.mean(noise ** 2))
        return _db(signal_power / max(noise_power, 1e-20))

    before = snr(noisy)
    after = snr(restored)

    residual_values: list[np.ndarray] = []
    for start, end in silence_ranges:
        local_start = max(int(start), 0)
        local_end = min(int(end), restored.shape[0])
        if local_end > local_start:
            residual_values.append(restored[local_start:local_end] - clean[local_start:local_end])
    if residual_values:
        residual_std = float(np.std(np.concatenate([v.ravel() for v in residual_values])))
    else:
        residual_std = float(np.std((restored - clean).ravel()))

    return SegmentStats(
        window_index=window_index,
        snr_before_db=before,
        snr_after_db=after,
        snr_gain_db=after - before,
        residual_std=residual_std,
    )


def locate_first_diff(left: np.ndarray, right: np.ndarray, window_starts: list[int]) -> dict:
    """定位首个不一致采样位置（全局索引）与所属窗口编号，一致返回 None。"""
    if left.shape != right.shape:
        return {
            "sample_index": 0,
            "window_index": 0,
            "left_shape": list(left.shape),
            "right_shape": list(right.shape),
        }
    unequal = ~np.isclose(left, right, rtol=0.0, atol=0.0)
    flat = np.flatnonzero(unequal.any(axis=tuple(range(1, unequal.ndim))))
    if flat.size == 0:
        return None
    index = int(flat[0])
    window_index = 0
    for window_index, start in enumerate(window_starts):
        if start > index:
            window_index -= 1
            break
    return {
        "sample_index": index,
        "window_index": max(window_index, 0),
        "left_value": left[index].tolist(),
        "right_value": right[index].tolist(),
    }


def assert_samples_equal(
    left: np.ndarray, right: np.ndarray, window_starts: list[int], context: str
) -> None:
    diff = locate_first_diff(left, right, window_starts)
    if diff is not None:
        raise WorkbenchError(
            "E_DIFF",
            f"{context} 输出逐样本不一致",
            f"samples[{diff['sample_index']}]",
            {"first_diff": diff},
        )
