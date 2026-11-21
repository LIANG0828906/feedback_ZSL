"""逐帧逐像素误差上界推导（严格区间传播）。

每个像素的每个通道携带一个值区间 ``[lo, hi]``，沿六步链路传播：

1. linearize   源码值区间 [x-0.5/code_max, x+0.5/code_max] 反演；
               定点路径额外叠加 QF 量化半宽
2. primaries   区间矩阵乘（M-P 矩阵，输出每通道独立取 min/max），
               定点路径再计入矩阵系数舍入与输出 QF 量化
3. adapt       白点适合同样按区间矩阵乘传播
4. tonemap     对区间两端点分别求值并截断到 [0,1]，天然包住曲线截断
5. encode      对区间两端点分别做目标传递函数编码
6. quantize    信号区间折算到码值后加半码值舍入，定点再加 QF 折算项

每一步同时记录该步累计半宽（码值单位），用于“最早越界环节”归因；
出现 NaN/Inf 等无法证明有界的情况抛 E_ERROR_UNBOUNDED 并附候选因子。
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from . import color
from .errors import ErrorUnboundedError
from .fixedpoint import Quantizer, quantize_matrix
from .pipeline import STEP_NAMES


@dataclass
class PixelErrorReport:
    frame_index: int
    bound_codes: np.ndarray
    stage_bounds: dict
    untrusted: np.ndarray
    first_offending_stage: np.ndarray


def _finite_or_raise(array: np.ndarray, frame_index: int, candidates):
    if not np.all(np.isfinite(array)):
        raise ErrorUnboundedError(
            "误差传播出现非有限值，无法证明有界",
            candidates=candidates,
            bad_frame=frame_index,
        )


def _interval_matmul(lo: np.ndarray, hi: np.ndarray, matrix: np.ndarray):
    """区间向量乘矩阵：输出通道 j 取 8 个角点组合的 min/max。"""
    neg = np.minimum(matrix, 0.0)
    pos = np.maximum(matrix, 0.0)
    out_lo = np.matmul(hi, neg) + np.matmul(lo, pos)
    out_hi = np.matmul(lo, neg) + np.matmul(hi, pos)
    return out_lo, out_hi


def _half_width(lo: np.ndarray, hi: np.ndarray) -> np.ndarray:
    return 0.5 * np.abs(hi - lo)


def compute_frame_error_bound(
    frame_index: int,
    signal: np.ndarray,
    header,
    config,
    matrix: np.ndarray,
    adapt_matrix: np.ndarray,
    threshold_codes: float,
) -> PixelErrorReport:
    signal = np.asarray(signal, dtype=np.float64)
    source_step = 1.0 / float(header.code_max)
    code_max = float((1 << config.target_bit_depth) - 1)
    stage_bounds = {}
    candidates = [
        "linearize_step",
        "matrix_coefficient_rounding",
        "white_point_adaptation",
        "tone_curve_truncation",
        "output_quantization",
    ]

    if config.mode == "fixed":
        quantizer = Quantizer(
            config.frac_bits, config.rounding, config.seed
        )
        q = quantizer.step
        fixed_matrix = quantize_matrix(matrix, quantizer)
        fixed_adapt = quantize_matrix(adapt_matrix, quantizer)
    else:
        q = 0.0
        fixed_matrix = matrix
        fixed_adapt = adapt_matrix

    # 1) 线性化区间（真实中心 + 源区间半宽，定点叠加 QF 量化半宽）
    eps = 0.5 * source_step
    sig_lo = np.clip(signal - eps, 0.0, 1.0)
    sig_hi = np.clip(signal + eps, 0.0, 1.0)
    lin_lo = color.linearize(sig_lo, header.transfer)
    lin_hi = color.linearize(sig_hi, header.transfer)
    linear_center = color.linearize(signal, header.transfer)
    lin_half = _half_width(lin_lo, lin_hi)
    if config.mode == "fixed":
        lin_half = lin_half + 0.5 * q
    lin_lo, lin_hi = linear_center - lin_half, linear_center + lin_half
    _finite_or_raise(lin_hi, frame_index, candidates)
    stage_bounds["linearize"] = lin_half * code_max

    # 2) 基色矩阵：区间乘包络 + 系数舍入贡献 + 输出 QF 量化，真实中心定位
    coeff_error = np.abs(matrix - fixed_matrix)
    coeff_term = np.matmul(
        np.maximum(np.abs(lin_lo), np.abs(lin_hi)), coeff_error
    )
    pri_lo, pri_hi = _interval_matmul(lin_lo, lin_hi, fixed_matrix)
    pri_half = _half_width(pri_lo, pri_hi) + coeff_term
    if config.mode == "fixed":
        pri_half = pri_half + 0.5 * q
    primary_center = np.matmul(linear_center, fixed_matrix)
    pri_lo, pri_hi = primary_center - pri_half, primary_center + pri_half
    _finite_or_raise(pri_hi, frame_index, candidates)
    stage_bounds["primaries"] = pri_half * code_max

    # 3) 白点适应
    adapt_error = np.abs(adapt_matrix - fixed_adapt)
    adapt_term = np.matmul(
        np.maximum(np.abs(pri_lo), np.abs(pri_hi)), adapt_error
    )
    adp_lo, adp_hi = _interval_matmul(pri_lo, pri_hi, fixed_adapt)
    adp_half = _half_width(adp_lo, adp_hi) + adapt_term
    if config.mode == "fixed":
        adp_half = adp_half + 0.5 * q
    adapt_center = np.matmul(primary_center, fixed_adapt)
    adp_lo, adp_hi = adapt_center - adp_half, adapt_center + adp_half
    _finite_or_raise(adp_hi, frame_index, candidates)
    stage_bounds["adapt"] = adp_half * code_max

    # 4) 色调映射：保留上游全半宽（包络），真实中心定位，截断计入
    map_lo = color.tone_map(
        np.maximum(adp_lo, 0.0),
        config.tone_curve,
        header.peak_nits,
        config.target_peak_nits,
    )
    map_hi = color.tone_map(
        np.maximum(adp_hi, 0.0),
        config.tone_curve,
        header.peak_nits,
        config.target_peak_nits,
    )
    map_center = color.tone_map(
        adapt_center,
        config.tone_curve,
        header.peak_nits,
        config.target_peak_nits,
    )
    map_delta = 1e-7
    map_slope = (
        color.tone_map(
            np.maximum(adapt_center, 0.0) + map_delta,
            config.tone_curve,
            header.peak_nits,
            config.target_peak_nits,
        )
        - map_center
    ) / map_delta
    map_half = np.maximum(
        _half_width(map_lo, map_hi),
        adp_half * np.abs(map_slope),
    )
    if config.mode == "fixed":
        map_half = map_half + 0.5 * q
    map_lo, map_hi = map_center - map_half, map_center + map_half
    _finite_or_raise(map_hi, frame_index, candidates)
    stage_bounds["tonemap"] = map_half * code_max

    # 5) 目标传递函数编码：上游半宽经编码局部斜率折算（中心差分），
    #    再与区间包络取 max，避免凸函数暗部包络低估；定点加编码 QF 量化
    enc_lo = color.encode(np.maximum(map_lo, 0.0), config.target_transfer)
    enc_hi = color.encode(np.maximum(map_hi, 0.0), config.target_transfer)
    enc_center = color.encode(
        np.maximum(map_center, 0.0), config.target_transfer
    )
    enc_delta = 1e-9
    enc_slope = (
        color.encode(
            np.maximum(map_center, 0.0) + enc_delta, config.target_transfer
        )
        - enc_center
    ) / enc_delta
    enc_half = np.maximum(
        _half_width(enc_lo, enc_hi),
        map_half * np.abs(enc_slope),
    )
    if config.mode == "fixed":
        enc_half = enc_half + 0.5 * q
    enc_lo, enc_hi = enc_center - enc_half, enc_center + enc_half
    _finite_or_raise(enc_hi, frame_index, candidates)
    stage_bounds["encode"] = enc_half * code_max

    # 6) 输出量化：信号半宽折算码值 + 半码值舍入；
    #    定点编码后的 QF 再量化已在信号域（enc_half），
    #    另外两条路径各自独立做半码值舍入，舍入方向可能相反，再补半码值
    bound_codes = enc_half * code_max + 0.5
    if config.mode == "fixed":
        bound_codes = bound_codes + 0.5
    _finite_or_raise(bound_codes, frame_index, candidates)
    stage_bounds["quantize"] = bound_codes

    untrusted = bound_codes > threshold_codes
    first_stage = np.full(bound_codes.shape, None, dtype=object)
    for step in STEP_NAMES:
        offending = (stage_bounds[step] > threshold_codes) & np.equal(
            first_stage, None
        )
        first_stage[offending] = step

    return PixelErrorReport(
        frame_index=frame_index,
        bound_codes=bound_codes,
        stage_bounds=stage_bounds,
        untrusted=untrusted,
        first_offending_stage=first_stage,
    )

