from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable

import numpy as np


@dataclass(frozen=True)
class TransferSegment:
    index: int
    start: float
    end: float
    absorption_start: float
    absorption_end: float
    emission_start: np.ndarray
    emission_end: np.ndarray

    @property
    def width(self) -> float:
        return self.end - self.start

    def evaluate(self, density: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
        values = np.asarray(density, dtype=np.float64)
        parameter = (values - self.start) / self.width
        absorption = self.absorption_start + (
            self.absorption_end - self.absorption_start
        ) * parameter
        emission = self.emission_start[None, :] + (
            self.emission_end - self.emission_start
        )[None, :] * parameter[:, None]
        return absorption, emission


@dataclass(frozen=True)
class TransferFunction:
    density_min: float
    density_max: float
    transition_width: float
    segments: tuple[TransferSegment, ...]

    @property
    def boundaries(self) -> tuple[float, ...]:
        return tuple(segment.end for segment in self.segments[:-1])

    def _base_evaluate(
        self, values: np.ndarray
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
        absorption = np.empty(values.shape, dtype=np.float64)
        emission = np.empty((values.size, 3), dtype=np.float64)
        segment_index = np.empty(values.shape, dtype=np.int32)
        transition = np.zeros(values.shape, dtype=bool)
        flat = values.ravel()

        for segment in self.segments:
            if segment.index == len(self.segments) - 1:
                mask = (flat >= segment.start) & (flat <= segment.end)
            else:
                mask = (flat >= segment.start) & (flat < segment.end)
            if not np.any(mask):
                continue
            local_absorption, local_emission = segment.evaluate(flat[mask])
            absorption.ravel()[mask] = local_absorption
            emission[mask] = local_emission
            segment_index.ravel()[mask] = segment.index

        if self.transition_width > 0.0:
            half_width = 0.5 * self.transition_width
            for left, right in zip(self.segments[:-1], self.segments[1:]):
                boundary = left.end
                band = (flat >= boundary - half_width) & (
                    flat <= boundary + half_width
                )
                if not np.any(band):
                    continue
                band_values = flat[band]
                left_absorption, left_emission = left.evaluate(band_values)
                right_absorption, right_emission = right.evaluate(band_values)
                blend = np.clip(
                    (band_values - (boundary - half_width))
                    / self.transition_width,
                    0.0,
                    1.0,
                )
                blend = blend * blend * (3.0 - 2.0 * blend)
                absorption.ravel()[band] = (
                    left_absorption * (1.0 - blend) + right_absorption * blend
                )
                emission[band] = (
                    left_emission * (1.0 - blend[:, None])
                    + right_emission * blend[:, None]
                )
                transition.ravel()[band] = True
                segment_index.ravel()[band] = np.where(
                    band_values < boundary, left.index, right.index
                )

        return absorption, emission, segment_index, transition

    def evaluate(
        self, density: np.ndarray | float
    ) -> tuple[np.ndarray, np.ndarray]:
        original = np.asarray(density, dtype=np.float64)
        clamped = np.clip(original, self.density_min, self.density_max)
        absorption, emission, _, _ = self._base_evaluate(clamped)
        if original.ndim == 0:
            return absorption.reshape(()), emission.reshape(3)
        return absorption, emission

    def evaluate_scalar(self, density: float) -> tuple[float, np.ndarray]:
        absorption, emission = self.evaluate(np.asarray([density]))
        return float(absorption[0]), emission[0].copy()

    def boundary_checks(self, tolerance: float = 1e-9) -> list[dict]:
        checks: list[dict] = []
        for left, right in zip(self.segments[:-1], self.segments[1:]):
            boundary = left.end
            absorption_delta = abs(left.absorption_end - right.absorption_start)
            emission_delta = float(
                np.max(np.abs(left.emission_end - right.emission_start))
            )
            if self.transition_width > 0.0:
                mode = "smooth_transition"
                passed = True
            else:
                mode = "shared_endpoint"
                passed = (
                    absorption_delta <= tolerance
                    and emission_delta <= tolerance
                )
            checks.append(
                {
                    "boundary": boundary,
                    "left_segment": left.index,
                    "right_segment": right.index,
                    "left_absorption": left.absorption_end,
                    "right_absorption": right.absorption_start,
                    "absorption_delta": absorption_delta,
                    "left_emission": left.emission_end.tolist(),
                    "right_emission": right.emission_start.tolist(),
                    "emission_delta": emission_delta,
                    "mode": mode,
                    "transition_width": self.transition_width,
                    "passed": passed,
                }
            )
        return checks

    def scan(self, input_densities: Iterable[float]) -> dict:
        inputs = np.asarray(list(input_densities), dtype=np.float64)
        clamped = np.clip(inputs, self.density_min, self.density_max)
        absorption, emission, segment_index, transition = self._base_evaluate(
            clamped
        )
        samples = []
        for index, input_density in enumerate(inputs):
            samples.append(
                {
                    "input_density": float(input_density),
                    "clamped_density": float(clamped[index]),
                    "absorption": float(absorption[index]),
                    "emission": emission[index].tolist(),
                    "segment": int(segment_index[index]),
                    "transition_blend": bool(transition[index]),
                }
            )
        return {
            "density_range": [self.density_min, self.density_max],
            "transition_width": self.transition_width,
            "segments": [
                {
                    "index": segment.index,
                    "range": [segment.start, segment.end],
                    "absorption": [
                        segment.absorption_start,
                        segment.absorption_end,
                    ],
                    "emission": [
                        segment.emission_start.tolist(),
                        segment.emission_end.tolist(),
                    ],
                }
                for segment in self.segments
            ],
            "boundary_checks": self.boundary_checks(),
            "samples": samples,
        }

    def default_scan_densities(self, points_per_interval: int = 5) -> list[float]:
        values: set[float] = {
            self.density_min - 0.1,
            self.density_min,
            self.density_max,
            self.density_max + 0.1,
        }
        half_width = 0.5 * self.transition_width
        for segment in self.segments:
            for point_index in range(points_per_interval):
                fraction = point_index / max(points_per_interval - 1, 1)
                values.add(segment.start + fraction * segment.width)
        for boundary in self.boundaries:
            if half_width > 0.0:
                values.add(boundary - half_width)
            values.add(boundary)
            if half_width > 0.0:
                values.add(boundary + half_width)
        values.add(self.density_min - 1e-6)
        values.add(self.density_max + 1e-6)
        return sorted(float(value) for value in values)

