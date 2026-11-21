"""色彩科学原语：传递函数、基色矩阵、白点适应、色调映射。

全部浮点计算使用 float64。基色与白点取自常用规范值：

- Rec.709 / sRGB：D65，标准 709 原色
- Rec.2020：D65，BT.2020 原色
- Display-P3：D65，SMPTE RP 431-2 P3 原色

三种空间白点都是 D65，因此默认白点适应为恒等；
Bradford 链路保留，用于自定义白点或非 D65 素材的扩展验证。
"""

from __future__ import annotations

import hashlib

import numpy as np

from .hdrformat import (
    CS_DISPLAY_P3,
    CS_REC2020,
    CS_REC709,
    TF_HLG,
    TF_PQ,
    TF_SDR,
)

D65_XY = (0.3127, 0.3290)

# (红原色 xy, 绿原色 xy, 蓝原色 xy, 白点 xy)
CHROMATICITIES = {
    CS_REC709: (
        (0.640, 0.330),
        (0.300, 0.600),
        (0.150, 0.060),
        D65_XY,
    ),
    CS_REC2020: (
        (0.708, 0.292),
        (0.170, 0.797),
        (0.131, 0.046),
        D65_XY,
    ),
    CS_DISPLAY_P3: (
        (0.680, 0.320),
        (0.265, 0.690),
        (0.150, 0.060),
        D65_XY,
    ),
}

# PQ 常量（SMPTE ST 2084）
PQ_M1 = 0.1593017578125
PQ_M2 = 78.84375
PQ_C1 = 0.8359375
PQ_C2 = 18.8515625
PQ_C3 = 18.6875

# HLG 常量（BT.2100）
HLG_A = 0.17883277
HLG_B = 0.28466892
HLG_C = 0.55991073

BRADFORD = np.array(
    [
        [0.8951, 0.2664, -0.1614],
        [-0.7502, 1.7135, 0.0367],
        [0.0389, -0.0685, 1.0296],
    ],
    dtype=np.float64,
)
BRADFORD_INV = np.linalg.inv(BRADFORD)


def _xy_to_xyz(xy):
    x, y = xy
    if y == 0:
        return np.array([0.0, 0.0, 0.0], dtype=np.float64)
    return np.array([x / y, 1.0, (1.0 - x - y) / y], dtype=np.float64)


def rgb_to_xyz_matrix(colorspace: int) -> np.ndarray:
    """由原色与白点推导线性 RGB -> XYZ（白点归一）矩阵。"""
    if colorspace not in CHROMATICITIES:
        raise KeyError(f"未知色彩空间码 {colorspace}")
    rx, gx, bx, white = CHROMATICITIES[colorspace]
    primaries = np.column_stack(
        [_xy_to_xyz(rx), _xy_to_xyz(gx), _xy_to_xyz(bx)]
    )
    white_xyz = _xy_to_xyz(white)
    scales = np.linalg.solve(primaries, white_xyz)
    return primaries * scales


def xyz_to_rgb_matrix(colorspace: int) -> np.ndarray:
    return np.linalg.inv(rgb_to_xyz_matrix(colorspace))


def white_adaptation_matrix(
    source_xy=D65_XY, target_xy=D65_XY
) -> np.ndarray:
    """Bradford 锥响应适应矩阵；同白点时为单位阵。"""
    if tuple(source_xy) == tuple(target_xy):
        return np.eye(3, dtype=np.float64)
    src = _xy_to_xyz(source_xy)
    dst = _xy_to_xyz(target_xy)
    src_cone = BRADFORD @ src
    dst_cone = BRADFORD @ dst
    gain = np.diag(dst_cone / src_cone)
    return BRADFORD_INV @ gain @ BRADFORD


def color_transform_matrix(source_cs: int, target_cs: int) -> np.ndarray:
    """完整线性域转换：src RGB -> XYZ -> 白点适应 -> dst RGB。"""
    src_white = CHROMATICITIES[source_cs][3]
    dst_white = CHROMATICITIES[target_cs][3]
    adapt = white_adaptation_matrix(src_white, dst_white)
    return xyz_to_rgb_matrix(target_cs) @ adapt @ rgb_to_xyz_matrix(source_cs)


# ---------------------------------------------------------------------------
# 传递函数：信号 [0,1] <-> 线性光 [0, ...]
# ---------------------------------------------------------------------------


def srgb_oetf_inverse(signal: np.ndarray) -> np.ndarray:
    signal = np.asarray(signal, dtype=np.float64)
    return np.where(
        signal <= 0.04045,
        signal / 12.92,
        np.power(np.maximum(signal + 0.055, 0.0) / 1.055, 2.4),
    )


def srgb_oetf(linear: np.ndarray) -> np.ndarray:
    linear = np.asarray(linear, dtype=np.float64)
    return np.where(
        linear <= 0.0031308,
        12.92 * linear,
        1.055 * np.power(np.maximum(linear, 0.0), 1.0 / 2.4) - 0.055,
    )


def pq_oetf_inverse(signal: np.ndarray) -> np.ndarray:
    """PQ 信号 -> 归一化线性光（1.0 对应 10000 cd/m^2）。"""
    signal = np.clip(np.asarray(signal, dtype=np.float64), 0.0, 1.0)
    power = np.power(signal, 1.0 / PQ_M2)
    numerator = np.maximum(power - PQ_C1, 0.0)
    denominator = PQ_C2 - PQ_C3 * power
    return np.power(numerator / denominator, 1.0 / PQ_M1)


def pq_oetf(linear: np.ndarray) -> np.ndarray:
    linear = np.clip(np.asarray(linear, dtype=np.float64), 0.0, None)
    powered = np.power(linear, PQ_M1)
    return np.power(
        (PQ_C1 + PQ_C2 * powered) / (1.0 + PQ_C3 * powered), PQ_M2
    )


def hlg_oetf_inverse(signal: np.ndarray) -> np.ndarray:
    """HLG 场景线性信号反演，输出场景线性光。"""
    signal = np.clip(np.asarray(signal, dtype=np.float64), 0.0, 1.0)
    return np.where(
        signal <= 0.5,
        np.square(signal) / 3.0,
        (np.exp((signal - HLG_C) / HLG_A) + HLG_B) / 12.0,
    )


def hlg_oetf(scene_linear: np.ndarray) -> np.ndarray:
    scene_linear = np.clip(
        np.asarray(scene_linear, dtype=np.float64), 0.0, None
    )
    return np.where(
        scene_linear <= 1.0 / 12.0,
        np.sqrt(np.maximum(3.0 * scene_linear, 0.0)),
        HLG_A * np.log(np.maximum(12.0 * scene_linear - HLG_B, 1e-12))
        + HLG_C,
    )


def linearize(signal: np.ndarray, transfer: int) -> np.ndarray:
    if transfer == TF_SDR:
        return srgb_oetf_inverse(signal)
    if transfer == TF_PQ:
        return pq_oetf_inverse(signal)
    if transfer == TF_HLG:
        return hlg_oetf_inverse(signal)
    raise KeyError(f"未知传递函数码 {transfer}")


def encode(linear: np.ndarray, transfer: int) -> np.ndarray:
    if transfer == TF_SDR:
        return srgb_oetf(linear)
    if transfer == TF_PQ:
        return pq_oetf(linear)
    if transfer == TF_HLG:
        return hlg_oetf(linear)
    raise KeyError(f"未知传递函数码 {transfer}")


# ---------------------------------------------------------------------------
# 色调映射（线性域，逐通道；输入/输出均为归一化线性光）
# ---------------------------------------------------------------------------

TONE_REINHARD = "reinhard"
TONE_HABLE = "hable"
TONE_BT2446 = "bt2446"
TONE_CURVES = (TONE_REINHARD, TONE_HABLE, TONE_BT2446)


def _hable_unary(x: np.ndarray) -> np.ndarray:
    a = 0.15
    b = 0.50
    c = 0.10
    d = 0.20
    e = 0.02
    f = 0.30
    return ((x * (a * x + c * b) + d * e) / (x * (a * x + b) + d * f)) - e / f


def tone_map(
    linear: np.ndarray,
    curve: str,
    source_peak_nits: float,
    target_peak_nits: float,
) -> np.ndarray:
    """按源/目标峰值比做曝光归一后套用滚降曲线。"""
    linear = np.asarray(linear, dtype=np.float64)
    exposure = float(target_peak_nits) / float(source_peak_nits)
    scaled = np.maximum(linear * exposure, 0.0)
    if curve == TONE_REINHARD:
        return scaled / (1.0 + scaled)
    if curve == TONE_HABLE:
        white = _hable_unary(np.array(exposure, dtype=np.float64))
        return _hable_unary(scaled) / white
    if curve == TONE_BT2446:
        # BT.2446 Method A 风格的亮部压缩，保留暗部近似线性
        knee = 0.75
        p = 1.0770
        q = 0.0153
        mapped = np.where(
            scaled <= knee,
            scaled,
            p * np.log(np.maximum(scaled - q, 1e-12))
            + (1.0 - p * np.log(knee - q)),
        )
        return np.maximum(mapped, 0.0)
    raise KeyError(f"未知色调映射曲线 {curve}")


def matrix_hash(matrix: np.ndarray) -> str:
    """矩阵内容哈希：规范化字节（C 连续、float64 小端）后取 SHA-256。"""
    canonical = np.ascontiguousarray(matrix, dtype="<f8").tobytes()
    return hashlib.sha256(canonical).hexdigest()

