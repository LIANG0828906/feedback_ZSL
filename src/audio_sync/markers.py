from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .errors import E_BOUND, E_DECODE, SyncError
from .manifest import MarkerSpec


@dataclass(frozen=True)
class MarkerDetection:
    marker_id: str
    ref_sample: int
    offset_samples: float
    score: float

    @property
    def arrival_sample(self) -> float:
        return self.ref_sample + self.offset_samples


def marker_half_support(marker: MarkerSpec) -> float:
    if marker.kind == "pulse":
        sigma = float(marker.template.get("sigma_samples", 10.0))
        support = marker.template.get("support_samples", 4.2 * sigma)
        return float(support)
    if marker.kind == "chirp":
        length = float(marker.template.get("length_samples", 112.0))
        return 0.5 * length
    raise SyncError(E_DECODE, f"unknown marker kind '{marker.kind}'", marker=marker.id, field="kind")


def marker_waveform(marker: MarkerSpec, u_samples: np.ndarray | float) -> np.ndarray:
    """Evaluate the known continuous marker template at offsets from its centre."""

    u = np.asarray(u_samples, dtype=np.float64)
    out = np.zeros_like(u)
    if marker.kind == "pulse":
        carrier_hz = float(marker.template["carrier_hz"])
        sigma = float(marker.template["sigma_samples"])
        support = float(marker.template.get("support_samples", 4.2 * sigma))
        mask = np.abs(u) <= support
        envelope = np.exp(-0.5 * np.square(u[mask] / sigma))
        out[mask] = envelope * np.sin(2.0 * np.pi * carrier_hz * u[mask] / _fs_hint(marker))
        return out
    if marker.kind == "chirp":
        start_hz = float(marker.template["start_hz"])
        end_hz = float(marker.template["end_hz"])
        length = float(marker.template["length_samples"])
        fs = _fs_hint(marker)
        mask = np.abs(u) <= 0.5 * length
        local = u[mask]
        slope_hz_per_sample = (end_hz - start_hz) / length
        phase = 2.0 * np.pi * (start_hz * local + 0.5 * slope_hz_per_sample * local * local) / fs
        envelope = 0.5 * (1.0 + np.cos(2.0 * np.pi * local / length))
        out[mask] = envelope * np.sin(phase)
        return out
    raise SyncError(E_DECODE, f"unknown marker kind '{marker.kind}'", marker=marker.id, field="kind")


def _fs_hint(marker: MarkerSpec) -> float:
    value = marker.template.get("sample_rate")
    if value is None:
        raise SyncError(E_DECODE, "marker template is missing sample_rate", marker=marker.id, field="template.sample_rate")
    return float(value)


def _correlation_score(
    signal: np.ndarray,
    marker: MarkerSpec,
    ref_sample: int,
    candidate_offset: float,
    half_support: int,
    source_offset: int,
) -> float:
    centre = source_offset + ref_sample + candidate_offset
    base = int(np.floor(centre))
    n0 = base - half_support
    n1 = base + half_support + 1
    if n0 < 0 or n1 > signal.shape[0]:
        return -np.inf
    indices = np.arange(n0, n1, dtype=np.int64)
    template = marker_waveform(marker, indices.astype(np.float64) - centre)
    template_energy = float(np.dot(template, template))
    if template_energy <= 0.0:
        return -np.inf
    observed = np.asarray(signal[n0:n1], dtype=np.float64)
    return float(np.dot(observed, template) / np.sqrt(template_energy))


def detect_marker_offset(
    signal: np.ndarray,
    marker: MarkerSpec,
    *,
    max_offset_samples: int,
    source_offset: int = 0,
    coarse_step: float = 1.0,
    fine_step: float = 1.0 / 64.0,
    ultra_step: float = 1.0 / 1024.0,
) -> MarkerDetection:
    """Find a marker using only the observed channel and manifest template."""

    half_support = int(np.ceil(marker_half_support(marker))) + 2

    coarse_grid = np.arange(
        -float(max_offset_samples),
        float(max_offset_samples) + 0.5 * coarse_step,
        coarse_step,
        dtype=np.float64,
    )
    best_offset = 0.0
    best_score = -np.inf
    for candidate in coarse_grid:
        score = _correlation_score(
            signal,
            marker,
            marker.ref_sample,
            float(candidate),
            half_support,
            source_offset,
        )
        if score > best_score:
            best_score = score
            best_offset = float(candidate)

    fine_radius_steps = int(np.ceil(coarse_step / fine_step))
    fine_grid = best_offset + np.arange(
        -fine_radius_steps,
        fine_radius_steps + 1,
        dtype=np.float64,
    ) * fine_step
    for candidate in fine_grid:
        score = _correlation_score(
            signal,
            marker,
            marker.ref_sample,
            float(candidate),
            half_support,
            source_offset,
        )
        if score > best_score:
            best_score = score
            best_offset = float(candidate)

    ultra_grid = best_offset + np.arange(-16, 17, dtype=np.float64) * ultra_step
    for candidate in ultra_grid:
        score = _correlation_score(
            signal,
            marker,
            marker.ref_sample,
            float(candidate),
            half_support,
            source_offset,
        )
        if score > best_score:
            best_score = score
            best_offset = float(candidate)

    if not np.isfinite(best_score) or best_score <= 0.0:
        raise SyncError(
            E_DECODE,
            "marker could not be observed in channel data",
            marker=marker.id,
            field="samples",
        )
    if abs(best_offset) > max_offset_samples + 1e-9:
        raise SyncError(
            E_BOUND,
            "detected marker offset exceeds the permitted search range",
            marker=marker.id,
            sample=marker.ref_sample,
        )
    return MarkerDetection(marker.id, marker.ref_sample, best_offset, best_score)

