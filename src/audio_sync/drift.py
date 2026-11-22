from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any

import numpy as np

from .config import AlignmentConfig
from .errors import E_BOUND, E_PARA, SyncError
from .manifest import MarkerSpec
from .markers import MarkerDetection


def segment_boundaries(session_samples: int, segment_length: int) -> np.ndarray:
    if session_samples <= 0 or segment_length <= 0:
        raise SyncError(E_PARA, "session_samples and segment_length must be positive", field="segment_length")
    count = int(math.ceil(session_samples / segment_length))
    boundaries = np.array(
        [min(index * segment_length, session_samples) for index in range(count + 1)],
        dtype=np.int64,
    )
    return boundaries


def drift_design_matrix(positions: np.ndarray, knots: np.ndarray) -> np.ndarray:
    t = np.asarray(positions, dtype=np.float64)
    piece_count = len(knots) - 1
    matrix = np.ones((t.shape[0], piece_count + 1), dtype=np.float64)
    for index in range(piece_count):
        left = float(knots[index])
        right = float(knots[index + 1])
        matrix[:, index + 1] = np.clip(t, left, right) - left
    return matrix


def delay_from_coefficients(positions: np.ndarray | float, knots: np.ndarray, coefficients: np.ndarray) -> np.ndarray:
    t = np.asarray(positions, dtype=np.float64)
    design = drift_design_matrix(t, knots)
    return design @ np.asarray(coefficients, dtype=np.float64)


@dataclass
class DriftModel:
    channel_id: str
    reference_channel: str
    knots_ref_samples: np.ndarray
    coefficients: np.ndarray
    covariance: np.ndarray
    marker_times: np.ndarray
    marker_offsets: np.ndarray
    marker_scores: np.ndarray
    residual_std_samples: float
    marker_uncertainty_samples: float
    is_reference: bool = False
    diagnostics: dict[str, Any] = field(default_factory=dict)

    @property
    def initial_offset_samples(self) -> float:
        return float(self.coefficients[0])

    @property
    def rates_samples_per_sample(self) -> np.ndarray:
        return np.asarray(self.coefficients[1:], dtype=np.float64)

    @property
    def rates_ppm(self) -> np.ndarray:
        return self.rates_samples_per_sample * 1.0e6

    @property
    def initial_ci_samples(self) -> float:
        tcrit = float(self.diagnostics.get("t_critical", 2.2))
        return tcrit * float(np.sqrt(max(self.covariance[0, 0], 0.0)))

    @property
    def rates_ci_ppm(self) -> np.ndarray:
        tcrit = float(self.diagnostics.get("t_critical", 2.2))
        variances = np.maximum(np.diag(self.covariance)[1:], 0.0)
        return tcrit * np.sqrt(variances) * 1.0e6

    def delay_at(self, positions: np.ndarray | float) -> np.ndarray:
        return delay_from_coefficients(positions, self.knots_ref_samples, self.coefficients)

    def rate_ppm_at(self, positions: np.ndarray | float) -> np.ndarray:
        t = np.atleast_1d(np.asarray(positions, dtype=np.float64))
        indices = np.searchsorted(self.knots_ref_samples[1:], t, side="right")
        indices = np.clip(indices, 0, len(self.rates_ppm) - 1)
        result = self.rates_ppm[indices]
        return result if np.ndim(positions) else np.asarray(result[0])

    def prediction_ci_at(self, positions: np.ndarray) -> np.ndarray:
        t = np.asarray(positions, dtype=np.float64)
        design = drift_design_matrix(t, self.knots_ref_samples)
        variance = np.einsum("ij,jk,ik->i", design, self.covariance, design)
        tcrit = float(self.diagnostics.get("t_critical", 2.2))
        return tcrit * np.sqrt(np.maximum(variance, 0.0))

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel_id": self.channel_id,
            "reference_channel": self.reference_channel,
            "model": "initial_phase_plus_piecewise_linear_rates",
            "knots_ref_samples": self.knots_ref_samples.tolist(),
            "initial_offset_samples": self.initial_offset_samples,
            "initial_ci95_samples": self.initial_ci_samples,
            "rates_ppm": self.rates_ppm.tolist(),
            "rates_ci95_ppm": self.rates_ci_ppm.tolist(),
            "marker_times_ref_samples": self.marker_times.tolist(),
            "marker_offsets_samples": self.marker_offsets.tolist(),
            "marker_scores": self.marker_scores.tolist(),
            "residual_std_samples": self.residual_std_samples,
            "is_reference": self.is_reference,
            "diagnostics": self.diagnostics,
        }


def _reference_model(channel_id: str, reference_channel: str, knots: np.ndarray) -> DriftModel:
    coefficients = np.zeros(len(knots), dtype=np.float64)
    covariance = np.zeros((len(knots), len(knots)), dtype=np.float64)
    return DriftModel(
        channel_id=channel_id,
        reference_channel=reference_channel,
        knots_ref_samples=knots,
        coefficients=coefficients,
        covariance=covariance,
        marker_times=np.empty(0, dtype=np.float64),
        marker_offsets=np.empty(0, dtype=np.float64),
        marker_scores=np.empty(0, dtype=np.float64),
        residual_std_samples=0.0,
        marker_uncertainty_samples=0.0,
        is_reference=True,
        diagnostics={"t_critical": 2.2, "rank": len(knots), "observed_only": True},
    )


def estimate_drift_model(
    channel_id: str,
    reference_channel: str,
    markers: list[MarkerSpec] | tuple[MarkerSpec, ...],
    detections: list[MarkerDetection] | tuple[MarkerDetection, ...],
    knots: np.ndarray,
    config: AlignmentConfig,
) -> DriftModel:
    config.validate()
    if len(markers) != len(detections):
        raise SyncError(E_PARA, "marker/detection count mismatch", channel=channel_id, field="markers")
    if len(markers) < 3:
        raise SyncError(E_BOUND, "at least three observable markers are required", channel=channel_id, field="markers")
    if channel_id == reference_channel:
        return _reference_model(channel_id, reference_channel, knots)

    times = np.array([marker.ref_sample for marker in markers], dtype=np.float64)
    offsets = np.array([detection.offset_samples for detection in detections], dtype=np.float64)
    scores = np.array([detection.score for detection in detections], dtype=np.float64)
    if not np.all(np.isfinite(offsets)):
        raise SyncError(E_BOUND, "non-finite marker offsets", channel=channel_id, field="markers")
    if np.max(np.abs(offsets)) > config.max_offset_samples:
        raise SyncError(
            E_PARA,
            "estimated offset exceeds max_offset_samples",
            channel=channel_id,
            field="max_offset_samples",
        )

    requested_pieces = len(knots) - 1
    max_pieces = max(1, min(requested_pieces, len(markers) - 2))
    if max_pieces == requested_pieces:
        fit_knots = np.asarray(knots, dtype=np.float64)
    else:
        fit_knots = np.linspace(float(knots[0]), float(knots[-1]), max_pieces + 1)
        fit_knots[-1] = float(knots[-1])

    design = drift_design_matrix(times, fit_knots)
    if design.shape[0] <= design.shape[1]:
        raise SyncError(
            E_BOUND,
            "not enough markers to identify the requested drift model",
            channel=channel_id,
            details={"markers": len(markers), "parameters": design.shape[1]},
        )
    coefficients, _, rank, _ = np.linalg.lstsq(design, offsets, rcond=None)
    predicted = design @ coefficients
    residuals = offsets - predicted
    degrees = max(1, len(markers) - design.shape[1])
    residual_std = float(np.sqrt(np.sum(np.square(residuals)) / degrees))
    scale_sigma = max(float(config.marker_uncertainty_samples), residual_std)
    gram_inv = np.linalg.pinv(design.T @ design)
    covariance = scale_sigma * scale_sigma * gram_inv
    tcrit = 2.2 if degrees < 10 else 2.0
    if not np.all(np.isfinite(covariance)):
        raise SyncError(E_BOUND, "drift covariance is not finite", channel=channel_id)

    rates_ppm = coefficients[1:] * 1.0e6
    if np.max(np.abs(rates_ppm)) > config.max_drift_ppm:
        raise SyncError(
            E_PARA,
            "estimated drift rate exceeds max_drift_ppm",
            channel=channel_id,
            field="max_drift_ppm",
            details={"max_abs_rate_ppm": float(np.max(np.abs(rates_ppm)))},
        )
    if coefficients[0] < -config.max_offset_samples or coefficients[0] > config.max_offset_samples:
        raise SyncError(E_PARA, "initial offset exceeds max_offset_samples", channel=channel_id, field="max_offset_samples")

    diagnostics = {
        "t_critical": tcrit,
        "degrees_of_freedom": degrees,
        "rank": int(rank),
        "requested_pieces": requested_pieces,
        "fitted_pieces": max_pieces,
        "observed_only": True,
        "fit_rmse_samples": residual_std,
        "min_marker_score": float(np.min(scores)),
    }
    return DriftModel(
        channel_id=channel_id,
        reference_channel=reference_channel,
        knots_ref_samples=fit_knots,
        coefficients=coefficients,
        covariance=covariance,
        marker_times=times,
        marker_offsets=offsets,
        marker_scores=scores,
        residual_std_samples=residual_std,
        marker_uncertainty_samples=float(config.marker_uncertainty_samples),
        diagnostics=diagnostics,
    )


def interpolation_error_bound(config: AlignmentConfig, max_marker_hz: float, sample_rate: int) -> float:
    """Conservative bandlimited interpolation bound from order and observed marker bandwidth."""

    order = int(config.interpolation_order)
    ratio = min(0.95, 2.0 * math.pi * float(max_marker_hz) / float(sample_rate))
    return float(ratio ** (order + 1) / math.factorial(order + 1))


def residual_bounds(
    model: DriftModel,
    marker_times: np.ndarray,
    config: AlignmentConfig,
    interpolation_bound: float,
) -> np.ndarray:
    """Compute bounds using only observations, confidence, spacing and interpolation order."""

    times = np.asarray(marker_times, dtype=np.float64)
    if times.shape[0] == 0:
        return np.empty(0, dtype=np.float64)
    prediction_ci = model.prediction_ci_at(times)
    if times.shape[0] > 1:
        spacings = np.diff(times)
        nearest = np.empty_like(times)
        nearest[0] = spacings[0]
        nearest[-1] = spacings[-1]
        if times.shape[0] > 2:
            nearest[1:-1] = np.minimum(spacings[:-1], spacings[1:])
    else:
        nearest = np.zeros_like(times)
    max_rate_ci = float(np.max(model.rates_ci_ppm)) * 1.0e-6 if model.rates_ci_ppm.size else 0.0
    spacing_term = 0.5 * max_rate_ci * nearest
    score_term = 0.25 * model.marker_uncertainty_samples / np.maximum(model.marker_scores, 1.0e-3)
    detection_floor = 0.5 * model.marker_uncertainty_samples
    return (
        prediction_ci
        + spacing_term
        + interpolation_bound
        + score_term
        + detection_floor
        + float(config.residual_bound_margin)
    )

