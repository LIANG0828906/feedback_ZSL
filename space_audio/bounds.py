from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

from .config import AnalysisConfig, RenderConfig


@dataclass(frozen=True)
class ErrorBounds:
    peak_quantization_s: float
    energy_truncation_relative: float
    tdoa_s: float
    convolution_interpolation_relative: float

    def scaled(self, frame_index: int) -> "ErrorBounds":
        scale = float(frame_index + 1)
        return ErrorBounds(
            peak_quantization_s=self.peak_quantization_s * scale,
            energy_truncation_relative=min(
                1.0,
                self.energy_truncation_relative * scale,
            ),
            tdoa_s=self.tdoa_s * scale,
            convolution_interpolation_relative=min(
                2.0,
                self.convolution_interpolation_relative * scale,
            ),
        )

    def to_dict(self) -> dict[str, float]:
        return {
            "peak_quantization_s": self.peak_quantization_s,
            "energy_truncation_relative": self.energy_truncation_relative,
            "tdoa_s": self.tdoa_s,
            "convolution_interpolation_relative": self.convolution_interpolation_relative,
        }


@dataclass(frozen=True)
class ObservedErrors:
    peak_quantization_s: float = 0.0
    energy_truncation_relative: float = 0.0
    tdoa_s: float = 0.0
    convolution_interpolation_relative: float = 0.0

    def plus(self, other: "ObservedErrors") -> "ObservedErrors":
        return ObservedErrors(
            peak_quantization_s=(
                self.peak_quantization_s + other.peak_quantization_s
            ),
            energy_truncation_relative=min(
                1.0,
                self.energy_truncation_relative
                + other.energy_truncation_relative,
            ),
            tdoa_s=self.tdoa_s + other.tdoa_s,
            convolution_interpolation_relative=min(
                2.0,
                self.convolution_interpolation_relative
                + other.convolution_interpolation_relative,
            ),
        )

    def maximum(self, other: "ObservedErrors") -> "ObservedErrors":
        return ObservedErrors(
            peak_quantization_s=max(
                self.peak_quantization_s,
                other.peak_quantization_s,
            ),
            energy_truncation_relative=max(
                self.energy_truncation_relative,
                other.energy_truncation_relative,
            ),
            tdoa_s=max(self.tdoa_s, other.tdoa_s),
            convolution_interpolation_relative=max(
                self.convolution_interpolation_relative,
                other.convolution_interpolation_relative,
            ),
        )

    def to_dict(self) -> dict[str, float]:
        return {
            "peak_quantization_s": self.peak_quantization_s,
            "energy_truncation_relative": self.energy_truncation_relative,
            "tdoa_s": self.tdoa_s,
            "convolution_interpolation_relative": self.convolution_interpolation_relative,
        }


@dataclass(frozen=True)
class BoundDerivation:
    sample_quantum_s: float
    peak_quantization: str
    energy_truncation: str
    tdoa: str
    convolution_interpolation: str

    def to_dict(self) -> dict[str, str]:
        return {
            "sample_quantum_s": f"{self.sample_quantum_s:.12g}",
            "peak_quantization": self.peak_quantization,
            "energy_truncation": self.energy_truncation,
            "tdoa": self.tdoa,
            "convolution_interpolation": self.convolution_interpolation,
        }


def compute_bounds(
    sample_rate: int,
    analysis: AnalysisConfig,
    render: RenderConfig,
) -> tuple[ErrorBounds, BoundDerivation]:
    if sample_rate <= 0:
        raise ValueError("sample_rate must be positive")
    sample_quantum = 1.0 / float(sample_rate)
    peak = 0.5 * sample_quantum
    order = max(1, int(analysis.interpolation_order))
    boundary_fraction = (
        2.0 * float(analysis.direct_half_width + 1)
        / float(analysis.window_len)
    )
    late_fraction = float(analysis.frame_len - analysis.window_len) / float(
        analysis.frame_len * order
    )
    energy = min(1.0, max(0.0, boundary_fraction + late_fraction))
    tdoa = 2.0 * peak
    conv = min(
        2.0,
        0.5 / float(order + 1)
        + float(render.block_size) / float(render.conv_window_len)
        + sample_quantum,
    )
    bounds = ErrorBounds(
        peak_quantization_s=peak,
        energy_truncation_relative=energy,
        tdoa_s=tdoa,
        convolution_interpolation_relative=conv,
    )
    derivation = BoundDerivation(
        sample_quantum_s=sample_quantum,
        peak_quantization=(
            "0.5 * (1/sample_rate): one half of an integer-sample "
            "quantization cell, independent of truth"
        ),
        energy_truncation=(
            "min(1, 2*(direct_half_width+1)/window_len + "
            "(frame_len-window_len)/(frame_len*interpolation_order)): "
            "boundary truncation plus integration-window fraction"
        ),
        tdoa=(
            "2 * peak_quantization bound: difference of two independently "
            "quantized channel arrival estimates"
        ),
        convolution_interpolation=(
            "min(2, 0.5/(interpolation_order+1) + block_size/conv_window_len "
            "+ 1/sample_rate): Lagrange residual, block/window partition "
            "fraction, and one sample quantum"
        ),
    )
    return bounds, derivation


def lagrange_weights(fraction: float, order: int) -> tuple[np.ndarray, int]:
    import numpy as np

    order = int(order)
    if order < 1:
        raise ValueError("order must be at least one")
    left = order // 2
    nodes = np.arange(-left, order - left + 1, dtype=np.float64)
    target = float(fraction)
    weights = np.ones(order + 1, dtype=np.float64)
    for index, node in enumerate(nodes):
        for other_index, other in enumerate(nodes):
            if index == other_index:
                continue
            weights[index] *= (target - other) / (node - other)
    return weights, -left


def interpolation_error(fraction: float, order: int) -> float:
    import numpy as np

    weights, _ = lagrange_weights(fraction, order)
    left = int(order) // 2
    nodes = np.arange(-left, order - left + 1, dtype=np.float64)
    omega = np.pi / 4.0
    approximated = np.sum(weights * np.exp(1j * omega * nodes))
    ideal = np.exp(1j * omega * float(fraction))
    return float(abs(approximated - ideal))


def check_bound(
    *,
    observed: ObservedErrors,
    bounds: ErrorBounds,
) -> tuple[str, float] | None:
    pairs = (
        ("peak_quantization_s", observed.peak_quantization_s, bounds.peak_quantization_s),
        (
            "energy_truncation_relative",
            observed.energy_truncation_relative,
            bounds.energy_truncation_relative,
        ),
        ("tdoa_s", observed.tdoa_s, bounds.tdoa_s),
        (
            "convolution_interpolation_relative",
            observed.convolution_interpolation_relative,
            bounds.convolution_interpolation_relative,
        ),
    )
    tolerance = 1.0e-12
    for name, value, bound in pairs:
        if not math.isfinite(value):
            return name, float("inf")
        if value > bound + tolerance:
            return name, value - bound
    return None
