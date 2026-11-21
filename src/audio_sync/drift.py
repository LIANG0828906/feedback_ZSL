"""First-order drift fitting and linear-versus-step diagnosis."""

from __future__ import annotations

import math

import numpy as np

from .errors import E_DRIFT_UNKNOWN
from .models import DriftModel, SegmentDelay, SyncConfig

_EPS = 1e-12


def _weighted_fit(design: np.ndarray, y: np.ndarray, weights: np.ndarray) -> tuple[np.ndarray, np.ndarray, float, np.ndarray]:
    root = np.sqrt(np.asarray(weights, dtype=np.float64))
    matrix = design * root[:, None]
    target = y * root
    coefficients, _, rank, _ = np.linalg.lstsq(matrix, target, rcond=None)
    fitted = design @ coefficients
    residual = y - fitted
    sse = float(np.sum(weights * residual * residual))
    if rank < design.shape[1]:
        covariance = np.full((design.shape[1], design.shape[1]), np.inf)
    else:
        normal_inverse = np.linalg.pinv(matrix.T @ matrix)
        covariance = normal_inverse
    return coefficients, covariance, sse, residual


def _bic(sse: float, count: int, parameters: int) -> float:
    return count * math.log(max(sse / max(count, 1), _EPS)) + parameters * math.log(max(count, 2))


def _interval(value: float, covariance: np.ndarray, index: int, s2: float) -> tuple[float, float]:
    variance = covariance[index, index] if np.isfinite(covariance[index, index]) else math.inf
    half = 1.96 * math.sqrt(max(0.0, variance * s2))
    if not math.isfinite(half):
        half = math.inf
    return value - half, value + half


def _reference_model(channel: int) -> DriftModel:
    return DriftModel(
        channel=channel,
        model_kind="reference",
        offset_samples=0.0,
        drift_ppm=0.0,
        confidence=1.0,
        diagnosis="reference channel; no drift compensation",
        evidence_segments=(),
        fit_residual_rms=0.0,
    )


def fit_drift_model(
    channel: int,
    segments: tuple[SegmentDelay, ...],
    sample_rate: int,
    config: SyncConfig,
) -> DriftModel:
    if config.enabled_channels is not None and channel == config.reference_channel:
        return _reference_model(channel)
    if channel == config.reference_channel:
        return _reference_model(channel)

    valid = [
        segment
        for segment in segments
        if segment.valid and segment.delay_samples is not None
    ]
    if not valid:
        return DriftModel(
            channel=channel,
            model_kind="uncertain",
            offset_samples=0.0,
            drift_ppm=0.0,
            confidence=0.0,
            error_code=E_DRIFT_UNKNOWN,
            candidates=("signal too weak for cross-correlation", "markers missing", "channel disconnected"),
            diagnosis="no valid segment delay; drift cannot be fitted",
        )

    centers = np.asarray([item.center_sample for item in valid], dtype=np.float64)
    delays = np.asarray([item.delay_samples for item in valid], dtype=np.float64)
    weights = np.clip(
        np.asarray([item.peak_correlation for item in valid], dtype=np.float64),
        0.05,
        1.0,
    )

    if valid[0].method == "whole_recording" or delays.size == 1:
        segment = valid[0]
        ci_low = segment.ci_low_samples if segment.ci_low_samples is not None else math.inf
        ci_high = segment.ci_high_samples if segment.ci_high_samples is not None else math.inf
        return DriftModel(
            channel=channel,
            model_kind="constant_offset",
            offset_samples=float(np.average(delays, weights=weights)),
            drift_ppm=0.0,
            offset_ci=(float(ci_low), float(ci_high)),
            drift_ppm_ci=(0.0, 0.0),
            confidence=float(np.clip(np.max(weights), 0.0, 1.0)),
            evidence_segments=(segment.segment_index,),
            fit_residual_rms=0.0,
            diagnosis="whole-recording correlation; first-order drift is not observable from one lag",
        )

    design = np.column_stack((np.ones_like(centers), centers))
    coefficients, covariance, sse_linear, residual_linear = _weighted_fit(design, delays, weights)
    offset = float(coefficients[0])
    slope = float(coefficients[1])
    drift_ppm = slope * 1e6
    count = centers.size
    degrees = max(1, count - design.shape[1])
    residual_rms = float(np.sqrt(np.mean(residual_linear**2)))
    centered = delays - np.average(delays, weights=weights)
    total_variance = float(np.sum(weights * centered * centered))
    r_squared = 1.0 - sse_linear / max(total_variance, _EPS)

    median_ci_half = float(
        np.median(
            [
                max(
                    (item.ci_high_samples or item.delay_samples) - item.delay_samples,
                    item.delay_samples - (item.ci_low_samples or item.delay_samples),
                    0.0,
                )
                for item in valid
            ]
        )
    )
    sigma_floor = max(median_ci_half / 1.96, 0.15)
    s2_linear = max(sse_linear / degrees, sigma_floor * sigma_floor)

    step_sample: float | None = None
    step_samples = 0.0
    step_ci: tuple[float, float] | None = None
    step_gain = -math.inf
    step_covariance: np.ndarray | None = None
    step_sse = sse_linear
    step_parameters: np.ndarray | None = None
    step_index: int | None = None

    if count >= 6:
        for candidate in range(2, count - 1):
            indicator = (centers >= centers[candidate]).astype(np.float64)
            step_design = np.column_stack((design, indicator))
            params, cov, sse, _ = _weighted_fit(step_design, delays, weights)
            gain = _bic(sse_linear, count, 2) - _bic(sse, count, 3)
            if gain > step_gain:
                step_gain = gain
                step_sse = sse
                step_parameters = params
                step_covariance = cov
                step_index = candidate

    if (
        step_parameters is not None
        and step_index is not None
        and step_gain >= config.step_bic_gain
        and abs(float(step_parameters[2])) >= config.step_min_samples
    ):
        offset = float(step_parameters[0])
        slope = float(step_parameters[1])
        drift_ppm = slope * 1e6
        step_samples = float(step_parameters[2])
        step_sample = 0.5 * (centers[step_index - 1] + centers[step_index])
        degrees = max(1, count - 3)
        residual_rms = float(
            np.sqrt(
                np.mean(
                    (
                        delays
                        - (
                            offset
                            + slope * centers
                            + step_samples * (centers >= step_sample)
                        )
                    )
                    ** 2
                )
            )
        )
        s2_step = max(step_sse / degrees, sigma_floor * sigma_floor)
        offset_ci = _interval(offset, step_covariance, 0, s2_step)
        slope_ci = _interval(slope, step_covariance, 1, s2_step)
        drift_ci = (slope_ci[0] * 1e6, slope_ci[1] * 1e6)
        step_ci = _interval(step_samples, step_covariance, 2, s2_step)
        step_quality = abs(step_samples) / max(abs(step_samples) + 3.0 * sigma_floor, _EPS)
        confidence = float(
            np.clip(0.45 + 0.35 * step_quality + 0.02 * min(step_gain, 15.0), 0.0, 0.99)
        )
        return DriftModel(
            channel=channel,
            model_kind="step_jump",
            offset_samples=offset,
            drift_ppm=drift_ppm,
            step_sample=float(step_sample),
            step_samples=step_samples,
            offset_ci=offset_ci,
            drift_ppm_ci=drift_ci,
            step_ci=step_ci,
            confidence=confidence,
            evidence_segments=(max(0, step_index - 1), step_index),
            fit_residual_rms=residual_rms,
            diagnosis=(
                f"step jump of {step_samples:.3f} samples near segment {step_index}, "
                "with a first-order background rate"
            ),
            candidates=("clock discontinuity", "buffer over/underrun", "sound-card restart"),
        )

    offset_ci = _interval(offset, covariance, 0, s2_linear)
    slope_ci = _interval(slope, covariance, 1, s2_linear)
    drift_ci = (slope_ci[0] * 1e6, slope_ci[1] * 1e6)
    confidence = float(
        np.clip(
            0.35
            + 0.6 * max(0.0, min(1.0, r_squared))
            - 0.08 * residual_rms / max(config.tolerance_samples, 0.1),
            0.0,
            0.99,
        )
    )
    evidence = tuple(
        int(valid[index].segment_index)
        for index in np.argsort(np.abs(residual_linear))[::-1][: min(3, count)]
    )

    if count == 2:
        return DriftModel(
            channel=channel,
            model_kind="uncertain",
            offset_samples=offset,
            drift_ppm=drift_ppm,
            offset_ci=offset_ci,
            drift_ppm_ci=drift_ci,
            confidence=confidence,
            evidence_segments=evidence,
            fit_residual_rms=residual_rms,
            error_code=E_DRIFT_UNKNOWN,
            candidates=("linear clock drift", "single step jump", "correlation ambiguity"),
            diagnosis="only two valid segments are available; linear and step explanations are indistinguishable",
        )

    if abs(drift_ppm) < config.linear_min_abs_rate_ppm:
        target_kind = "constant_offset"
        diagnosis = "no significant frequency offset; constant delay model selected"
    else:
        target_kind = "linear_drift"
        diagnosis = f"continuous first-order drift at {drift_ppm:.3f} ppm"

    residual_limit = max(3.0 * config.tolerance_samples, abs(drift_ppm) * 1e-6 * np.ptp(centers) * 0.08 + 0.5)
    if residual_rms > residual_limit and r_squared < 0.35:
        return DriftModel(
            channel=channel,
            model_kind="uncertain",
            offset_samples=offset,
            drift_ppm=drift_ppm,
            offset_ci=offset_ci,
            drift_ppm_ci=drift_ci,
            confidence=confidence,
            evidence_segments=evidence,
            fit_residual_rms=residual_rms,
            error_code=E_DRIFT_UNKNOWN,
            candidates=(
                "multiple step discontinuities",
                "clock reset or audio dropout",
                "periodic-marker ambiguity",
                "nonlinear drift",
            ),
            diagnosis=(
                f"segment delays are discontinuous or noisy (residual RMS {residual_rms:.3f} samples)"
            ),
        )

    return DriftModel(
        channel=channel,
        model_kind=target_kind,
        offset_samples=offset,
        drift_ppm=drift_ppm,
        offset_ci=offset_ci,
        drift_ppm_ci=drift_ci,
        confidence=confidence,
        evidence_segments=evidence,
        fit_residual_rms=residual_rms,
        diagnosis=diagnosis,
    )
