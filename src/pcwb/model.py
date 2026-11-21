"""Immutable point-cloud value objects."""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass, field
from typing import Any

import numpy as np

from .errors import PointCloudInvalid


def _float_tuple(values: Iterable[float]) -> tuple[float, float, float]:
    array = np.asarray(tuple(values), dtype=np.float64)
    if array.shape != (3,):
        raise PointCloudInvalid("a 3D vector is required")
    if not np.isfinite(array).all():
        raise PointCloudInvalid("coordinates must be finite")
    return tuple(float(v) for v in array)


@dataclass(frozen=True, slots=True)
class Bounds:
    minimum: tuple[float, float, float]
    maximum: tuple[float, float, float]

    def __post_init__(self) -> None:
        lo = _float_tuple(self.minimum)
        hi = _float_tuple(self.maximum)
        if any(a > b for a, b in zip(lo, hi)):
            raise PointCloudInvalid("bounding-box minimum exceeds maximum")
        object.__setattr__(self, "minimum", lo)
        object.__setattr__(self, "maximum", hi)

    @classmethod
    def from_points(cls, xyz: np.ndarray) -> Bounds:
        if xyz.size == 0:
            return cls((0.0, 0.0, 0.0), (0.0, 0.0, 0.0))
        values = np.asarray(xyz, dtype=np.float64)
        return cls(tuple(values.min(axis=0)), tuple(values.max(axis=0)))

    @property
    def size(self) -> tuple[float, float, float]:
        return tuple(b - a for a, b in zip(self.minimum, self.maximum))

    @property
    def center(self) -> tuple[float, float, float]:
        return tuple((a + b) * 0.5 for a, b in zip(self.minimum, self.maximum))

    @property
    def diagonal(self) -> float:
        return float(np.linalg.norm(np.asarray(self.size, dtype=np.float64)))

    def contains(self, points: np.ndarray, *, epsilon: float = 0.0) -> np.ndarray:
        values = np.asarray(points)
        lo = np.asarray(self.minimum, dtype=values.dtype) - epsilon
        hi = np.asarray(self.maximum, dtype=values.dtype) + epsilon
        return np.all((values >= lo) & (values <= hi), axis=1)

    def to_dict(self) -> dict[str, list[float]]:
        return {"min": list(self.minimum), "max": list(self.maximum)}

    @classmethod
    def union(cls, bounds: Iterable[Bounds]) -> Bounds:
        items = list(bounds)
        if not items:
            return cls((0.0, 0.0, 0.0), (0.0, 0.0, 0.0))
        return cls(
            tuple(min(b.minimum[i] for b in items) for i in range(3)),
            tuple(max(b.maximum[i] for b in items) for i in range(3)),
        )


def _readonly(array: np.ndarray) -> np.ndarray:
    array.setflags(write=False)
    return array


@dataclass(frozen=True, slots=True)
class PointCloud:
    """An immutable, contiguous point-cloud snapshot.

    ``bounds`` is the bounds persisted in the file header.  New clouds compute
    it from their points.  Keeping it explicit also permits validation of
    externally authored files whose header bounds are larger than tight bounds.
    """

    xyz: np.ndarray
    rgb: np.ndarray
    bounds: Bounds
    _hash: str | None = field(default=None, init=False, repr=False, compare=False)

    def __post_init__(self) -> None:
        xyz = np.ascontiguousarray(self.xyz, dtype=np.float32)
        rgb = np.ascontiguousarray(self.rgb, dtype=np.uint8)
        if xyz.ndim != 2 or xyz.shape[1] != 3:
            raise PointCloudInvalid("xyz must have shape (N, 3)")
        if rgb.ndim != 2 or rgb.shape[1] != 3:
            raise PointCloudInvalid("rgb must have shape (N, 3)")
        if xyz.shape[0] != rgb.shape[0]:
            raise PointCloudInvalid("xyz and rgb point counts differ")
        if not np.isfinite(xyz).all():
            bad = int(np.flatnonzero(~np.isfinite(xyz).all(axis=1))[0])
            raise PointCloudInvalid("point coordinates must be finite", point_index=bad)
        if not self.bounds.contains(xyz).all():
            bad = int(np.flatnonzero(~self.bounds.contains(xyz))[0])
            raise PointCloudInvalid(
                "point lies outside the bounding box", point_index=bad
            )
        object.__setattr__(self, "xyz", _readonly(xyz))
        object.__setattr__(self, "rgb", _readonly(rgb))

    @classmethod
    def from_arrays(
        cls,
        xyz: np.ndarray,
        rgb: np.ndarray,
        *,
        bounds: Bounds | None = None,
    ) -> PointCloud:
        xyz32 = np.ascontiguousarray(xyz, dtype=np.float32)
        rgb8 = np.ascontiguousarray(rgb, dtype=np.uint8)
        if xyz32.ndim != 2 or xyz32.shape[1] != 3:
            raise PointCloudInvalid("xyz must have shape (N, 3)")
        if rgb8.ndim != 2 or rgb8.shape[1] != 3:
            raise PointCloudInvalid("rgb must have shape (N, 3)")
        if xyz32.shape[0] != rgb8.shape[0]:
            raise PointCloudInvalid("xyz and rgb point counts differ")
        if not np.isfinite(xyz32).all():
            bad = int(np.flatnonzero(~np.isfinite(xyz32).all(axis=1))[0])
            raise PointCloudInvalid("point coordinates must be finite", point_index=bad)
        if bounds is None:
            bounds = Bounds.from_points(xyz32)
        return cls(xyz32, rgb8, bounds)

    @property
    def count(self) -> int:
        return int(self.xyz.shape[0])

    @property
    def content_hash(self) -> str:
        if self._hash is None:
            from .format import serialize_point_cloud
            from .util import sha256_hex

            object.__setattr__(self, "_hash", sha256_hex(serialize_point_cloud(self)))
        assert self._hash is not None
        return self._hash

    def subset(self, indices: Iterable[int] | np.ndarray) -> PointCloud:
        idx = np.asarray(indices, dtype=np.int64)
        if idx.ndim != 1:
            raise PointCloudInvalid("indices must be one-dimensional")
        if idx.size and (idx.min() < 0 or idx.max() >= self.count):
            raise PointCloudInvalid("point index outside cloud")
        return PointCloud.from_arrays(self.xyz[idx], self.rgb[idx])

    def with_xyz(self, xyz: np.ndarray) -> PointCloud:
        return PointCloud.from_arrays(xyz, self.rgb)

    def with_rgb(self, rgb: np.ndarray) -> PointCloud:
        return PointCloud.from_arrays(self.xyz, rgb)


@dataclass(frozen=True, slots=True)
class PointCloudSnapshot:
    version: int
    content_hash: str
    cloud: PointCloud
    source_path: str | None = None
    parent_version: int = 0

    @property
    def count(self) -> int:
        return self.cloud.count

    @property
    def short_hash(self) -> str:
        return self.content_hash[:12]

    def identity(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "content_hash": self.content_hash,
            "parent_version": self.parent_version,
            "point_count": self.count,
        }
