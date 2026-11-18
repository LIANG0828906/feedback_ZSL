from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .manifest import AutomationConfig, AutomationNode


@dataclass(frozen=True)
class SmoothingSegment:
    segment_index: int
    node_time: float
    gain_change_db: float
    start_seconds: float
    duration_seconds: float

    def as_dict(self) -> dict[str, float | int]:
        return {
            "segment_index": self.segment_index,
            "node_time": self.node_time,
            "gain_change_db": self.gain_change_db,
            "applied_smoothing_seconds": self.duration_seconds,
            "smoothed_window_start_seconds": self.start_seconds,
        }


def _unique_last(times: np.ndarray, gains: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    """Keep the last gain for duplicate timestamps, making the curve right-continuous."""
    keep = np.ones(times.size, dtype=bool)
    if times.size > 1:
        keep[:-1] = times[1:] != times[:-1]
    return times[keep], gains[keep]


def base_gain_db(
    time_seconds: np.ndarray,
    nodes: tuple[AutomationNode, ...] | list[AutomationNode],
) -> np.ndarray:
    query = np.asarray(time_seconds, dtype=np.float64)
    if not nodes:
        return np.zeros_like(query, dtype=np.float64)
    times = np.asarray([node.time for node in nodes], dtype=np.float64)
    gains = np.asarray([node.gain_db for node in nodes], dtype=np.float64)
    times, gains = _unique_last(times, gains)
    return np.interp(query, times, gains, left=gains[0], right=gains[-1])


def apply_automation_smoothing(
    time_seconds: np.ndarray,
    nodes: tuple[AutomationNode, ...] | list[AutomationNode],
    config: AutomationConfig,
) -> tuple[np.ndarray, list[SmoothingSegment]]:
    query = np.asarray(time_seconds, dtype=np.float64)
    gain_db = base_gain_db(query, nodes)
    if not nodes or config.smoothing_seconds <= 0 or query.size == 0:
        return gain_db, []

    curve_start = float(nodes[0].time)
    curve_end = float(nodes[-1].time)
    half_window = config.smoothing_seconds / 2.0
    segments: list[SmoothingSegment] = []

    for index in range(len(nodes) - 1):
        left = nodes[index]
        right = nodes[index + 1]
        interval = right.time - left.time
        change_db = right.gain_db - left.gain_db
        if interval >= config.smoothing_seconds or abs(change_db) <= config.jump_threshold_db:
            continue

        midpoint = (left.time + right.time) / 2.0
        window_start = max(curve_start, midpoint - half_window)
        window_end = min(curve_end, midpoint + half_window)
        duration = window_end - window_start
        if duration <= 0:
            continue

        selected = (query >= window_start) & (query <= window_end)
        if np.any(selected):
            start_db = float(base_gain_db(np.asarray([window_start]), nodes)[0])
            end_db = float(base_gain_db(np.asarray([window_end]), nodes)[0])
            phase = (query[selected] - window_start) / duration
            phase = np.clip(phase, 0.0, 1.0)
            smooth_phase = phase * phase * (3.0 - 2.0 * phase)
            gain_db[selected] = start_db + (end_db - start_db) * smooth_phase

        segments.append(
            SmoothingSegment(
                segment_index=index,
                node_time=float(left.time),
                gain_change_db=float(change_db),
                start_seconds=float(window_start),
                duration_seconds=float(duration),
            )
        )

    return gain_db, segments
