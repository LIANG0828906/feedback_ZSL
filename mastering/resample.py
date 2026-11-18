from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import Issue


@dataclass(frozen=True)
class DriftAnchor:
    source_sample: float
    target_seconds: float

    def as_dict(self) -> dict[str, Any]:
        return {
            "source_sample": _clean_float(self.source_sample),
            "target_seconds": _clean_float(self.target_seconds),
        }


@dataclass(frozen=True)
class DriftMap:
    anchors: tuple[DriftAnchor, ...]
    input_sample_rate: int
    output_sample_rate: int
    enabled: bool

    def target_to_source(self, target_seconds: np.ndarray | float) -> np.ndarray:
        times = np.asarray(target_seconds, dtype=np.float64)
        scalar = times.ndim == 0
        times = np.atleast_1d(times)
        anchor_t = np.asarray([a.target_seconds for a in self.anchors], dtype=np.float64)
        anchor_s = np.asarray([a.source_sample for a in self.anchors], dtype=np.float64)
        result = np.interp(times, anchor_t, anchor_s)
        left = times <= anchor_t[0]
        if np.any(left):
            slope = (anchor_s[1] - anchor_s[0]) / (anchor_t[1] - anchor_t[0])
            result[left] = anchor_s[0] + (times[left] - anchor_t[0]) * slope
        right = times >= anchor_t[-1]
        if np.any(right):
            slope = (anchor_s[-1] - anchor_s[-2]) / (anchor_t[-1] - anchor_t[-2])
            result[right] = anchor_s[-1] + (times[right] - anchor_t[-1]) * slope
        return result[0] if scalar else result

    def source_to_target(self, source_sample: np.ndarray | float) -> np.ndarray:
        source = np.asarray(source_sample, dtype=np.float64)
        scalar = source.ndim == 0
        source = np.atleast_1d(source)
        anchor_t = np.asarray([a.target_seconds for a in self.anchors], dtype=np.float64)
        anchor_s = np.asarray([a.source_sample for a in self.anchors], dtype=np.float64)
        result = np.interp(source, anchor_s, anchor_t)
        left = source <= anchor_s[0]
        if np.any(left):
            slope = (anchor_t[1] - anchor_t[0]) / (anchor_s[1] - anchor_s[0])
            result[left] = anchor_t[0] + (source[left] - anchor_s[0]) * slope
        right = source >= anchor_s[-1]
        if np.any(right):
            slope = (anchor_t[-1] - anchor_t[-2]) / (anchor_s[-1] - anchor_s[-2])
            result[right] = anchor_t[-1] + (source[right] - anchor_s[-1]) * slope
        return result[0] if scalar else result

    def summary(self) -> dict[str, Any]:
        return {
            "enabled": self.enabled,
            "input_sample_rate": self.input_sample_rate,
            "output_sample_rate": self.output_sample_rate,
            "anchor_count": len(self.anchors),
            "anchors": [anchor.as_dict() for anchor in self.anchors],
        }


def _clean_float(value: float) -> float:
    rounded = round(float(value), 12)
    return 0.0 if rounded == 0.0 else rounded


def _finite(value: Any) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    result = float(value)
    return result if math.isfinite(result) else None


def build_drift_map(
    track_name: str,
    raw_drift: Any,
    input_frames: int,
    input_sample_rate: int,
    output_sample_rate: int,
    project_tick_rate: float,
) -> tuple[DriftMap, list[Issue]]:
    issues: list[Issue] = []
    identity = (
        DriftAnchor(0.0, 0.0),
        DriftAnchor(float(input_frames), float(input_frames) / input_sample_rate),
    )
    if raw_drift is None:
        return (
            DriftMap(identity, input_sample_rate, output_sample_rate, enabled=False),
            issues,
        )
    if not isinstance(raw_drift, dict):
        issues.append(
            Issue(
                "INVALID_DRIFT_ANCHORS",
                "drift must be an object",
                track_name,
            )
        )
        return DriftMap(identity, input_sample_rate, output_sample_rate, False), issues

    raw_anchors = raw_drift.get("anchors")
    if not isinstance(raw_anchors, list) or len(raw_anchors) < 2:
        issues.append(
            Issue(
                "INVALID_DRIFT_ANCHORS",
                "drift requires at least two anchors",
                track_name,
            )
        )
        return DriftMap(identity, input_sample_rate, output_sample_rate, False), issues
    tick_rate = _finite(raw_drift.get("tick_rate", project_tick_rate))
    if tick_rate is None or tick_rate <= 0.0:
        issues.append(
            Issue(
                "INVALID_DRIFT_ANCHORS",
                "anchor tick rate must be positive",
                track_name,
            )
        )
        return DriftMap(identity, input_sample_rate, output_sample_rate, False), issues

    anchors: list[DriftAnchor] = []
    for index, raw_anchor in enumerate(raw_anchors):
        if not isinstance(raw_anchor, dict):
            issues.append(
                Issue(
                    "INVALID_DRIFT_ANCHORS",
                    "anchor must be an object",
                    track_name,
                    {"anchor_index": index},
                )
            )
            continue
        source = _finite(raw_anchor.get("source_sample"))
        target_seconds: float | None = None
        if "target_tick" in raw_anchor:
            tick = _finite(raw_anchor["target_tick"])
            if tick is not None:
                target_seconds = tick / tick_rate
        elif "target_time_seconds" in raw_anchor:
            target_seconds = _finite(raw_anchor["target_time_seconds"])
        elif "target_seconds" in raw_anchor:
            target_seconds = _finite(raw_anchor["target_seconds"])
        if source is None or target_seconds is None:
            issues.append(
                Issue(
                    "INVALID_DRIFT_ANCHORS",
                    "anchor has non-finite source or target coordinate",
                    track_name,
                    {"anchor_index": index},
                )
            )
            continue
        anchors.append(DriftAnchor(source, target_seconds))

    if issues:
        return DriftMap(identity, input_sample_rate, output_sample_rate, False), issues
    for index in range(1, len(anchors)):
        if anchors[index].target_seconds <= anchors[index - 1].target_seconds:
            issues.append(
                Issue(
                    "INVALID_DRIFT_ANCHORS",
                    "anchor target times must be strictly increasing",
                    track_name,
                    {"anchor_index": index},
                )
            )
        if anchors[index].source_sample <= anchors[index - 1].source_sample:
            issues.append(
                Issue(
                    "INVALID_DRIFT_ANCHORS",
                    "anchor source samples must be strictly increasing",
                    track_name,
                    {"anchor_index": index},
                )
            )
    if issues:
        return DriftMap(identity, input_sample_rate, output_sample_rate, False), issues
    return DriftMap(
        tuple(anchors),
        input_sample_rate,
        output_sample_rate,
        enabled=True,
    ), issues


def _interpolate_channel(source: np.ndarray, source_positions: np.ndarray) -> np.ndarray:
    """Four-point Catmull-Rom interpolation with deterministic zero boundaries."""

    length = source.shape[0]
    base = np.floor(source_positions).astype(np.int64)
    fraction = source_positions - base
    valid = (base >= 0) & (base < length)
    result = np.zeros(source_positions.shape, dtype=np.float64)
    if not np.any(valid):
        return result
    indexes = base[valid]
    f = fraction[valid]

    def gather(offset: int) -> np.ndarray:
        idx = indexes + offset
        values = np.zeros(idx.shape, dtype=np.float64)
        inside = (idx >= 0) & (idx < length)
        values[inside] = source[idx[inside]]
        return values

    y0 = gather(-1)
    y1 = gather(0)
    y2 = gather(1)
    y3 = gather(2)
    value = 0.5 * (
        2.0 * y1
        + (-y0 + y2) * f
        + (2.0 * y0 - 5.0 * y1 + 4.0 * y2 - y3) * f * f
        + (-y0 + 3.0 * y1 - 3.0 * y2 + y3) * f * f * f
    )
    result[valid] = value
    return result


def render_track(
    source: np.ndarray,
    sample_rate: int,
    output_sample_rate: int,
    output_frames: int,
    drift_map: DriftMap,
    output_channels: int,
    pan: float = 0.0,
    block_frames: int = 262144,
) -> np.ndarray:
    """Resample/time-warp to an exact output frame count and map channel layout."""

    if source.ndim != 2:
        raise ValueError("source must be shape (frames, channels)")
    if not (-1.0 <= pan <= 1.0):
        raise ValueError("pan must be in [-1, 1]")
    result = np.zeros((output_frames, output_channels), dtype=np.float64)
    for start in range(0, output_frames, block_frames):
        stop = min(output_frames, start + block_frames)
        target_times = np.arange(start, stop, dtype=np.float64) / output_sample_rate
        source_positions = drift_map.target_to_source(target_times)
        block = np.zeros((stop - start, source.shape[1]), dtype=np.float64)
        for channel in range(source.shape[1]):
            block[:, channel] = _interpolate_channel(source[:, channel], source_positions)
        result[start:stop] = _map_channels(block, output_channels, pan)
    return result


def _map_channels(source: np.ndarray, output_channels: int, pan: float) -> np.ndarray:
    input_channels = source.shape[1]
    frames = source.shape[0]
    if input_channels == output_channels:
        return source.copy()
    result = np.zeros((frames, output_channels), dtype=np.float64)
    if input_channels == 1:
        if output_channels == 1:
            result[:, 0] = source[:, 0]
        elif output_channels == 2:
            left_gain = 1.0 if pan <= 0.0 else 1.0 - pan
            right_gain = 1.0 if pan >= 0.0 else 1.0 + pan
            result[:, 0] = source[:, 0] * left_gain
            result[:, 1] = source[:, 0] * right_gain
        else:
            result[:] = source[:, :1]
        return result
    if input_channels == 2 and output_channels == 1:
        result[:, 0] = (source[:, 0] + source[:, 1]) * 0.5
        return result
    copied = min(input_channels, output_channels)
    result[:, :copied] = source[:, :copied]
    if input_channels == 1:
        result[:, copied:] = source[:, :1]
    return result


def apply_integer_shift(audio: np.ndarray, shift_samples: int) -> np.ndarray:
    """Shift along time while preserving frame count exactly."""

    if shift_samples == 0:
        return audio
    frames = audio.shape[0]
    result = np.zeros_like(audio)
    if abs(shift_samples) >= frames:
        return result
    if shift_samples > 0:
        result[shift_samples:] = audio[: frames - shift_samples]
    else:
        result[: frames + shift_samples] = audio[-shift_samples:]
    return result

