"""Immutable point-cloud snapshot shared between render and edit threads."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .format import compute_bbox


@dataclass(frozen=True)
class PointCloud:
    """An immutable snapshot of a complete point-cloud version.

    ``xyz`` is float64 (canonical arithmetic precision), ``rgb`` is uint8.
    The arrays are treated as read-only; operators always build a new
    PointCloud instead of mutating one, so render threads can hold references
    without locks and never observe a partially updated cloud.
    """

    xyz: np.ndarray
    rgb: np.ndarray
    bbox: np.ndarray
    sha256: str
    version: int

    def __post_init__(self) -> None:
        if self.xyz.shape != self.rgb.shape or self.xyz.ndim != 2 \
                or self.xyz.shape[1] != 3:
            raise ValueError(f"invalid cloud shapes xyz={self.xyz.shape} "
                             f"rgb={self.rgb.shape}")
        self.xyz.setflags(write=False)
        self.rgb.setflags(write=False)
        self.bbox.setflags(write=False)

    @property
    def count(self) -> int:
        return int(self.xyz.shape[0])

    @classmethod
    def from_arrays(cls, xyz: np.ndarray, rgb: np.ndarray,
                    sha256: str, version: int,
                    bbox: np.ndarray | None = None) -> "PointCloud":
        xyz = np.array(xyz, dtype=np.float64, copy=True)
        rgb = np.array(rgb, dtype=np.uint8, copy=True)
        if bbox is None:
            bbox = compute_bbox(xyz)
        return cls(xyz, rgb,
                   np.array(bbox, dtype=np.float64, copy=True),
                   sha256, int(version))

    def replace(self, xyz: np.ndarray, rgb: np.ndarray,
                sha256: str, version: int) -> "PointCloud":
        return PointCloud.from_arrays(xyz, rgb, sha256, version)
