from __future__ import annotations

import math
from typing import Dict, Optional

import numpy as np
from scipy import signal

from .errors import E_PAIR_MISSING, BusinessError
from .geometry import GeometryManifest
from .measurement_io import (
    MeasurementManifest,
    PairMeasurement,
    component_content_hash,
    read_component,
    read_stimulus,
)
from .models import AlignmentResult
from .util import content_hash, optional_cancel


def estimate_arrival_samples(
    reference: np.ndarray,
    component: np.ndarray,
    max_lag: int,
) -> int:
    x = np.asarray(reference, dtype=np.float64)
    y = np.asarray(component, dtype=np.float64)
    x = x - float(np.mean(x))
    y = y - float(np.mean(y))
    correlation = signal.correlate(y, x, mode="full", method="fft")
    lags = np.arange(-len(x) + 1, len(y), dtype=np.int64)
    limit = min(int(max_lag), len(y) - 1)
    mask = (lags >= 0) & (lags <= limit)
    if not np.any(mask):
        return 0
    candidate_lags = lags[mask]
    candidate_values = np.abs(correlation[mask])
    peak = int(np.argmax(candidate_values))
    return int(candidate_lags[peak])


def _shift_to_reference(component: np.ndarray, delay: int) -> np.ndarray:
    if delay <= 0:
        return np.array(component, copy=True)
    shifted = np.zeros_like(component)
    if delay < component.size:
        shifted[: component.size - delay] = component[delay:]
    return shifted


def align_pair(
    manifest: MeasurementManifest,
    geometry: GeometryManifest,
    pair: PairMeasurement,
    reference: np.ndarray,
    main_receiver: str,
    cancel_event: Optional[object] = None,
) -> AlignmentResult:
    optional_cancel(cancel_event)
    pair_geometry = geometry.pair(pair.source, pair.receiver)
    component = read_component(manifest, pair)
    if component.size != reference.size:
        raise BusinessError(
            E_PAIR_MISSING,
            "component and reference lengths differ",
            source=pair.source,
            receiver=pair.receiver,
            details={
                "component_samples": int(component.size),
                "reference_samples": int(reference.size),
            },
        )
    component_rms = float(np.sqrt(np.mean(np.square(component))))
    source_rms = float(np.sqrt(np.mean(np.square(reference))))
    if component_rms <= 1.0 / 32768.0 or not np.isfinite(component_rms):
        raise BusinessError(
            E_PAIR_MISSING,
            "component has no valid signal across the full capture",
            source=pair.source,
            receiver=pair.receiver,
            details={
                "component_rms": component_rms,
                "path": pair.path_label,
                "channel": pair.channel,
            },
        )
    geometry_delay = int(
        round(pair_geometry.distance_m / geometry.speed_of_sound_m_s * manifest.sample_rate)
    )
    max_lag = min(
        component.size - 1,
        max(256, geometry_delay * 2 + 256),
    )
    arrival = estimate_arrival_samples(reference, component, max_lag)
    residual = arrival - geometry_delay
    if pair_geometry.position_distance_m is None:
        position_confidence = 0.72
    else:
        position_delta = abs(pair_geometry.position_distance_m - pair_geometry.distance_m)
        position_confidence = max(0.0, 1.0 - position_delta / 0.05)
    lag_confidence = max(0.0, 1.0 - abs(residual) / 8.0)
    confidence_score = round(min(position_confidence, lag_confidence), 6)
    if confidence_score >= 0.85:
        confidence = "high"
    elif confidence_score >= 0.55:
        confidence = "medium"
    else:
        confidence = "low"
    aligned_component = _shift_to_reference(component, geometry_delay)
    valid_samples = max(1, component.size - geometry_delay)
    basis = (
        f"{pair_geometry.basis}; delay_samples=round({pair_geometry.distance_m:.9f}"
        f"/{geometry.speed_of_sound_m_s:.6f}*{manifest.sample_rate})={geometry_delay}"
    )
    return AlignmentResult(
        source=pair.source,
        receiver=pair.receiver,
        main_receiver=main_receiver,
        path_label=pair.path_label,
        channel=pair.channel,
        layout=pair.layout,
        sample_rate=manifest.sample_rate,
        component_hash=component_content_hash(manifest, pair),
        arrival_samples=arrival,
        geometry_delay_samples=geometry_delay,
        alignment_offset_samples=-geometry_delay,
        distance_m=pair_geometry.distance_m,
        speed_of_sound_m_s=geometry.speed_of_sound_m_s,
        position_distance_m=pair_geometry.position_distance_m,
        distance_delta_m=pair_geometry.distance_delta_m,
        distance_basis=basis,
        confidence=confidence,
        confidence_score=confidence_score,
        measured_minus_geometry_samples=residual,
        valid_samples=valid_samples,
        source_rms=source_rms,
        component_rms=component_rms,
        aligned_component=aligned_component,
        aligned_reference=np.array(reference, copy=True),
    )


def align_all(
    manifest: MeasurementManifest,
    geometry: GeometryManifest,
    cancel_event: Optional[object] = None,
) -> tuple[Dict[str, AlignmentResult], Dict[str, float]]:
    references = {}
    for source in manifest.speakers:
        optional_cancel(cancel_event)
        reference = read_stimulus(manifest, source)
        if float(np.sqrt(np.mean(np.square(reference)))) <= 1.0 / 32768.0:
            raise BusinessError(
                E_PAIR_MISSING,
                "source stimulus has no valid signal",
                source=source,
                details={"stimulus": str(manifest.stimuli[source].name)},
            )
        references[source] = reference
    results: Dict[str, AlignmentResult] = {}
    scores: Dict[str, float] = {}
    for pair in manifest.pairs:
        optional_cancel(cancel_event)
        result = align_pair(
            manifest,
            geometry,
            pair,
            references[pair.source],
            manifest.main_receivers[pair.source],
            cancel_event=cancel_event,
        )
        results[result.pair_key] = result
        scores[result.pair_key] = result.confidence_score
    geometry_alignment_hash = content_hash(
        {
            "geometry": geometry.digest,
            "manifest": manifest.digest,
            "pairs": {
                key: {
                    "arrival": result.arrival_samples,
                    "geometry_delay": result.geometry_delay_samples,
                    "offset": result.alignment_offset_samples,
                    "component_hash": result.component_hash,
                    "confidence": result.confidence,
                }
                for key, result in sorted(results.items())
            },
        }
    )
    scores["__alignment_hash__"] = geometry_alignment_hash
    return results, scores
