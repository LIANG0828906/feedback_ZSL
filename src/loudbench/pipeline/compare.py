"""三条执行路径（并行 / 串行 / 断点恢复）逐样本一致性比对。"""
from __future__ import annotations

import numpy as np

from ..core.errors import DiffError

#: float64 逐样本严格一致的容差（仅吸收文件序列化误差，内存比对用 0）
DEFAULT_ATOL = 0.0
DEFAULT_RTOL = 0.0


def assert_outputs_equal(outputs_a: list, outputs_b: list,
                         atol: float = DEFAULT_ATOL,
                         rtol: float = DEFAULT_RTOL) -> None:
    """比对两段式输出列表；不一致抛 E_DIFF 并定位段号/采样位置。"""
    if len(outputs_a) != len(outputs_b):
        raise DiffError(
            f"输出段数不一致 {len(outputs_a)} != {len(outputs_b)}",
            segment_index=-1, sample_index=-1,
            expected=len(outputs_a), actual=len(outputs_b))
    for oa, ob in zip(outputs_a, outputs_b):
        a, b = oa.output_samples, ob.output_samples
        if a.shape != b.shape:
            raise DiffError(
                f"段 {oa.index} 形状不一致 {a.shape} != {b.shape}",
                segment_index=oa.index, sample_index=-1,
                expected=float(a.size), actual=float(b.size))
        diff = ~np.isclose(a, b, atol=atol, rtol=rtol, equal_nan=True)
        if np.any(diff):
            first = int(np.argmax(diff))
            raise DiffError(
                f"段 {oa.index} 首个不一致采样位于位置 {first}",
                segment_index=oa.index, sample_index=first,
                expected=float(a.flat[first]), actual=float(b.flat[first]))


def assert_concat_equal(audio_a: np.ndarray, audio_b: np.ndarray,
                        segment_bounds: list[int], atol: float = 1e-12
                        ) -> None:
    """比对拼接后的整段音频，并把全局位置映射回段编号。"""
    if audio_a.shape != audio_b.shape:
        raise DiffError(
            f"拼接音频形状不一致 {audio_a.shape} != {audio_b.shape}",
            segment_index=-1, sample_index=-1,
            expected=float(audio_a.size), actual=float(audio_b.size))
    diff = ~np.isclose(audio_a, audio_b, atol=atol, rtol=0.0,
                       equal_nan=True)
    if np.any(diff):
        first = int(np.argmax(diff))
        seg = 0
        for idx, bound in enumerate(segment_bounds):
            if first < bound:
                seg = idx
                break
        raise DiffError(
            f"拼接音频首个不一致采样全局位置 {first}（段 {seg}）",
            segment_index=seg, sample_index=first,
            expected=float(audio_a.flat[first]),
            actual=float(audio_b.flat[first]))


def assert_reports_equal(report_a: dict, report_b: dict,
                         atol: float = 1e-9) -> None:
    """响度报告数值比对（键顺序无关，数值带容差）。"""
    if report_a.keys() != report_b.keys():
        raise DiffError("报告顶层键不一致", segment_index=-1,
                        sample_index=-1, expected=0, actual=0)
    for key in report_a:
        va, vb = report_a[key], report_b[key]
        if isinstance(va, float) or isinstance(vb, float):
            if not np.isclose(va, vb, atol=atol, rtol=0.0,
                              equal_nan=True):
                raise DiffError(
                    f"报告字段 {key} 不一致: {va} != {vb}",
                    segment_index=-1, sample_index=-1,
                    expected=va, actual=vb)
        elif va != vb:
            raise DiffError(
                f"报告字段 {key} 不一致: {va} != {vb}",
                segment_index=-1, sample_index=-1, expected=0, actual=0)
