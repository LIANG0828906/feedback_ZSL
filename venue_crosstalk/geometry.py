"""Geometric distance resolution and integer-sample alignment."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .dsp import estimate_arrival_sample
from .errors import E_GEOMETRY_INVALID, VenueError
from .models import GeometryManifest, PairSpec, Point3D


@dataclass(frozen=True)
class DistanceBasis:
    pair_id: str
    source: str
    pickup: str
    kind: str
    distance_m: float
    speaker_position: list[float] | None
    pickup_position: list[float] | None
    speed_of_sound_m_s: float
    delay_seconds: float
    delay_samples: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "pair": self.pair_id,
            "source": self.source,
            "pickup": self.pickup,
            "kind": self.kind,
            "distance_m": self.distance_m,
            "speaker_position": self.speaker_position,
            "pickup_position": self.pickup_position,
            "speed_of_sound_m_s": self.speed_of_sound_m_s,
            "delay_seconds": self.delay_seconds,
            "delay_samples": self.delay_samples,
        }


@dataclass(frozen=True)
class AlignedMeasurement:
    pair_id: str
    reference: np.ndarray
    pickup: np.ndarray
    arrival_samples: int
    alignment_offset_samples: int
    residual_offset_samples: int
    distance: DistanceBasis

    def summary(self) -> dict[str, Any]:
        return {
            "pair": self.pair_id,
            "arrival_samples": self.arrival_samples,
            "alignment_offset_samples": self.alignment_offset_samples,
            "residual_offset_samples": self.residual_offset_samples,
            "distance_basis": self.distance.to_dict(),
        }


def resolve_distance(
    pair: PairSpec,
    geometry: GeometryManifest,
    sample_rate: int,
) -> DistanceBasis:
    speaker: Point3D | None = geometry.speakers.get(pair.source)
    pickup: Point3D | None = geometry.pickups.get(pair.pickup)
    missing: list[dict[str, str]] = []
    if speaker is None:
        missing.append({"kind": "speaker", "id": pair.source})
    if pickup is None:
        missing.append({"kind": "pickup", "id": pair.pickup})
    if missing:
        raise VenueError(
            E_GEOMETRY_INVALID,
            "geometry manifest is missing an entry required by the pair",
            {
                "pair": pair.pair_id,
                "source": pair.source,
                "pickup": pair.pickup,
                "missing": missing,
            },
        )
    if pair.distance_override_m is None:
        assert speaker is not None and pickup is not None
        distance = math.dist(speaker.to_list(), pickup.to_list())
        kind = "euclidean_speaker_to_pickup"
    else:
        distance = float(pair.distance_override_m)
        kind = "pair_distance_override"
        if distance < 0:
            raise VenueError(
                E_GEOMETRY_INVALID,
                "distance must not be negative",
                {
                    "pair": pair.pair_id,
                    "source": pair.source,
                    "pickup": pair.pickup,
                    "distance_m": distance,
                },
            )
    if not math.isfinite(distance):
        raise VenueError(
            E_GEOMETRY_INVALID,
            "distance must be finite",
            {"pair": pair.pair_id, "distance_m": distance},
        )
    delay_seconds = distance / geometry.speed_of_sound_m_s
    delay_samples = int(round(delay_seconds * sample_rate))
    return DistanceBasis(
        pair_id=pair.pair_id,
        source=pair.source,
        pickup=pair.pickup,
        kind=kind,
        distance_m=distance,
        speaker_position=None if speaker is None else speaker.to_list(),
        pickup_position=None if pickup is None else pickup.to_list(),
        speed_of_sound_m_s=geometry.speed_of_sound_m_s,
        delay_seconds=delay_seconds,
        delay_samples=delay_samples,
    )


def shift_integer_samples(signal: np.ndarray, delay_samples: int) -> np.ndarray:
    """Shift a signal later by a positive delay without circular wraparound."""

    values = np.asarray(signal, dtype=np.float64)
    if delay_samples == 0:
        return values.copy()
    shifted = np.zeros_like(values)
    if abs(delay_samples) >= values.size:
        return shifted
    if delay_samples > 0:
        shifted[delay_samples:] = values[:-delay_samples]
    else:
        shifted[:delay_samples] = values[-delay_samples:]
    return shifted


def align_measurement(
    pair: PairSpec,
    geometry: GeometryManifest,
    reference: np.ndarray,
    pickup: np.ndarray,
    sample_rate: int,
) -> AlignedMeasurement:
    if reference.size != pickup.size:
        raise VenueError(
            "E_MEASUREMENT_INVALID",
            "reference and pickup channels must have equal sample counts",
            {
                "pair": pair.pair_id,
                "reference_frames": int(reference.size),
                "pickup_frames": int(pickup.size),
            },
        )
    distance = resolve_distance(pair, geometry, sample_rate)
    arrival = estimate_arrival_sample(reference, pickup)
    tap = distance.delay_samples
    aligned_reference = reference[: max(0, reference.size - tap)]
    aligned_pickup = pickup[tap:] if tap > 0 else pickup[:]
    if tap < 0:
        aligned_pickup = np.concatenate([np.zeros(-tap), pickup])
        aligned_reference = reference[: aligned_pickup.size]
    length = min(aligned_reference.size, aligned_pickup.size)
    aligned_reference = aligned_reference[:length]
    aligned_pickup = aligned_pickup[:length]
    return AlignedMeasurement(
        pair_id=pair.pair_id,
        reference=aligned_reference,
        pickup=aligned_pickup,
        arrival_samples=arrival,
        alignment_offset_samples=-tap,
        residual_offset_samples=arrival - tap,
        distance=distance,
    )


def apply_geometry_alignment(
    pair: PairSpec,
    geometry: GeometryManifest,
    reference: np.ndarray,
    pickup: np.ndarray,
    sample_rate: int,
) -> AlignedMeasurement:
    """Public requirement-oriented alias for :func:`align_measurement`."""

    return align_measurement(pair, geometry, reference, pickup, sample_rate)
