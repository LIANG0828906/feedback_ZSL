"""Anchor matching and drift-curve estimation."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .signal import (
    DETECTION_HOP,
    Detection,
    envelope_lag,
    nearest_hit,
    refine_event_offset,
)


@dataclass(frozen=True)
class DriftCurve:
    ticks: list[int]
    offsets: list[int]
    bars: list[int]
    base_hits: list[int]
    aux_hits: list[int]
    correlations: list[float]

    def offsets_at(self, positions: np.ndarray) -> np.ndarray:
        return np.interp(
            np.asarray(positions, dtype=np.float64),
            np.asarray(self.ticks, dtype=np.float64),
            np.asarray(self.offsets, dtype=np.float64),
        )


@dataclass(frozen=True)
class AnchorMatch:
    bar: int
    base_hit: int
    aux_hit: int
    offset: int
    correlation: float


def _base_anchor_hit(
    hits: np.ndarray, bar: int, tolerance: int, used: set[int]
) -> int | None:
    hit = nearest_hit(hits, bar, tolerance)
    if hit is None:
        return None
    if hit in used:
        return None
    return hit


def _predict_offset(matches: list[AnchorMatch], position: int) -> float:
    if not matches:
        return 0.0
    if len(matches) == 1:
        return float(matches[-1].offset)
    take = matches[-8:]
    x = np.asarray([item.base_hit for item in take], dtype=np.float64)
    y = np.asarray([item.offset for item in take], dtype=np.float64)
    x -= x[-1]
    slope, intercept = np.polyfit(x, y, 1)
    # Drum-camera clock drift is slow; clamp protects against a mistuned slope.
    slope = max(-0.08, min(0.08, float(slope)))
    return float(intercept + slope * (position - take[-1].base_hit))


def _candidate_offsets(
    aux_hits: np.ndarray,
    predicted_hit: int,
    tolerance: int,
    used: set[int],
    limit: int = 5,
) -> list[int]:
    if aux_hits.size == 0:
        return []
    left = int(np.searchsorted(aux_hits, predicted_hit - tolerance, side="left"))
    right = int(np.searchsorted(aux_hits, predicted_hit + tolerance, side="right"))
    candidates = [
        int(aux_hits[i]) for i in range(left, right) if int(aux_hits[i]) not in used
    ]
    candidates.sort(key=lambda hit: (abs(hit - predicted_hit), hit))
    return candidates[:limit]


def estimate_curve(
    *,
    bars: list[int],
    base: Detection,
    aux: Detection,
    base_raw: np.ndarray,
    aux_raw: np.ndarray,
    sample_rate: int,
    curve_points: int,
    max_offset_seconds: float,
) -> DriftCurve:
    hop = DETECTION_HOP
    first_bar = bars[0] if bars else 0
    segment_start = max(0, int(round((first_bar - sample_rate) / hop)))
    segment_frames = max(64, int(round(12.0 * sample_rate / hop)))
    global_offset = envelope_lag(
        base.onset,
        aux.onset,
        start_frame=segment_start,
        segment_frames=segment_frames,
        max_lag_frames=int(round(max_offset_seconds * sample_rate / hop)),
        hop=hop,
    )

    matched: list[AnchorMatch] = []
    used_base: set[int] = set()
    used_aux: set[int] = set()
    anchor_tolerance = max(1, int(round(0.30 * sample_rate)))

    for bar in bars:
        base_hit = _base_anchor_hit(base.hits, bar, anchor_tolerance, used_base)
        if base_hit is None:
            continue
        predicted_offset = (
            float(global_offset) if not matched else _predict_offset(matched, base_hit)
        )
        predicted_hit = int(round(base_hit + predicted_offset))
        broad = max(1, int(round(0.45 * sample_rate))) if not matched else max(
            1, int(round(0.24 * sample_rate))
        )
        candidates = _candidate_offsets(
            aux.hits, predicted_hit, broad, used_aux, limit=6
        )
        if not candidates:
            continue

        best: AnchorMatch | None = None
        for aux_hit in candidates:
            offset, correlation = refine_event_offset(
                base_raw, aux_raw, base_hit, aux_hit
            )
            # Prefer waveform agreement, then proximity to the prediction.
            score = correlation - 0.15 * (
                abs(aux_hit - predicted_hit) / max(1.0, float(broad))
            )
            if best is None:
                best = AnchorMatch(
                    bar=bar,
                    base_hit=base_hit,
                    aux_hit=aux_hit,
                    offset=int(offset),
                    correlation=float(correlation),
                )
                best_score = score
            elif score > best_score:
                best = AnchorMatch(
                    bar=bar,
                    base_hit=base_hit,
                    aux_hit=aux_hit,
                    offset=int(offset),
                    correlation=float(correlation),
                )
                best_score = score

        if best is None:
            continue
        used_base.add(best.base_hit)
        used_aux.add(best.aux_hit)
        matched.append(best)

    if not matched:
        # A single synthetic case can have no detector peak exactly near the
        # anchors. Preserve the global lag as a constant, auditable curve.
        tick = int(bars[0]) if bars else 0
        return DriftCurve(
            ticks=[tick],
            offsets=[int(global_offset)],
            bars=[tick],
            base_hits=[tick],
            aux_hits=[tick + int(global_offset)],
            correlations=[0.0],
        )

    count = min(max(1, curve_points), len(matched))
    if count == 1:
        indices = [0]
    else:
        indices = sorted(
            {
                int(round(value))
                for value in np.linspace(0, len(matched) - 1, count)
            }
        )
    selected = [matched[index] for index in indices]
    return DriftCurve(
        ticks=[item.bar for item in selected],
        offsets=[int(item.offset) for item in selected],
        bars=[int(item.bar) for item in selected],
        base_hits=[int(item.base_hit) for item in selected],
        aux_hits=[int(item.aux_hit) for item in selected],
        correlations=[round(float(item.correlation), 6) for item in selected],
    )
