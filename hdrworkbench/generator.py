"""确定性 HDR 样本帧序列生成器。

同种子、同参数生成的文件逐字节一致：像素模式由 ``numpy.random.default_rng``
驱动（PCG64，种子序列显式构造），叠加确定性梯度、亮部滚降条带与暗部纹理，
覆盖暗部细节与高光两类敏感区域。不使用时间戳或文件大小。
"""

from __future__ import annotations

import os
from dataclasses import dataclass

import numpy as np

from . import color
from .hdrformat import (
    Header,
    TF_HLG,
    TF_PQ,
    TF_SDR,
    write_sequence,
)


@dataclass(frozen=True)
class SampleSpec:
    width: int
    height: int
    frame_count: int
    bit_depth: int
    colorspace: int
    transfer: int
    peak_nits: int
    seed: int


def _scene_linear_frame(width: int, height: int, seed: int, frame_index: int):
    rng = np.random.default_rng(
        np.random.SeedSequence([seed, frame_index])
    )
    yy, xx = np.meshgrid(
        np.linspace(0.0, 1.0, height, dtype=np.float64),
        np.linspace(0.0, 1.0, width, dtype=np.float64),
        indexing="ij",
    )
    # 暗部纹理（0..0.05 线性光）
    dark = rng.random((height, width), dtype=np.float64) * 0.05
    # 基础梯度 + 帧间缓慢相位移动
    base = 0.15 + 0.55 * xx + 0.1 * np.sin(
        2.0 * np.pi * (yy + frame_index * 0.07)
    )
    # 高光滚降条带，最高超过 1.0 线性光，模拟 HDR 峰值
    highlight = np.where(
        (xx > 0.72) & (xx < 0.92),
        1.6 * (1.0 - np.abs(xx - 0.82) / 0.10),
        0.0,
    )
    luma = np.clip(base + dark + highlight, 0.0, 2.2)
    channels = np.stack(
        [
            luma,
            np.clip(luma * (0.85 + 0.1 * yy), 0.0, 2.2),
            np.clip(luma * (0.7 + 0.2 * (1.0 - xx)), 0.0, 2.2),
        ],
        axis=-1,
    )
    return channels


def _encode_scene(linear: np.ndarray, transfer: int, peak_nits: int):
    if transfer == TF_SDR:
        signal = color.srgb_oetf(np.clip(linear, 0.0, 1.0))
    elif transfer == TF_PQ:
        normalized = np.clip(linear * (1000.0 / peak_nits), 0.0, None)
        signal = color.pq_oetf(normalized)
    else:
        signal = color.hlg_oetf(np.clip(linear, 0.0, 1.0))
    return np.clip(signal, 0.0, 1.0)


def generate_frames(spec: SampleSpec) -> list[np.ndarray]:
    code_max = (1 << spec.bit_depth) - 1
    frames = []
    for frame_index in range(spec.frame_count):
        linear = _scene_linear_frame(
            spec.width, spec.height, spec.seed, frame_index
        )
        signal = _encode_scene(linear, spec.transfer, spec.peak_nits)
        codes = np.rint(signal * code_max)
        if spec.bit_depth == 8:
            frames.append(codes.astype(np.uint8))
        else:
            frames.append(codes.astype(np.uint16))
    return frames


def write_sample(path: str, spec: SampleSpec) -> Header:
    header = Header(
        width=spec.width,
        height=spec.height,
        frame_count=spec.frame_count,
        bit_depth=spec.bit_depth,
        colorspace=spec.colorspace,
        transfer=spec.transfer,
        peak_nits=spec.peak_nits,
    )
    frames = generate_frames(spec)
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    write_sequence(path, header, frames)
    return header


def default_specs() -> list[SampleSpec]:
    """覆盖三种传递函数、两种位深与多档峰值的标准样本集。"""
    return [
        SampleSpec(16, 12, 4, 10, 2, TF_PQ, 1000, seed=1001),
        SampleSpec(16, 12, 4, 10, 2, TF_HLG, 1000, seed=1002),
        SampleSpec(12, 10, 3, 8, 1, TF_SDR, 100, seed=1003),
        SampleSpec(20, 12, 3, 12, 3, TF_PQ, 4000, seed=1004),
    ]

