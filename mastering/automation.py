from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .errors import Issue, ProjectRejected


@dataclass(frozen=True)
class AutomationNode:
    time_seconds: float
    gain_db: float

    def as_dict(self) -> dict[str, float]:
        return {"time_seconds": self.time_seconds, "gain_db": self.gain_db}


@dataclass(frozen=True)
class SmoothRegion:
    start_seconds: float
    end_seconds: float
    start_gain_db: float
    end_gain_db: float
    source_segment_count: int

    @property
    def duration_seconds(self) -> float:
        return self.end_seconds - self.start_seconds

    def as_dict(self) -> dict[str, Any]:
        return {
            "start_seconds": _clean_float(self.start_seconds),
            "end_seconds": _clean_float(self.end_seconds),
            "duration_seconds": _clean_float(self.duration_seconds),
            "start_gain_db": _clean_float(self.start_gain_db),
            "end_gain_db": _clean_float(self.end_gain_db),
            "merged_sharp_segments": self.source_segment_count,
        }


@dataclass(frozen=True)
class AutomationPlan:
    track: str
    nodes: tuple[AutomationNode, ...]
    regions: tuple[SmoothRegion, ...]
    sharp_segment_count: int
    smoothing_duration_seconds: float
    smoothing_region_count: int
    _unique_times: np.ndarray
    _unique_gains: np.ndarray

    @property
    def start_seconds(self) -> float:
        return self.nodes[0].time_seconds

    @property
    def end_seconds(self) -> float:
        return self.nodes[-1].time_seconds

    def baseline_gain_db(self, times: np.ndarray | float) -> np.ndarray:
        values = np.asarray(times, dtype=np.float64)
        scalar = values.ndim == 0
        result = np.interp(
            np.atleast_1d(values),
            self._unique_times,
            self._unique_gains,
        )
        return result[0] if scalar else result

    def gain_db_at(self, times: np.ndarray | float) -> np.ndarray:
        values = np.asarray(times, dtype=np.float64)
        scalar = values.ndim == 0
        values = np.atleast_1d(values)
        result = self.baseline_gain_db(values)
        for region in self.regions:
            mask = (values >= region.start_seconds) & (values <= region.end_seconds)
            if not np.any(mask):
                continue
            span = region.end_seconds - region.start_seconds
            if span <= 0.0:
                continue
            u = (values[mask] - region.start_seconds) / span
            smoothstep = u * u * (3.0 - 2.0 * u)
            result[mask] = region.start_gain_db + (
                region.end_gain_db - region.start_gain_db
            ) * smoothstep
        return result[0] if scalar else result

    def linear_gain_at(self, times: np.ndarray | float) -> np.ndarray:
        return np.power(10.0, self.gain_db_at(times) / 20.0)

    def summary(self) -> dict[str, Any]:
        return {
            "node_count": len(self.nodes),
            "time_range_seconds": [
                _clean_float(self.start_seconds),
                _clean_float(self.end_seconds),
            ],
            "sharp_segments_smoothed": self.sharp_segment_count,
            "smoothing_region_count": self.smoothing_region_count,
            "total_smoothing_seconds": _clean_float(self.smoothing_duration_seconds),
            "smoothing_regions": [region.as_dict() for region in self.regions],
        }


@dataclass(frozen=True)
class SmoothingConfig:
    gain_min_db: float
    gain_max_db: float
    max_interval_seconds: float
    min_gain_delta_db: float
    duration_seconds: float


def _clean_float(value: float) -> float:
    rounded = round(float(value), 12)
    return 0.0 if rounded == 0.0 else rounded


def _finite_number(value: Any) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    result = float(value)
    return result if math.isfinite(result) else None


def _parse_nodes(raw_curve: Any, track: str) -> tuple[list[AutomationNode] | None, list[Issue]]:
    issues: list[Issue] = []
    if isinstance(raw_curve, dict):
        raw_nodes = raw_curve.get("nodes")
    else:
        raw_nodes = raw_curve
    if not isinstance(raw_nodes, list) or not raw_nodes:
        return None, [
            Issue(
                "INVALID_AUTOMATION_CURVE",
                "curve must contain a non-empty nodes array",
                track,
            )
        ]
    nodes: list[AutomationNode] = []
    for index, raw_node in enumerate(raw_nodes):
        time_value: Any = None
        gain_value: Any = None
        if isinstance(raw_node, list) and len(raw_node) == 2:
            time_value, gain_value = raw_node
        elif isinstance(raw_node, dict):
            if "time_seconds" in raw_node:
                time_value = raw_node.get("time_seconds")
            elif "time" in raw_node:
                time_value = raw_node.get("time")
            if "gain_db" in raw_node:
                gain_value = raw_node.get("gain_db")
            elif "gain" in raw_node:
                gain_value = raw_node.get("gain")
        time_seconds = _finite_number(time_value)
        gain_db = _finite_number(gain_value)
        if time_seconds is None or gain_db is None:
            issues.append(
                Issue(
                    "INVALID_AUTOMATION_CURVE",
                    "node must contain finite time and gain values",
                    track,
                    {"node_index": index},
                )
            )
            continue
        nodes.append(AutomationNode(time_seconds, gain_db))
    return (nodes if not issues else None), issues


def _merge_intervals(
    intervals: list[tuple[float, float, int]]
) -> list[tuple[float, float, int]]:
    if not intervals:
        return []
    intervals = sorted(intervals, key=lambda item: (item[0], item[1]))
    merged: list[tuple[float, float, int]] = []
    for start, end, count in intervals:
        if not merged or start > merged[-1][1] + 1e-12:
            merged.append((start, end, count))
        else:
            old_start, old_end, old_count = merged[-1]
            merged[-1] = (old_start, max(old_end, end), old_count + count)
    return merged


def _build_plan(
    track: str,
    nodes: list[AutomationNode],
    config: SmoothingConfig,
) -> AutomationPlan:
    unique_times: list[float] = []
    unique_gains: list[float] = []
    for node in nodes:
        if unique_times and node.time_seconds == unique_times[-1]:
            unique_gains[-1] = node.gain_db
        else:
            unique_times.append(node.time_seconds)
            unique_gains.append(node.gain_db)

    time_values = np.asarray(unique_times, dtype=np.float64)
    gain_values = np.asarray(unique_gains, dtype=np.float64)
    curve_start = float(time_values[0])
    curve_end = float(time_values[-1])

    intervals: list[tuple[float, float, int]] = []
    sharp_count = 0
    for left, right in zip(nodes[:-1], nodes[1:]):
        interval = right.time_seconds - left.time_seconds
        delta = abs(right.gain_db - left.gain_db)
        if interval <= config.max_interval_seconds and delta >= config.min_gain_delta_db:
            sharp_count += 1
            midpoint = (left.time_seconds + right.time_seconds) * 0.5
            start = max(curve_start, midpoint - config.duration_seconds * 0.5)
            end = min(curve_end, midpoint + config.duration_seconds * 0.5)
            if end > start:
                intervals.append((start, end, 1))

    regions: list[SmoothRegion] = []
    for start, end, count in _merge_intervals(intervals):
        start_gain = float(np.interp(start, time_values, gain_values))
        end_gain = float(np.interp(end, time_values, gain_values))
        regions.append(SmoothRegion(start, end, start_gain, end_gain, count))

    return AutomationPlan(
        track=track,
        nodes=tuple(nodes),
        regions=tuple(regions),
        sharp_segment_count=sharp_count,
        smoothing_duration_seconds=sum(region.duration_seconds for region in regions),
        smoothing_region_count=len(regions),
        _unique_times=time_values,
        _unique_gains=gain_values,
    )


def load_and_validate_automation(
    path: str | Path,
    track_names: Iterable[str],
    config: SmoothingConfig,
) -> dict[str, AutomationPlan]:
    """Validate every curve before any audio processing starts."""

    names = list(track_names)
    issues: list[Issue] = []
    try:
        with Path(path).open("r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except FileNotFoundError:
        raise ProjectRejected(
            [Issue("FILE_NOT_FOUND", f"automation file not found: {path}")]
        )
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ProjectRejected(
            [Issue("INVALID_JSON", f"cannot read automation JSON: {exc}")]
        )

    raw_curves = raw.get("tracks", raw.get("curves")) if isinstance(raw, dict) else raw
    curve_map: dict[str, Any] = {}
    if isinstance(raw_curves, dict):
        curve_map = dict(raw_curves)
    elif isinstance(raw_curves, list):
        for item in raw_curves:
            if not isinstance(item, dict):
                issues.append(
                    Issue(
                        "INVALID_AUTOMATION_CURVE",
                        "automation list entries must be objects",
                    )
                )
                continue
            name = item.get("track", item.get("name", item.get("id")))
            if name is None:
                issues.append(
                    Issue(
                        "INVALID_AUTOMATION_CURVE",
                        "automation entry has no track name",
                    )
                )
                continue
            curve_map[str(name)] = item
    else:
        issues.append(
            Issue(
                "INVALID_AUTOMATION_CURVE",
                "automation root must be an object or a list",
            )
        )

    for name in names:
        if name not in curve_map:
            issues.append(
                Issue(
                    "INVALID_AUTOMATION_CURVE",
                    "missing curve for track",
                    name,
                )
            )
    unknown = sorted(set(curve_map) - set(names))
    for name in unknown:
        issues.append(
            Issue(
                "INVALID_AUTOMATION_CURVE",
                "curve refers to an unknown track",
                name,
            )
        )

    plans: dict[str, AutomationPlan] = {}
    for name in names:
        if name not in curve_map:
            continue
        nodes, node_issues = _parse_nodes(curve_map[name], name)
        issues.extend(node_issues)
        if nodes is None:
            continue
        track_issues_before = len(issues)
        previous_time: float | None = None
        for index, node in enumerate(nodes):
            if node.time_seconds < 0.0:
                issues.append(
                    Issue(
                        "INVALID_AUTOMATION_CURVE",
                        "node time must be non-negative",
                        name,
                        {"node_index": index, "time_seconds": node.time_seconds},
                    )
                )
            if (
                not math.isfinite(node.gain_db)
                or node.gain_db < config.gain_min_db
                or node.gain_db > config.gain_max_db
            ):
                issues.append(
                    Issue(
                        "INVALID_AUTOMATION_CURVE",
                        "gain is outside the configured legal range",
                        name,
                        {
                            "node_index": index,
                            "gain_db": node.gain_db,
                            "min_db": config.gain_min_db,
                            "max_db": config.gain_max_db,
                        },
                    )
                )
            if previous_time is not None and node.time_seconds < previous_time:
                issues.append(
                    Issue(
                        "INVALID_AUTOMATION_CURVE",
                        "node times must be non-decreasing",
                        name,
                        {"node_index": index},
                    )
                )
            previous_time = node.time_seconds
        if len(issues) == track_issues_before:
            plans[name] = _build_plan(name, nodes, config)

    if issues:
        raise ProjectRejected(issues)
    return plans

