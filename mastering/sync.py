from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import Issue


@dataclass(frozen=True)
class SyncDetection:
    track: str
    detected: bool
    detected_source_sample: int | None
    mapped_output_sample: int | None
    target_output_sample: int | None
    applied_offset_samples: int
    aligned: bool
    alignment_residual_samples: float | None
    warning: Issue | None

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "detected": self.detected,
            "aligned": self.aligned,
            "detected_source_sample": self.detected_source_sample,
            "mapped_output_sample": self.mapped_output_sample,
            "target_output_sample": self.target_output_sample,
            "applied_offset_samples": self.applied_offset_samples,
            "alignment_residual_samples": (
                None
                if self.alignment_residual_samples is None
                else round(self.alignment_residual_samples, 12)
            ),
        }
        if self.warning is not None:
            result["warning"] = self.warning.as_dict()
        return result


def detect_sync_pulse(
    samples: np.ndarray,
    sample_rate: int,
    spec: Any,
    track: str,
) -> tuple[int | None, Issue | None, dict[str, Any]]:
    """Detect the first threshold crossing of a deterministic sync transient."""

    spec = spec if isinstance(spec, dict) else {}
    threshold_ratio = spec.get("threshold_ratio", 0.20)
    min_peak = spec.get("min_peak", 1e-5)
    if not isinstance(threshold_ratio, (int, float)) or not 0.0 < threshold_ratio <= 1.0:
        threshold_ratio = 0.20
    if not isinstance(min_peak, (int, float)) or min_peak < 0.0:
        min_peak = 1e-5

    if "search_start_sample" in spec:
        start = int(spec["search_start_sample"])
    else:
        start = int(round(float(spec.get("search_start_seconds", 0.0)) * sample_rate))
    if "search_end_sample" in spec:
        end = int(spec["search_end_sample"])
    else:
        end = int(round(float(spec.get("search_end_seconds", 0.1)) * sample_rate))
    start = max(0, min(start, samples.shape[0]))
    end = max(start, min(end, samples.shape[0]))
    window = samples[start:end]
    if window.size == 0:
        warning = Issue(
            "DETECTION_FAILED",
            "sync search window is empty",
            track,
            {"search_start": start, "search_end": end},
        )
        return None, warning, {"peak": 0.0, "threshold": 0.0}

    envelope = np.max(np.abs(window), axis=1)
    peak = float(np.max(envelope))
    if not math.isfinite(peak) or peak < float(min_peak):
        warning = Issue(
            "DETECTION_FAILED",
            "sync pulse peak is below the detection floor",
            track,
            {"peak": peak, "min_peak": float(min_peak)},
        )
        return None, warning, {"peak": peak, "threshold": 0.0}
    threshold = peak * float(threshold_ratio)
    candidates = np.flatnonzero(envelope >= threshold)
    if candidates.size == 0:
        warning = Issue(
            "DETECTION_FAILED",
            "sync threshold was not crossed",
            track,
            {"peak": peak, "threshold": threshold},
        )
        return None, warning, {"peak": peak, "threshold": threshold}
    onset = start + int(candidates[0])
    return onset, None, {"peak": peak, "threshold": threshold}


def plan_sync_alignment(
    track: str,
    detected_source_sample: int | None,
    drift_map: Any,
    output_sample_rate: int,
    target_time_seconds: float,
    warning: Issue | None,
) -> SyncDetection:
    target_frame = int(round(float(target_time_seconds) * output_sample_rate))
    if detected_source_sample is None:
        return SyncDetection(
            track=track,
            detected=False,
            detected_source_sample=None,
            mapped_output_sample=None,
            target_output_sample=target_frame,
            applied_offset_samples=0,
            aligned=False,
            alignment_residual_samples=None,
            warning=warning,
        )
    mapped_time = float(drift_map.source_to_target(float(detected_source_sample)))
    mapped_frame = int(round(mapped_time * output_sample_rate))
    offset = target_frame - mapped_frame
    residual = abs((mapped_frame + offset) - target_frame)
    return SyncDetection(
        track=track,
        detected=True,
        detected_source_sample=int(detected_source_sample),
        mapped_output_sample=mapped_frame,
        target_output_sample=target_frame,
        applied_offset_samples=int(offset),
        aligned=residual <= 1,
        alignment_residual_samples=float(residual),
        warning=None,
    )

