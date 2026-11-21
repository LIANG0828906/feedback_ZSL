from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .geometry import blocked_radiance, first_hit, occlusion_digest, visibility_fraction
from .scene import Scene
from .sh import (
    direct_irradiance,
    luminance,
    project_radiance,
    radiance_to_irradiance,
)


@dataclass(frozen=True)
class SampledProbe:
    radiance_coefficients: np.ndarray
    irradiance_coefficients: np.ndarray
    truncation_error: float
    first_hit_ids: tuple[int, ...]
    first_hit_distances: tuple[float, ...]
    raw_mean_radiance: tuple[float, float, float]
    visibility_fraction: float
    occlusion_digest: str


@dataclass(frozen=True)
class AuditProbe:
    visibility_fraction: float
    mean_radiance: tuple[float, float, float]
    sky_mean: tuple[float, float, float]
    first_hit_ids: tuple[int, ...]
    first_hit_distances: tuple[float, ...]
    occlusion_digest: str


def sample_radiance_batch(
    scene: Scene,
    origins: np.ndarray,
    directions: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Sample unoccluded sky or first-hit indirect radiance for each probe/direction."""
    origins = np.asarray(origins, dtype=np.float64)
    directions = np.asarray(directions, dtype=np.float64)
    hit_ids, hit_distances = first_hit(scene, origins, directions)
    sky = scene.sky.radiance(directions)
    blocked = blocked_radiance(scene, hit_ids)
    visible = hit_ids < 0
    samples = np.where(visible[:, :, None], sky[None, :, :], blocked)
    return samples, hit_ids, hit_distances


def sample_probe(
    scene: Scene,
    origin: np.ndarray,
    directions: np.ndarray,
    order: int,
) -> SampledProbe:
    origins = np.asarray(origin, dtype=np.float64)[None, :]
    samples, hit_ids, hit_distances = sample_radiance_batch(scene, origins, directions)
    first_hit_ids = tuple(int(item) for item in hit_ids[0])
    distances = tuple(float(item) for item in hit_distances[0])
    coefficients, _, truncation_error = project_radiance(
        directions, samples[0], order
    )
    irradiance = radiance_to_irradiance(coefficients, order)
    raw_mean = tuple(float(item) for item in np.mean(samples[0], axis=0))
    coefficients.setflags(write=False)
    irradiance.setflags(write=False)
    return SampledProbe(
        radiance_coefficients=coefficients,
        irradiance_coefficients=irradiance,
        truncation_error=truncation_error,
        first_hit_ids=first_hit_ids,
        first_hit_distances=distances,
        raw_mean_radiance=raw_mean,  # type: ignore[arg-type]
        visibility_fraction=float(visibility_fraction(hit_ids[0])),
        occlusion_digest=occlusion_digest(hit_ids[0], hit_distances[0]),
    )


def audit_probe(
    scene: Scene,
    origin: np.ndarray,
    audit_directions: np.ndarray,
) -> AuditProbe:
    origins = np.asarray(origin, dtype=np.float64)[None, :]
    samples, hit_ids, hit_distances = sample_radiance_batch(
        scene, origins, audit_directions
    )
    mean_radiance = np.mean(samples[0], axis=0)
    sky_mean = np.mean(scene.sky.radiance(audit_directions), axis=0)
    return AuditProbe(
        visibility_fraction=float(visibility_fraction(hit_ids[0])),
        mean_radiance=tuple(float(item) for item in mean_radiance),  # type: ignore[arg-type]
        sky_mean=tuple(float(item) for item in sky_mean),  # type: ignore[arg-type]
        first_hit_ids=tuple(int(item) for item in hit_ids[0]),
        first_hit_distances=tuple(float(item) for item in hit_distances[0]),
        occlusion_digest=occlusion_digest(hit_ids[0], hit_distances[0]),
    )


def direct_truth_irradiance(
    scene: Scene,
    origin: np.ndarray,
    normals: np.ndarray,
    truth_directions: np.ndarray,
) -> np.ndarray:
    samples, _, _ = sample_radiance_batch(
        scene, np.asarray(origin)[None, :], truth_directions
    )
    return direct_irradiance(truth_directions, samples[0], normals)


def leak_score_from_audit(
    raw_mean_radiance: tuple[float, float, float],
    audit_mean_radiance: tuple[float, float, float],
    visibility: float,
    audit_sky_mean: tuple[float, float, float],
) -> float:
    raw_luma = float(luminance(np.asarray(raw_mean_radiance)))
    audit_luma = float(luminance(np.asarray(audit_mean_radiance)))
    sky_luma = max(float(luminance(np.asarray(audit_sky_mean))), 1.0e-9)
    deficit = max(0.0, 1.0 - visibility)
    excess = max(0.0, raw_luma - audit_luma) / sky_luma
    return float(np.clip(4.0 * deficit * excess, 0.0, 1.0))

