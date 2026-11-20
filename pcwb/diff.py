"""Point-cloud version difference computation and highlight indices."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .cloud import PointCloud


@dataclass(frozen=True)
class CloudDiff:
    """Result of comparing two committed cloud versions.

    Point identity uses coordinates quantized onto a fixed grid so matching is
    independent of index order. ``added`` / ``deleted`` hold indices into the
    new / old cloud respectively; ``color_changed`` holds paired indices into
    both clouds for points sharing a coordinate whose RGB bytes differ.
    """

    old_version: int
    new_version: int
    added: np.ndarray
    deleted: np.ndarray
    color_changed_old: np.ndarray
    color_changed_new: np.ndarray

    @property
    def counts(self) -> dict:
        return {
            "added": int(self.added.shape[0]),
            "deleted": int(self.deleted.shape[0]),
            "color_changed": int(self.color_changed_new.shape[0]),
        }


def _point_keys(xyz: np.ndarray, grid: float) -> np.ndarray:
    """Canonical integer identity key for every point."""
    scaled = np.rint(np.asarray(xyz, dtype=np.float64) / float(grid))
    scaled = np.ascontiguousarray(scaled.astype(np.int64))
    return scaled


def diff_clouds(old: PointCloud, new: PointCloud,
                grid: float = 1e-6) -> CloudDiff:
    """Compare two versions and classify every point-level change."""
    if grid <= 0:
        raise ValueError("diff grid must be positive")
    old_keys = _point_keys(old.xyz, grid)
    new_keys = _point_keys(new.xyz, grid)

    # First occurrence index per key (input order is already canonical).
    old_first: dict[tuple, int] = {}
    for i, key in enumerate(old_keys):
        t = tuple(key.tolist())
        if t not in old_first:
            old_first[t] = i
    new_first: dict[tuple, int] = {}
    for i, key in enumerate(new_keys):
        t = tuple(key.tolist())
        if t not in new_first:
            new_first[t] = i

    old_key_set = set(old_first)
    new_key_set = set(new_first)

    added = np.array(sorted(new_first[k] for k in new_key_set - old_key_set),
                     dtype=np.int64)
    deleted = np.array(
        sorted(old_first[k] for k in old_key_set - new_key_set),
        dtype=np.int64)

    changed_old: list[int] = []
    changed_new: list[int] = []
    for key in sorted(old_key_set & new_key_set):
        oi, ni = old_first[key], new_first[key]
        if not np.array_equal(old.rgb[oi], new.rgb[ni]):
            changed_old.append(oi)
            changed_new.append(ni)

    return CloudDiff(
        old_version=old.version,
        new_version=new.version,
        added=added,
        deleted=deleted,
        color_changed_old=np.array(changed_old, dtype=np.int64),
        color_changed_new=np.array(changed_new, dtype=np.int64),
    )
