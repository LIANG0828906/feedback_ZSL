"""First-order clock drift fitting and parameter-change comparison."""

from __future__ import annotations

from typing import Any, Iterable

import numpy as np

from .models import DriftFit, FitConfig, SegmentDelay


def _weighted_line(
    x: np.ndarray,
    y: np.ndarray,
    weights: np.ndarray,
) -> tuple[float, float, np.ndarray, np.ndarray]:
    design = np.column_stack((np.ones_like(x), x))
    weighted_design = design * np.sqrt(weights)[:, None]
    weighted_target = y * np.sqrt(weights)
    coefficients, _, _, _ = np.linalg.lstsq(
        weighted_design, weighted_target, rcond=None
    )
    predicted = design @ coefficients
    residuals = y - predicted
    return (
        float(coefficients[1]),
        float(coefficients[0]),
        residuals,
        predicted,
    )


def _ransac_line_initial(
    x: np.ndarray,
    y: np.ndarray,
    widths: np.ndarray,
) -> tuple[float, float, np.ndarray] | None:
    count = x.size
    if count < 3:
        return None
    if count <= 64:
        fit_indices = np.arange(count)
    else:
        fit_indices = np.unique(
            np.linspace(0, count - 1, 64).round().astype(np.int64)
        )
    inlier_threshold = max(
        2.0,
        2.5 * float(np.median(widths[fit_indices])),
    )
    best: tuple[tuple[float, float, float], float, float, np.ndarray] | None = None
    for left_position, left_index in enumerate(fit_indices[:-1]):
        for right_index in fit_indices[left_position + 1 :]:
            denominator = x[right_index] - x[left_index]
            if abs(float(denominator)) < 1.0e-12:
                continue
            slope = float(
                (y[right_index] - y[left_index]) / denominator
            )
            intercept = float(y[left_index] - slope * x[left_index])
            residual = np.abs(y - (intercept + slope * x))
            inliers = residual <= inlier_threshold
            inlier_count = int(np.sum(inliers))
            if inlier_count < 2:
                continue
            median_residual = float(np.median(residual[inliers]))
            rms = float(
                np.sqrt(np.mean(residual[inliers] ** 2))
            )
            score = (
                float(inlier_count),
                -median_residual,
                -rms,
                -abs(slope),
            )
            if best is None or score > best[0]:
                best = (score, slope, intercept, inliers)
    if best is None:
        return None
    _, slope, intercept, inliers = best
    return slope, intercept, inliers


def fit_channel_drift(
    segments: Iterable[SegmentDelay],
    *,
    channel: int,
    sample_rate: int,
    config: FitConfig,
    reference_channel: int = 0,
) -> DriftFit:
    ordered = sorted(segments, key=lambda item: item.segment_index)
    if not ordered:
        return DriftFit(
            channel=channel,
            initial_offset_samples=0.0,
            drift_ppm=0.0,
            slope=0.0,
            residual_rms=0.0,
            residual_max=0.0,
            residual_threshold_samples=config.residual_threshold_samples,
            max_residual_segment=None,
            residual_by_segment=(),
            predicted_by_segment=(),
            exceeded_threshold=False,
            slope_std=0.0,
            drift_ppm_ci_low=0.0,
            drift_ppm_ci_high=0.0,
        )
    midpoints = np.array(
        [
            0.5 * (segment.start_sample + segment.end_sample - 1)
            for segment in ordered
        ],
        dtype=np.float64,
    )
    delays = np.array([segment.delay_samples for segment in ordered], dtype=np.float64)
    if channel == reference_channel:
        slope = 0.0
        intercept = 0.0
        residuals = np.zeros_like(delays)
        predicted = np.zeros_like(delays)
        slope_std = 0.0
    elif midpoints.size < 2 or float(np.ptp(midpoints)) == 0.0:
        slope = 0.0
        intercept = float(np.mean(delays))
        predicted = np.full_like(delays, intercept)
        residuals = delays - predicted
        slope_std = 0.0
    else:
        widths = np.array(
            [
                max(
                    0.5,
                    0.5
                    * (
                        (segment.ci_high_samples or segment.delay_samples + 1.0)
                        - (segment.ci_low_samples or segment.delay_samples - 1.0)
                    ),
                )
                for segment in ordered
            ],
            dtype=np.float64,
        )
        weights = 1.0 / np.maximum(widths**2, 0.25)
        initial = _ransac_line_initial(midpoints, delays, widths)
        initial_weights = weights.copy()
        if initial is not None:
            _, _, inliers = initial
            initial_weights = initial_weights * np.where(inliers, 1.0, 0.05)
        slope, intercept, residuals, predicted = _weighted_line(
            midpoints, delays, initial_weights
        )
        for _ in range(max(0, config.robust_iterations)):
            center = float(np.median(residuals))
            scale = 1.4826 * float(np.median(np.abs(residuals - center)))
            scale = max(scale, 0.25)
            normalized = np.abs(residuals - center) / (1.345 * scale)
            robust_weights = np.where(
                normalized <= 1.0, 1.0, 1.0 / np.maximum(normalized, 1.0e-12)
            )
            slope, intercept, residuals, predicted = _weighted_line(
                midpoints, delays, initial_weights * robust_weights
            )
        design = np.column_stack((np.ones_like(midpoints), midpoints))
        weighted_design = design * np.sqrt(weights)[:, None]
        try:
            covariance = np.linalg.inv(
                weighted_design.T @ weighted_design
            ) * float(np.mean(residuals**2))
            slope_std = float(np.sqrt(max(covariance[1, 1], 0.0)))
        except np.linalg.LinAlgError:
            slope_std = 0.0
    residual_rms = float(np.sqrt(np.mean(residuals**2))) if residuals.size else 0.0
    residual_max = float(np.max(np.abs(residuals))) if residuals.size else 0.0
    max_index = int(np.argmax(np.abs(residuals))) if residuals.size else 0
    max_segment = ordered[max_index].segment_index if ordered else None
    predicted_by_segment = tuple(
        {
            "segment_index": segment.segment_index,
            "midpoint_sample": float(midpoint),
            "predicted_delay_samples": float(prediction),
            "observed_delay_samples": float(delay),
            "residual_samples": float(as_float),
        }
        for segment, midpoint, prediction, delay, as_float in zip(
            ordered, midpoints, predicted, delays, residuals
        )
    )
    drift_ppm = slope * 1.0e6
    ci = 1.96 * slope_std * 1.0e6
    return DriftFit(
        channel=channel,
        initial_offset_samples=float(intercept),
        drift_ppm=float(drift_ppm),
        slope=float(slope),
        residual_rms=residual_rms,
        residual_max=residual_max,
        residual_threshold_samples=config.residual_threshold_samples,
        max_residual_segment=max_segment,
        residual_by_segment=tuple(float(value) for value in residuals),
        predicted_by_segment=predicted_by_segment,
        exceeded_threshold=residual_max > config.residual_threshold_samples,
        slope_std=slope_std,
        drift_ppm_ci_low=float(drift_ppm - ci),
        drift_ppm_ci_high=float(drift_ppm + ci),
    )


def fit_all_channels(
    segments: Iterable[SegmentDelay],
    *,
    channels: Iterable[int],
    reference_channel: int,
    sample_rate: int,
    config: FitConfig,
) -> tuple[DriftFit, ...]:
    grouped: dict[int, list[SegmentDelay]] = {
        channel: [] for channel in channels
    }
    for segment in segments:
        grouped.setdefault(segment.channel, []).append(segment)
    return tuple(
        fit_channel_drift(
            grouped.get(channel, []),
            channel=channel,
            sample_rate=sample_rate,
            config=config,
            reference_channel=reference_channel,
        )
        for channel in sorted(grouped)
    )


def compare_drift_fits(
    old_fits: Iterable[DriftFit],
    new_fits: Iterable[DriftFit],
    segments: Iterable[SegmentDelay],
) -> dict[str, Any]:
    old_by_channel = {fit.channel: fit for fit in old_fits}
    new_by_channel = {fit.channel: fit for fit in new_fits}
    segment_by_key = {
        (segment.channel, segment.segment_index): segment for segment in segments
    }
    channels: list[dict[str, Any]] = []
    for channel in sorted(set(old_by_channel).union(new_by_channel)):
        old = old_by_channel.get(channel)
        new = new_by_channel.get(channel)
        item: dict[str, Any] = {
            "channel": channel,
            "old": old.to_dict() if old is not None else None,
            "new": new.to_dict() if new is not None else None,
            "initial_offset_delta_samples": (
                new.initial_offset_samples - old.initial_offset_samples
                if old is not None and new is not None
                else None
            ),
            "drift_ppm_delta": (
                new.drift_ppm - old.drift_ppm
                if old is not None and new is not None
                else None
            ),
            "segments": [],
        }
        if old is not None and new is not None:
            old_predictions = {
                int(entry["segment_index"]): float(
                    entry["predicted_delay_samples"]
                )
                for entry in old.predicted_by_segment
            }
            new_predictions = {
                int(entry["segment_index"]): float(
                    entry["predicted_delay_samples"]
                )
                for entry in new.predicted_by_segment
            }
            for segment_index in sorted(
                set(old_predictions).union(new_predictions)
            ):
                old_predicted = old_predictions.get(segment_index)
                new_predicted = new_predictions.get(segment_index)
                segment = segment_by_key.get((channel, segment_index))
                observed = segment.delay_samples if segment is not None else None
                item["segments"].append(
                    {
                        "segment_index": segment_index,
                        "old_predicted_samples": old_predicted,
                        "new_predicted_samples": new_predicted,
                        "prediction_delta_samples": (
                            new_predicted - old_predicted
                            if old_predicted is not None and new_predicted is not None
                            else None
                        ),
                        "observed_delay_samples": observed,
                        "new_prediction_error_samples": (
                            observed - new_predicted
                            if observed is not None and new_predicted is not None
                            else None
                        ),
                    }
                )
        channels.append(item)
    return {
        "schema_version": "fit-comparison-v1",
        "channels": channels,
    }

