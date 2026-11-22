from __future__ import annotations

import math
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, replace
from typing import Any

import numpy as np

from .bounds import (
    BoundDerivation,
    ErrorBounds,
    ObservedErrors,
    check_bound,
    compute_bounds,
    interpolation_error,
)
from .config import AnalysisConfig, RenderConfig, WorkbenchConfig
from .errors import ErrorCode, WorkbenchError, frame_error
from .model import (
    AcousticSample,
    FrameTruth,
    frame_truth_for_sample,
    validate_construction_order,
)

EPS = 1.0e-30


@dataclass(frozen=True)
class ChannelFrameEstimate:
    channel_id: str
    frame_index: int
    direct_delay_samples: float
    direct_delay_s: float
    direct_energy: float
    early_reflection_energy: float
    reverb_energy: float
    total_energy: float
    drr: float
    tdoa_samples: float
    tdoa_s: float
    peak_index: int
    peak_fraction: float
    peak_amplitude: float
    observed_errors: ObservedErrors

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel": self.channel_id,
            "frame_index": self.frame_index,
            "direct_delay_samples": self.direct_delay_samples,
            "direct_delay_s": self.direct_delay_s,
            "direct_energy": self.direct_energy,
            "early_reflection_energy": self.early_reflection_energy,
            "reverb_energy": self.reverb_energy,
            "total_energy": self.total_energy,
            "drr": self.drr,
            "tdoa_samples": self.tdoa_samples,
            "tdoa_s": self.tdoa_s,
            "peak_index": self.peak_index,
            "peak_fraction": self.peak_fraction,
            "peak_amplitude": self.peak_amplitude,
            "observed_errors": self.observed_errors.to_dict(),
        }


@dataclass(frozen=True)
class FrameResult:
    frame_index: int
    channels: tuple[ChannelFrameEstimate, ...]
    bounds: ErrorBounds
    cumulative_bounds: ErrorBounds
    cumulative_errors: ObservedErrors
    algorithm_version: str
    input_hash: str
    config: WorkbenchConfig
    derivation: BoundDerivation

    @property
    def max_observed_errors(self) -> ObservedErrors:
        aggregate = ObservedErrors()
        for channel in self.channels:
            aggregate = aggregate.maximum(channel.observed_errors)
        return aggregate

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame_index": self.frame_index,
            "channels": [channel.to_dict() for channel in self.channels],
            "bounds": self.bounds.to_dict(),
            "cumulative_bounds": self.cumulative_bounds.to_dict(),
            "cumulative_errors": self.cumulative_errors.to_dict(),
            "max_observed_errors": self.max_observed_errors.to_dict(),
            "algorithm_version": self.algorithm_version,
            "input_hash": self.input_hash,
            "config": self.config.to_dict(),
            "derivation": self.derivation.to_dict(),
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> "FrameResult":
        from .config import AnalysisConfig, RenderConfig, WorkbenchConfig

        channels: list[ChannelFrameEstimate] = []
        for item in payload["channels"]:
            errors = item.get("observed_errors", {})
            channels.append(
                ChannelFrameEstimate(
                    channel_id=str(item["channel"]),
                    frame_index=int(item["frame_index"]),
                    direct_delay_samples=float(item["direct_delay_samples"]),
                    direct_delay_s=float(item["direct_delay_s"]),
                    direct_energy=float(item["direct_energy"]),
                    early_reflection_energy=float(
                        item["early_reflection_energy"]
                    ),
                    reverb_energy=float(item["reverb_energy"]),
                    total_energy=float(item["total_energy"]),
                    drr=float(item["drr"]),
                    tdoa_samples=float(item["tdoa_samples"]),
                    tdoa_s=float(item["tdoa_s"]),
                    peak_index=int(item["peak_index"]),
                    peak_fraction=float(item["peak_fraction"]),
                    peak_amplitude=float(item["peak_amplitude"]),
                    observed_errors=ObservedErrors(
                        peak_quantization_s=float(
                            errors.get("peak_quantization_s", 0.0)
                        ),
                        energy_truncation_relative=float(
                            errors.get("energy_truncation_relative", 0.0)
                        ),
                        tdoa_s=float(errors.get("tdoa_s", 0.0)),
                        convolution_interpolation_relative=float(
                            errors.get("convolution_interpolation_relative", 0.0)
                        ),
                    ),
                )
            )
        bounds = payload["bounds"]
        cumulative_bounds = payload["cumulative_bounds"]
        cumulative = payload["cumulative_errors"]
        config_payload = payload["config"]
        analysis_payload = config_payload["analysis"]
        render_payload = config_payload["render"]
        config = WorkbenchConfig(
            analysis=AnalysisConfig(**analysis_payload),
            render=RenderConfig(
                conv_window_len=int(render_payload["conv_window_len"]),
                block_size=int(render_payload["block_size"]),
                interpolation_order=int(render_payload["interpolation_order"]),
                channel_map=(
                    None
                    if render_payload.get("channel_map") is None
                    else tuple(render_payload["channel_map"])
                ),
            ),
            max_workers=int(config_payload.get("max_workers", 1)),
            per_thread_buffer_samples=int(
                config_payload.get("per_thread_buffer_samples", 2_000_000)
            ),
            algorithm_version=str(
                config_payload.get(
                    "algorithm_version",
                    payload.get("algorithm_version", "spatial-workbench-1.0.0"),
                )
            ),
        )
        derivation_payload = payload["derivation"]
        derivation = BoundDerivation(
            sample_quantum_s=float(derivation_payload["sample_quantum_s"]),
            peak_quantization=str(derivation_payload["peak_quantization"]),
            energy_truncation=str(derivation_payload["energy_truncation"]),
            tdoa=str(derivation_payload["tdoa"]),
            convolution_interpolation=str(
                derivation_payload["convolution_interpolation"]
            ),
        )
        return cls(
            frame_index=int(payload["frame_index"]),
            channels=tuple(channels),
            bounds=ErrorBounds(
                peak_quantization_s=float(bounds["peak_quantization_s"]),
                energy_truncation_relative=float(
                    bounds["energy_truncation_relative"]
                ),
                tdoa_s=float(bounds["tdoa_s"]),
                convolution_interpolation_relative=float(
                    bounds["convolution_interpolation_relative"]
                ),
            ),
            cumulative_bounds=ErrorBounds(
                peak_quantization_s=float(
                    cumulative_bounds["peak_quantization_s"]
                ),
                energy_truncation_relative=float(
                    cumulative_bounds["energy_truncation_relative"]
                ),
                tdoa_s=float(cumulative_bounds["tdoa_s"]),
                convolution_interpolation_relative=float(
                    cumulative_bounds["convolution_interpolation_relative"]
                ),
            ),
            cumulative_errors=ObservedErrors(
                peak_quantization_s=float(cumulative["peak_quantization_s"]),
                energy_truncation_relative=float(
                    cumulative["energy_truncation_relative"]
                ),
                tdoa_s=float(cumulative["tdoa_s"]),
                convolution_interpolation_relative=float(
                    cumulative["convolution_interpolation_relative"]
                ),
            ),
            algorithm_version=str(payload["algorithm_version"]),
            input_hash=str(payload["input_hash"]),
            config=config,
            derivation=derivation,
        )


def _fractional_peak(
    magnitude: np.ndarray,
    peak_index: int,
    order: int,
) -> float:
    if peak_index <= 0 or peak_index >= magnitude.size - 1:
        return 0.0
    if order <= 1:
        left = float(magnitude[peak_index - 1])
        center = float(magnitude[peak_index])
        right = float(magnitude[peak_index + 1])
        denominator = left - 2.0 * center + right
        if abs(denominator) <= 1.0e-20:
            return 0.0
        fraction = 0.5 * (left - right) / denominator
        return float(np.clip(fraction, -0.5, 0.5))

    point_count = min(order + 1, magnitude.size)
    left_count = point_count // 2
    left_index = peak_index - left_count
    right_index = left_index + point_count
    if left_index < 0:
        left_index = 0
        right_index = point_count
    if right_index > magnitude.size:
        right_index = magnitude.size
        left_index = right_index - point_count
    x = np.arange(left_index - peak_index, right_index - peak_index, dtype=np.float64)
    y = magnitude[left_index:right_index].astype(np.float64)
    if x.size < 2:
        return 0.0
    try:
        coefficients = np.polyfit(x, y, min(order, x.size - 1))
        derivative = np.polyder(coefficients)
        roots = np.roots(derivative) if derivative.size > 1 else np.empty(0)
        candidates = [
            float(root.real)
            for root in roots
            if abs(root.imag) < 1.0e-7 and -1.0 <= root.real <= 1.0
        ]
        if not candidates:
            return 0.0
        best = max(
            candidates,
            key=lambda value: float(np.polyval(coefficients, value)),
        )
        return float(np.clip(best, -0.5, 0.5))
    except (np.linalg.LinAlgError, ValueError):
        return 0.0


def _energy_between(
    ir: np.ndarray,
    start: int,
    end: int,
) -> float:
    low = max(0, int(start))
    high = min(ir.size, int(end))
    if high <= low:
        return 0.0
    block = ir[low:high]
    return float(np.dot(block, block))


def _estimate_channel(
    sample: AcousticSample,
    frame_index: int,
    channel_index: int,
    config: AnalysisConfig,
    truth: FrameTruth | None,
) -> ChannelFrameEstimate:
    channel = sample.layout.channels[channel_index]
    start = frame_index * config.hop
    end = start + config.frame_len
    data = sample.ir[channel_index]
    segment = data[start:end]
    used_length = min(config.frame_len, config.hop)
    used = data[start : start + used_length]
    if segment.size < config.frame_len:
        raise frame_error(
            "frame is truncated by the end of the impulse response",
            channel=channel.id,
            frame_index=frame_index,
            details={
                "available_samples": int(segment.size),
                "frame_len": int(config.frame_len),
            },
        )
    if not np.all(np.isfinite(used)):
        bad = int(np.flatnonzero(~np.isfinite(used))[0])
        raise frame_error(
            "frame contains NaN or infinite samples",
            channel=channel.id,
            frame_index=frame_index,
            sample_index=start + bad,
        )

    absolute_segment = np.abs(used)
    maximum = float(np.max(absolute_segment))
    if maximum > config.clip_ceiling:
        bad = int(np.argmax(absolute_segment > config.clip_ceiling))
        raise WorkbenchError(
            ErrorCode.CLIP,
            "frame gain exceeds the configured clip ceiling",
            field="clip_ceiling",
            channel=channel.id,
            frame_index=frame_index,
            sample_index=start + bad,
            details={"amplitude": maximum, "clip_ceiling": config.clip_ceiling},
        )
    threshold = 10.0 ** (config.energy_threshold_db / 20.0)
    if not math.isfinite(maximum) or maximum < threshold:
        raise frame_error(
            "frame is silent below the energy threshold",
            channel=channel.id,
            frame_index=frame_index,
            details={"peak": maximum, "threshold": threshold},
        )

    search = absolute_segment[: config.window_len]
    peak_local = int(np.argmax(search))
    fraction = _fractional_peak(search, peak_local, config.interpolation_order)
    delay_samples = float(start + peak_local + fraction)
    center = int(round(delay_samples))
    direct_energy = _energy_between(
        data,
        center - config.direct_half_width,
        center + config.direct_half_width + 1,
    )
    early_start = center + config.direct_half_width + 1
    early_end = start + config.window_len
    early_energy = _energy_between(data, early_start, early_end)
    late_start = start + config.window_len
    late_end = start + min(config.frame_len, config.hop)
    reverb_energy = _energy_between(data, late_start, late_end)
    total_energy = float(np.dot(used, used))
    drr = direct_energy / max(reverb_energy, EPS)

    if truth is None:
        peak_error = 0.0
        energy_error = 0.0
        tdoa_error = 0.0
    else:
        peak_error = abs(delay_samples - truth.direct_delay_samples) / float(
            sample.sample_rate
        )
        energy_relative_early = abs(
            early_energy - truth.early_reflection_energy
        ) / max(abs(truth.early_reflection_energy), EPS)
        energy_relative_reverb = abs(
            reverb_energy - truth.reverb_energy
        ) / max(abs(truth.reverb_energy), EPS)
        energy_relative_direct = abs(
            direct_energy - truth.direct_energy
        ) / max(abs(truth.direct_energy), EPS)
        energy_error = min(
            1.0,
            max(
                energy_relative_early,
                energy_relative_reverb,
                energy_relative_direct,
            ),
        )
        tdoa_error = 0.0

    conv_error = interpolation_error(
        fraction % 1.0,
        config.interpolation_order,
    )
    observed = ObservedErrors(
        peak_quantization_s=peak_error,
        energy_truncation_relative=energy_error,
        tdoa_s=tdoa_error,
        convolution_interpolation_relative=conv_error,
    )
    return ChannelFrameEstimate(
        channel_id=channel.id,
        frame_index=frame_index,
        direct_delay_samples=delay_samples,
        direct_delay_s=delay_samples / float(sample.sample_rate),
        direct_energy=direct_energy,
        early_reflection_energy=early_energy,
        reverb_energy=reverb_energy,
        total_energy=total_energy,
        drr=float(drr),
        tdoa_samples=0.0,
        tdoa_s=0.0,
        peak_index=start + peak_local,
        peak_fraction=fraction,
        peak_amplitude=maximum,
        observed_errors=observed,
    )


def _validate_sample_channels(sample: AcousticSample) -> None:
    if sample.channel_count != sample.layout.count:
        raise WorkbenchError(
            ErrorCode.CHAN,
            "loaded IR channel count does not match declared layout",
            field="ir",
            details={
                "ir_channels": sample.channel_count,
                "layout_channels": sample.layout.count,
            },
        )
    if sample.dry.shape[0] == 0:
        raise WorkbenchError(
            ErrorCode.CHAN,
            "dry clip has no output source channels",
            field="dry",
        )


def analyze_frame(
    sample: AcousticSample,
    frame_index: int,
    config: WorkbenchConfig,
    *,
    previous_cumulative_errors: ObservedErrors | None = None,
    executor: ThreadPoolExecutor | None = None,
) -> FrameResult:
    _validate_sample_channels(sample)
    validate_construction_order(sample)
    config.validate()
    frame_count = sample.frame_count(config.analysis)
    if not 0 <= frame_index < frame_count:
        raise frame_error(
            "frame index is outside the valid range",
            channel=sample.layout.channels[0].id,
            frame_index=frame_index,
            details={"frame_count": frame_count},
        )
    start = frame_index * config.analysis.hop
    if start + config.analysis.frame_len > sample.ir_length:
        raise frame_error(
            "frame extends beyond the impulse response",
            channel=sample.layout.channels[0].id,
            frame_index=frame_index,
        )

    truths = frame_truth_for_sample(sample, frame_index, config.analysis)
    truth_by_channel = {truth.channel_id: truth for truth in truths}
    bounds, derivation = compute_bounds(
        sample.sample_rate,
        config.analysis,
        config.render,
    )

    def task(channel_index: int) -> ChannelFrameEstimate:
        channel_id = sample.layout.channels[channel_index].id
        return _estimate_channel(
            sample,
            frame_index,
            channel_index,
            config.analysis,
            truth_by_channel.get(channel_id),
        )

    if executor is not None and config.max_workers > 1 and sample.channel_count > 1:
        estimates = list(executor.map(task, range(sample.channel_count)))
    else:
        estimates = [task(index) for index in range(sample.channel_count)]

    reference_delay = estimates[0].direct_delay_samples
    adjusted: list[ChannelFrameEstimate] = []
    for estimate in estimates:
        truth = truth_by_channel[estimate.channel_id]
        tdoa_samples = estimate.direct_delay_samples - reference_delay
        tdoa_s = tdoa_samples / float(sample.sample_rate)
        tdoa_error = abs(tdoa_samples - truth.tdoa_samples) / float(
            sample.sample_rate
        )
        observed = replace(
            estimate.observed_errors,
            tdoa_s=tdoa_error,
        )
        adjusted_estimate = replace(
            estimate,
            tdoa_samples=tdoa_samples,
            tdoa_s=tdoa_s,
            observed_errors=observed,
        )
        failure = check_bound(observed=observed, bounds=bounds)
        if failure is not None:
            metric, excess = failure
            raise WorkbenchError(
                ErrorCode.BOUND,
                "observed estimation error exceeds its derived upper bound",
                field=metric,
                channel=estimate.channel_id,
                frame_index=frame_index,
                excess=excess,
                details={
                    "observed": observed.to_dict(),
                    "bounds": bounds.to_dict(),
                },
            )
        adjusted.append(adjusted_estimate)

    frame_errors = ObservedErrors()
    for estimate in adjusted:
        frame_errors = frame_errors.maximum(estimate.observed_errors)
    cumulative_errors = (
        frame_errors
        if previous_cumulative_errors is None
        else previous_cumulative_errors.plus(frame_errors)
    )
    cumulative_bounds = bounds.scaled(frame_index)
    failure = check_bound(
        observed=cumulative_errors,
        bounds=cumulative_bounds,
    )
    if failure is not None:
        metric, excess = failure
        raise WorkbenchError(
            ErrorCode.BOUND,
            "cumulative observed error exceeds its cumulative upper bound",
            field=metric,
            channel=adjusted[0].channel_id,
            frame_index=frame_index,
            excess=excess,
            details={
                "cumulative_observed": cumulative_errors.to_dict(),
                "cumulative_bounds": cumulative_bounds.to_dict(),
            },
        )

    return FrameResult(
        frame_index=frame_index,
        channels=tuple(adjusted),
        bounds=bounds,
        cumulative_bounds=cumulative_bounds,
        cumulative_errors=cumulative_errors,
        algorithm_version=config.algorithm_version,
        input_hash=sample.input_hash(),
        config=config,
        derivation=derivation,
    )
