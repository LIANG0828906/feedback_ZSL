from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from .errors import sh_invalid
from .util import sha256_json

GOLDEN_ANGLE = math.pi * (3.0 - math.sqrt(5.0))


@dataclass(frozen=True)
class DirectionSet:
    vectors: np.ndarray
    seed: int
    kind: str = "fibonacci"

    @property
    def count(self) -> int:
        return int(self.vectors.shape[0])

    @property
    def fingerprint(self) -> str:
        return sha256_json(
            {
                "kind": self.kind,
                "seed": self.seed,
                "count": self.count,
                "vectors": np.round(self.vectors, 12).tolist(),
            }
        )


def make_directions(count: int, seed: int = 0) -> DirectionSet:
    if isinstance(count, bool) or not isinstance(count, int):
        raise sh_invalid("directions.count", "direction count must be an integer")
    if count <= 0:
        raise sh_invalid("directions", "direction set must not be empty")
    if isinstance(seed, bool) or not isinstance(seed, int):
        raise sh_invalid("directions.seed", "direction seed must be an integer")
    index = np.arange(count, dtype=np.float64) + 0.5
    elevation = 1.0 - 2.0 * index / count
    radial = np.sqrt(np.maximum(0.0, 1.0 - elevation * elevation))
    yaw_offset = ((seed * 0.6180339887498949) % 1.0) * 2.0 * math.pi
    phase = index * GOLDEN_ANGLE + yaw_offset
    vectors = np.stack(
        [radial * np.cos(phase), elevation, radial * np.sin(phase)],
        axis=1,
    )
    vectors /= np.linalg.norm(vectors, axis=1, keepdims=True)
    vectors.setflags(write=False)
    return DirectionSet(vectors=vectors, seed=seed)


def validate_direction_array(directions: np.ndarray) -> np.ndarray:
    array = np.asarray(directions, dtype=np.float64)
    if array.ndim != 2 or array.shape[1] != 3:
        raise sh_invalid("directions", "directions must have shape (N, 3)")
    if array.shape[0] == 0:
        raise sh_invalid("directions", "direction set must not be empty")
    if not np.all(np.isfinite(array)):
        raise sh_invalid("directions", "direction vectors must be finite")
    lengths = np.linalg.norm(array, axis=1)
    if np.any(lengths <= 1.0e-12):
        raise sh_invalid("directions", "direction vectors must be non-zero")
    return array / lengths[:, None]


def validation_directions(count: int, seed: int) -> DirectionSet:
    return make_directions(count, seed + 104729)

