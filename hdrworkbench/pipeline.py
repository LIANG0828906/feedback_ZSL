"""完整色彩转换链与浮点 / 定点双路径。

链路固定为六步：

1. linearize   源传递函数线性化
2. primaries   基色矩阵变换（src RGB -> XYZ -> dst RGB）
3. adapt       白点适应（同白点为恒等）
4. tonemap     色调映射与峰值亮度滚降
5. encode      目标传递函数编码
6. quantize    量化回写为目标位深整数码值

每一步产出中间量内容哈希；双路径逐帧逐像素输出绝对偏差，
并给出每步最大码值偏差与超容差帧号。
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from . import color
from .errors import ModeInvalidError
from .fixedpoint import Quantizer, fixed_matmul, quantize_matrix
from .hashing import array_hash
from .hdrformat import FrameSequence, Header

STEP_NAMES = ("linearize", "primaries", "adapt", "tonemap", "encode", "quantize")
VALID_MODES = ("float", "fixed")


@dataclass(frozen=True)
class ConversionConfig:
    target_colorspace: int
    target_transfer: int
    target_bit_depth: int
    target_peak_nits: int
    tone_curve: str
    mode: str = "float"
    frac_bits: int = 16
    rounding: str = "nearest"
    seed: int = 0
    tolerance_codes: float = 4.0

    def validate(self) -> None:
        if self.mode not in VALID_MODES:
            raise ModeInvalidError(
                f"非法转换模式 {self.mode!r}",
                mode=self.mode,
                reason="unknown_mode",
            )
        if self.target_bit_depth not in (8, 10, 12, 16):
            raise ModeInvalidError(
                f"目标位深非法 {self.target_bit_depth}",
                mode=self.mode,
                reason="bad_target_bit_depth",
            )
        if self.tone_curve not in color.TONE_CURVES:
            raise ModeInvalidError(
                f"非法色调映射曲线 {self.tone_curve!r}",
                mode=self.mode,
                reason="bad_tone_curve",
            )
        if self.mode == "fixed":
            Quantizer(
                frac_bits=self.frac_bits,
                rounding=self.rounding,
                seed=self.seed,
            )

    def factor_dict(self) -> dict:
        """四类影响因子的规范表示，用于依赖判定与版本哈希。"""
        return {
            "target_colorspace": self.target_colorspace,
            "target_transfer": self.target_transfer,
            "target_bit_depth": self.target_bit_depth,
            "target_peak_nits": self.target_peak_nits,
            "tone_curve": self.tone_curve,
            "mode": self.mode,
            "frac_bits": self.frac_bits,
            "rounding": self.rounding,
            "seed": self.seed,
            "tolerance_codes": self.tolerance_codes,
        }


@dataclass
class FrameResult:
    frame_index: int
    codes: np.ndarray
    stage_hashes: dict
    stage_max_abs: dict
    pixel_diff: np.ndarray
    max_diff_codes: float
    over_tolerance: bool
    rounding_directions: dict = field(default_factory=dict)
    float_stages: dict = field(default_factory=dict)
    fixed_stages: dict = field(default_factory=dict)
    source_hash: str = ""
    bound_summary: dict = field(default_factory=dict)


def _quantize_codes(signal: np.ndarray, bit_depth: int) -> np.ndarray:
    code_max = (1 << bit_depth) - 1
    clipped = np.clip(signal, 0.0, 1.0)
    return np.rint(clipped * code_max).astype(np.uint16 if bit_depth > 8 else np.uint8)


def _run_float_chain(signal, header, config, matrix, adapt_matrix):
    stages = {}
    linear = color.linearize(signal, header.transfer)
    stages["linearize"] = linear
    primary = np.matmul(linear, matrix)
    stages["primaries"] = primary
    adapted = np.matmul(primary, adapt_matrix)
    stages["adapt"] = adapted
    mapped = color.tone_map(
        adapted,
        config.tone_curve,
        header.peak_nits,
        config.target_peak_nits,
    )
    stages["tonemap"] = mapped
    encoded = color.encode(mapped, config.target_transfer)
    stages["encode"] = encoded
    codes = _quantize_codes(encoded, config.target_bit_depth)
    stages["quantize"] = codes.astype(np.float64)
    return stages, codes


def _run_fixed_chain(signal, header, config, matrix, adapt_matrix, quantizer):
    stages = {}
    directions = {}

    linear_raw = color.linearize(signal, header.transfer)
    linear = quantizer.quantize(linear_raw, salt=100 + header.transfer)
    stages["linearize"] = linear
    directions["linearize"] = quantizer.rounding_direction(linear_raw, linear)

    fixed_matrix = quantize_matrix(matrix, quantizer)
    primary = fixed_matmul(linear, fixed_matrix, quantizer, salt=200)
    stages["primaries"] = primary
    directions["primaries"] = quantizer.rounding_direction(
        np.matmul(linear, fixed_matrix), primary
    )

    fixed_adapt = quantize_matrix(adapt_matrix, quantizer)
    adapted = fixed_matmul(primary, fixed_adapt, quantizer, salt=300)
    stages["adapt"] = adapted
    directions["adapt"] = quantizer.rounding_direction(
        np.matmul(primary, fixed_adapt), adapted
    )

    mapped_raw = color.tone_map(
        adapted,
        config.tone_curve,
        header.peak_nits,
        config.target_peak_nits,
    )
    mapped = quantizer.quantize(mapped_raw, salt=400)
    stages["tonemap"] = mapped
    directions["tonemap"] = quantizer.rounding_direction(mapped_raw, mapped)

    encoded_raw = color.encode(mapped, config.target_transfer)
    encoded = quantizer.quantize(encoded_raw, salt=500)
    stages["encode"] = encoded
    directions["encode"] = quantizer.rounding_direction(encoded_raw, encoded)

    codes = _quantize_codes(encoded, config.target_bit_depth)
    stages["quantize"] = codes.astype(np.float64)
    return stages, codes, directions, fixed_matrix, fixed_adapt


def _stage_code_deviation(
    float_stages: dict,
    fixed_stages: dict,
    target_bit_depth: int,
) -> dict:
    """每一步中间量换算到目标码值尺度后的最大绝对偏差。"""
    code_max = (1 << target_bit_depth) - 1
    result = {}
    for step in STEP_NAMES:
        diff = np.abs(float_stages[step] - fixed_stages[step])
        if step == "quantize":
            result[step] = float(np.max(diff))
        else:
            result[step] = float(np.max(diff) * code_max)
    return result


def convert_sequence(
    sequence: FrameSequence, config: ConversionConfig
) -> list[FrameResult]:
    """按配置跑完整链路；模式非法时抛 E_MODE_INVALID 且不产出任何帧。"""
    config.validate()
    results = []
    norm = sequence.normalized_frames()
    for index, signal in enumerate(norm):
        results.append(
            convert_frame(
                index,
                signal,
                array_hash(sequence.raw_frames[index]),
                sequence.header,
                config,
            )
        )
    return results


def convert_frame(
    frame_index: int,
    signal: np.ndarray,
    source_hash: str,
    header: Header,
    config: ConversionConfig,
) -> FrameResult:
    """单帧双路径转换，供增量重算逐帧调用。"""
    config.validate()
    matrix = color.color_transform_matrix(
        header.colorspace, config.target_colorspace
    )
    adapt_matrix = color.white_adaptation_matrix(
        color.CHROMATICITIES[header.colorspace][3],
        color.CHROMATICITIES[config.target_colorspace][3],
    )
    quantizer = None
    if config.mode == "fixed":
        quantizer = Quantizer(config.frac_bits, config.rounding, config.seed)

    float_stages, float_codes = _run_float_chain(
        signal, header, config, matrix, adapt_matrix
    )
    if config.mode == "fixed":
        fixed_stages, fixed_codes, directions, _, _ = _run_fixed_chain(
            signal, header, config, matrix, adapt_matrix, quantizer
        )
        pixel_diff = np.abs(
            float_codes.astype(np.int32) - fixed_codes.astype(np.int32)
        )
        codes = fixed_codes
        stage_hashes = {
            step: array_hash(fixed_stages[step]) for step in STEP_NAMES
        }
        stage_max_abs = _stage_code_deviation(
            float_stages, fixed_stages, config.target_bit_depth
        )
    else:
        fixed_stages, directions, pixel_diff = {}, {}, np.zeros(
            signal.shape, dtype=np.int32
        )
        codes = float_codes
        stage_hashes = {
            step: array_hash(float_stages[step]) for step in STEP_NAMES
        }
        stage_max_abs = {step: 0.0 for step in STEP_NAMES}

    max_diff = float(np.max(pixel_diff))
    return FrameResult(
        frame_index=frame_index,
        codes=codes,
        stage_hashes=stage_hashes,
        stage_max_abs=stage_max_abs,
        pixel_diff=pixel_diff,
        max_diff_codes=max_diff,
        over_tolerance=max_diff > config.tolerance_codes,
        rounding_directions=directions,
        float_stages=float_stages,
        fixed_stages=fixed_stages,
        source_hash=source_hash,
    )


def diff_summary(
    old_results: list[FrameResult], new_results: list[FrameResult]
) -> dict:
    """新旧版本编码值差异摘要。"""
    frame_count = min(len(old_results), len(new_results))
    per_frame = []
    changed = []
    for index in range(frame_count):
        diff = np.abs(
            old_results[index].codes.astype(np.int32)
            - new_results[index].codes.astype(np.int32)
        )
        max_abs = int(np.max(diff))
        mean_abs = float(np.mean(diff))
        per_frame.append(
            {"frame": index, "max_abs": max_abs, "mean_abs": mean_abs}
        )
        if max_abs > 0:
            changed.append(index)
    return {
        "frames_compared": frame_count,
        "changed_frames": changed,
        "overall_max_abs": max(
            (item["max_abs"] for item in per_frame), default=0
        ),
        "per_frame": per_frame,
    }

