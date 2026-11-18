"""Deterministic water particle and absorption-modulation tables."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .canonical import canonical_bytes, sha256_bytes
from .prng import StableRandom

PARTICLE_TABLE_VERSION = 1


@dataclass
class ParticleTable:
    component_id: str
    particle_seed: int
    resolution: tuple[int, int]
    u: np.ndarray
    v: np.ndarray
    radial_fraction: np.ndarray
    size: np.ndarray
    intensity: np.ndarray
    modulation: np.ndarray
    payload_bytes: bytes
    sha256: str

    def to_dict(self) -> dict[str, Any]:
        value = {
            "table_version": PARTICLE_TABLE_VERSION,
            "component_id": self.component_id,
            "particle_seed": self.particle_seed,
            "resolution": [self.resolution[0], self.resolution[1]],
            "particles": [
                {
                    "u": round(float(self.u[i]), 8),
                    "v": round(float(self.v[i]), 8),
                    "radial_fraction": round(float(self.radial_fraction[i]), 8),
                    "size": round(float(self.size[i]), 8),
                    "intensity": round(float(self.intensity[i]), 8),
                }
                for i in range(self.u.size)
            ],
            "absorption_modulation": [
                [round(float(value), 8) for value in row] for row in self.modulation
            ],
        }
        value["sha256"] = self.sha256
        return value

    def content_dict(self) -> dict[str, Any]:
        value = self.to_dict()
        value.pop("sha256")
        return value


def build_particle_table(
    particle_seed: int,
    component_id: str,
    resolution: tuple[int, int],
    particle_count: int | None = None,
) -> ParticleTable:
    """Build a table using only the seed, component id and grid resolution.

    The PRNG loop is over a fixed one-dimensional particle index. It never sees
    mesh triangles and therefore cannot depend on triangle traversal order.
    When ``particle_count`` is omitted it is derived solely from the seeded
    resolution, so a layer table can be recomputed from seed, id and resolution.
    """

    width, height = resolution
    if width < 2 or height < 2:
        raise ValueError("particle resolution must be at least 2x2")
    if particle_count is None:
        particle_count = max(1, (width * height) // 8)
    if particle_count < 0:
        raise ValueError("particle_count must be non-negative")

    rng = StableRandom(particle_seed, component_id, "particles")
    u = np.empty(particle_count, dtype=np.float64)
    v = np.empty(particle_count, dtype=np.float64)
    radial = np.empty(particle_count, dtype=np.float64)
    size = np.empty(particle_count, dtype=np.float64)
    intensity = np.empty(particle_count, dtype=np.float64)
    for index in range(particle_count):
        u[index] = rng.random()
        v[index] = rng.random()
        radial[index] = rng.uniform(0.18, 0.82)
        size[index] = rng.uniform(0.006, 0.019)
        intensity[index] = rng.uniform(0.35, 1.0)

    modulation = np.zeros((height, width), dtype=np.float64)
    half_w = max(width - 1.0, 1.0)
    half_h = max(height - 1.0, 1.0)
    for index in range(particle_count):
        px = float(u[index]) * half_w
        py = float(v[index]) * half_h
        sigma = max(float(size[index]) * max(width, height) * 1.4, 0.65)
        x0 = max(int(math.floor(px - 3.0 * sigma)), 0)
        x1 = min(int(math.ceil(px + 3.0 * sigma)), width - 1)
        y0 = max(int(math.floor(py - 3.0 * sigma)), 0)
        y1 = min(int(math.ceil(py + 3.0 * sigma)), height - 1)
        yy, xx = np.mgrid[y0 : y1 + 1, x0 : x1 + 1]
        weight = np.exp(-((xx - px) ** 2 + (yy - py) ** 2) / (2.0 * sigma * sigma))
        modulation[y0 : y1 + 1, x0 : x1 + 1] += weight * float(intensity[index])
    if particle_count:
        modulation /= max(float(modulation.max()), 1e-12)
    modulation = np.clip(modulation, 0.0, 1.0)
    modulation.setflags(write=False)

    table = ParticleTable(
        component_id=component_id,
        particle_seed=particle_seed,
        resolution=resolution,
        u=u,
        v=v,
        radial_fraction=radial,
        size=size,
        intensity=intensity,
        modulation=modulation,
        payload_bytes=b"",
        sha256="",
    )
    payload = canonical_bytes(table.content_dict())
    digest = sha256_bytes(payload)
    table.payload_bytes = payload
    table.sha256 = digest
    return table


def sample_modulation(table: ParticleTable, u: np.ndarray, v: np.ndarray) -> np.ndarray:
    """Bilinear, wrap-free sampling in the table's normalized domain."""

    width, height = table.resolution
    x = np.clip(u, 0.0, 1.0) * (width - 1)
    y = np.clip(v, 0.0, 1.0) * (height - 1)
    x0 = np.floor(x).astype(np.int64)
    y0 = np.floor(y).astype(np.int64)
    x1 = np.minimum(x0 + 1, width - 1)
    y1 = np.minimum(y0 + 1, height - 1)
    fx = x - x0
    fy = y - y0
    top = table.modulation[y0, x0] * (1.0 - fx) + table.modulation[y0, x1] * fx
    bottom = table.modulation[y1, x0] * (1.0 - fx) + table.modulation[y1, x1] * fx
    return top * (1.0 - fy) + bottom * fy


def particle_tables_fingerprint(tables: dict[str, ParticleTable]) -> str:
    payload = {
        component_id: {
            "seed": table.particle_seed,
            "resolution": list(table.resolution),
            "sha256": table.sha256,
        }
        for component_id, table in sorted(tables.items())
    }
    return sha256_bytes(canonical_bytes(payload))

