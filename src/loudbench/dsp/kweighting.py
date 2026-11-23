"""K 加权滤波（ITU-R BS.1770-4）与真峰值测量。

K 加权由两级级联组成：

1. 高频搁架：增益 +4 dB，转角 1681.763 Hz（RBJ high-shelf）
2. 高通滤波器：转角 38.13547 Hz，Q=0.5（RBJ 2 阶高通）

两级均通过预畸变双线性变换按实际采样率设计，48 kHz 时数值与
BS.1770-4 附录给出的规范系数一致（见 tests/test_kweighting.py）。
全部计算在 float64 下进行，保证跨进程/跨线程逐样本一致。
"""
from __future__ import annotations

import numpy as np
from scipy import signal

SHELF_GAIN_DB = 4.0
SHELF_FREQ_HZ = 1681.7632228343518
HIGHPASS_FREQ_HZ = 38.13547087602444
HIGHPASS_Q = 0.5


def _high_shelf_sos(fs: int, gain_db: float, freq_hz: float) -> np.ndarray:
    """BS.1770-4 附录 B.1/B.2 高频搁架（stage 1）精确双线性变换系数。

    模拟原型 H(s) = (V*s^2 + (2*sqrt(V))*s + 1) /
                    (s^2 + (2/sqrt(V))*s + 1) 经双线性变换得到，
    分母与标准 Q=1/sqrt(2) 低通原型一致，48 kHz 时与附录数值逐位吻合。
    """
    v_gain = 10.0 ** (gain_db / 20.0)
    vc = 1.0 / np.tan(np.pi * freq_hz / fs)
    vc2 = vc * vc
    denom = 1.0 + np.sqrt(2.0) * vc + vc2
    b0 = (v_gain * vc2 + np.sqrt(2.0 * v_gain) * vc + 1.0) / denom
    b1 = 2.0 * (1.0 - v_gain * vc2) / denom
    b2 = (v_gain * vc2 - np.sqrt(2.0 * v_gain) * vc + 1.0) / denom
    a0 = 1.0
    a1 = 2.0 * (1.0 - vc2) / denom
    a2 = (1.0 - np.sqrt(2.0) * vc + vc2) / denom
    return np.array([[b0, b1, b2, a0, a1, a2]], dtype=np.float64)


def _highpass_sos(fs: int, freq_hz: float, quality: float) -> np.ndarray:
    """BS.1770-4 附录 B.3 二阶高通（stage 2）精确双线性变换系数。"""
    vb = 1.0 / np.tan(np.pi * freq_hz / fs)
    vb2 = vb * vb
    # 规范分子保持 [1, -2, 1]，分母含 Q（规范 Q=0.5 -> 2*vb 项）
    denom = 1.0 + (1.0 / quality) * vb + vb2
    b0 = 1.0
    b1 = -2.0
    b2 = 1.0
    a0 = 1.0
    a1 = 2.0 * (1.0 - vb2) / denom
    a2 = (1.0 - (1.0 / quality) * vb + vb2) / denom
    return np.array([[b0, b1, b2, a0, a1, a2]], dtype=np.float64)


def k_weighting_sos(fs: int) -> np.ndarray:
    """返回两级级联的 SOS 系数（shape = (2, 6)）。"""
    return np.concatenate([
        _high_shelf_sos(fs, SHELF_GAIN_DB, SHELF_FREQ_HZ),
        _highpass_sos(fs, HIGHPASS_FREQ_HZ, HIGHPASS_Q),
    ], axis=0)


def apply_k_weighting(x: np.ndarray, fs: int) -> np.ndarray:
    """对单声道 (N,) 或立体声 (N, 2) 信号施加 K 加权。

    每通道从零初始状态滤波；输出 shape/dtype 与输入一致（内部 float64）。
    """
    if x.ndim == 1:
        return signal.sosfilt(k_weighting_sos(fs), x.astype(np.float64))
    if x.ndim != 2 or x.shape[1] != 2:
        raise ValueError("apply_k_weighting 仅支持 (N,) 或 (N, 2) 输入")
    out = np.empty_like(x, dtype=np.float64)
    sos = k_weighting_sos(fs)
    for ch in range(2):
        out[:, ch] = signal.sosfilt(sos, x[:, ch].astype(np.float64))
    return out


def true_peak_db(x: np.ndarray, oversample: int = 4) -> float:
    """真峰值（dBTP）：按 oversample 倍上采样后取全通道最大绝对值。

    使用 scipy 的确定性多相 FIR 重采样，无随机分量。
    """
    if oversample not in (1, 2, 4, 8):
        raise ValueError("oversample 仅支持 1/2/4/8")
    signal_2d = x if x.ndim == 2 else x[:, np.newaxis]
    if oversample == 1:
        up = signal_2d.astype(np.float64)
    else:
        up = signal.resample_poly(signal_2d.astype(np.float64),
                                  oversample, 1, axis=0)
    peak = float(np.max(np.abs(up))) if up.size else 0.0
    if peak <= 0.0:
        return -np.inf
    return 20.0 * np.log10(peak)
