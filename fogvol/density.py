"""Deterministic multi-octave stage-fog density generation."""

from __future__ import annotations

from typing import Any

import numpy as np


def _value_noise(
    coords: np.ndarray,
    frequency: float,
    seed: int,
    octave: int,
) -> np.ndarray:
    """Evaluate seeded tricubic-smoothed value noise at every coordinate."""

    rng = np.random.default_rng(np.random.SeedSequence([seed, octave, 0x0F06]))
    phase = rng.random(3)
    lattice_coords = coords * frequency + phase
    base = np.floor(lattice_coords).astype(np.int64)
    frac = lattice_coords - base
    fade = frac * frac * (3.0 - 2.0 * frac)

    grid_shape = tuple(int(v) + 2 for v in np.max(base, axis=(0, 1, 2)))
    lattice = rng.random(grid_shape, dtype=np.float64)

    x0, y0, z0 = base[..., 0], base[..., 1], base[..., 2]
    x1, y1, z1 = x0 + 1, y0 + 1, z0 + 1
    fx, fy, fz = fade[..., 0], fade[..., 1], fade[..., 2]

    c000 = lattice[x0, y0, z0]
    c100 = lattice[x1, y0, z0]
    c010 = lattice[x0, y1, z0]
    c110 = lattice[x1, y1, z0]
    c001 = lattice[x0, y0, z1]
    c101 = lattice[x1, y0, z1]
    c011 = lattice[x0, y1, z1]
    c111 = lattice[x1, y1, z1]

    c00 = c000 * (1.0 - fx) + c100 * fx
    c10 = c010 * (1.0 - fx) + c110 * fx
    c01 = c001 * (1.0 - fx) + c101 * fx
    c11 = c011 * (1.0 - fx) + c111 * fx
    c0 = c00 * (1.0 - fy) + c10 * fy
    c1 = c01 * (1.0 - fy) + c11 * fy
    return c0 * (1.0 - fz) + c1 * fz


def _fbm(
    coords: np.ndarray,
    *,
    seed: int,
    octaves: int,
    base_frequency: float,
    lacunarity: float,
    gain: float,
) -> np.ndarray:
    total = np.zeros(coords.shape[:-1], dtype=np.float64)
    amplitude = 1.0
    normalization = 0.0
    frequency = base_frequency
    for octave in range(octaves):
        total += amplitude * _value_noise(coords, frequency, seed, octave)
        normalization += amplitude
        amplitude *= gain
        frequency *= lacunarity
    return total / normalization


def generate_density(
    *,
    seed: int,
    dimensions: tuple[int, int, int],
    origin: np.ndarray,
    voxel_size: np.ndarray,
    density_range: tuple[float, float],
    noise: dict[str, Any],
) -> np.ndarray:
    """Generate a stable fog volume without stateful or unseeded randomness.

    Every voxel value is a pure function of its world-space coordinate and the
    integer scene seed.  The implementation is vectorized; it never accumulates
    values in voxel traversal order.
    """

    nx, ny, nz = dimensions
    indices = np.indices((nx, ny, nz), dtype=np.float64)
    local_world = np.stack(
        [
            (indices[0] + 0.5) * voxel_size[0],
            (indices[1] + 0.5) * voxel_size[1],
            (indices[2] + 0.5) * voxel_size[2],
        ],
        axis=-1,
    )
    world = np.stack(
        [
            origin[0] + (indices[0] + 0.5) * voxel_size[0],
            origin[1] + (indices[1] + 0.5) * voxel_size[1],
            origin[2] + (indices[2] + 0.5) * voxel_size[2],
        ],
        axis=-1,
    )
    extent_y = max(float(dimensions[1] * voxel_size[1]), 1.0e-9)
    height = np.clip((world[..., 1] - origin[1]) / extent_y, 0.0, 1.0)

    floor_thickness = float(noise.get("floor_thickness", 0.34))
    top_thickness = float(noise.get("top_thickness", 0.17))
    strata_frequency = float(noise.get("strata_frequency", 5.0))

    smoke_coords = np.stack(
        [
            local_world[..., 0],
            local_world[..., 1] * 1.65,
            local_world[..., 2],
        ],
        axis=-1
    )
    detail_coords = np.stack(
        [
            local_world[..., 0] * 1.35,
            local_world[..., 1] * 2.6,
            local_world[..., 2] * 1.35,
        ],
        axis=-1,
    )
    smoke = _fbm(
        smoke_coords,
        seed=seed,
        octaves=int(noise.get("octaves", 5)),
        base_frequency=float(noise.get("base_frequency", 0.105)),
        lacunarity=float(noise.get("lacunarity", 2.03)),
        gain=float(noise.get("gain", 0.53)),
    )
    detail = _fbm(
        detail_coords,
        seed=seed ^ 0x5EED,
        octaves=max(2, int(noise.get("octaves", 5)) - 1),
        base_frequency=float(noise.get("base_frequency", 0.105)) * 1.7,
        lacunarity=float(noise.get("lacunarity", 2.03)),
        gain=float(noise.get("gain", 0.53)),
    )

    floor_envelope = np.exp(-height / max(floor_thickness, 1.0e-6))
    top_envelope = np.exp(-(1.0 - height) / max(top_thickness, 1.0e-6))
    strata = 0.5 + 0.5 * np.cos(
        2.0 * np.pi * (strata_frequency * height + 0.58 * detail)
    )

    rolling_floor = floor_envelope * (0.14 + 0.86 * smoke)
    rolling_floor *= 0.78 + 0.22 * detail
    thin_top = top_envelope * (0.13 + 0.52 * strata) * (0.34 + 0.66 * detail)

    field = 1.22 * rolling_floor + 0.24 * thin_top
    field = np.clip(field, 0.0, 1.0)

    low, high = density_range
    density = low + (high - low) * field
    return np.ascontiguousarray(density, dtype=np.float64)

