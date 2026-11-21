"""Exact point-level version differences."""

from __future__ import annotations

from collections import Counter
from dataclasses import dataclass
from typing import Any

import numpy as np

from .model import Bounds, PointCloud


def _record_key(xyz: np.ndarray, rgb: np.ndarray, index: int) -> bytes:
    return xyz[index].tobytes() + rgb[index].tobytes()


def _xyz_key(xyz: np.ndarray, index: int) -> bytes:
    return xyz[index].tobytes()


@dataclass(frozen=True, slots=True)
class VersionDiff:
    old_hash: str
    new_hash: str
    added: int
    removed: int
    color_changed: int
    unchanged: int
    added_indices: tuple[int, ...]
    removed_indices: tuple[int, ...]
    color_changed_indices: tuple[int, ...]
    added_bounds: Bounds | None
    removed_bounds: Bounds | None
    changed_bounds: Bounds | None

    def to_dict(self) -> dict[str, Any]:
        return {
            "old_hash": self.old_hash,
            "new_hash": self.new_hash,
            "added": self.added,
            "removed": self.removed,
            "color_changed": self.color_changed,
            "unchanged": self.unchanged,
            "added_bounds": None
            if self.added_bounds is None
            else self.added_bounds.to_dict(),
            "removed_bounds": None
            if self.removed_bounds is None
            else self.removed_bounds.to_dict(),
            "changed_bounds": None
            if self.changed_bounds is None
            else self.changed_bounds.to_dict(),
        }


def _bounds_for(cloud: PointCloud, indices: list[int]) -> Bounds | None:
    if not indices:
        return None
    return Bounds.from_points(cloud.xyz[np.asarray(indices, dtype=np.int64)])


def diff_clouds(old: PointCloud, new: PointCloud) -> VersionDiff:
    """Compare clouds as multisets of exact XYZ/RGB records.

    Points at the same XYZ but with a different RGB are reported as color
    changes.  Only surplus points at an XYZ location count as additions or
    removals.  Input order is not significant.
    """

    old_exact = Counter(
        _record_key(old.xyz, old.rgb, index) for index in range(old.count)
    )
    old_xyz = Counter(_xyz_key(old.xyz, index) for index in range(old.count))
    new_exact = Counter(
        _record_key(new.xyz, new.rgb, index) for index in range(new.count)
    )
    new_xyz = Counter(_xyz_key(new.xyz, index) for index in range(new.count))

    exact_matches = sum(
        min(count, new_exact.get(key, 0)) for key, count in old_exact.items()
    )
    xyz_matches = sum(min(count, new_xyz.get(key, 0)) for key, count in old_xyz.items())
    color_changed = max(0, xyz_matches - exact_matches)

    exact_remaining = Counter(old_exact)
    xyz_remaining = Counter(old_xyz)
    added_indices: list[int] = []
    changed_indices: list[int] = []
    unchanged = 0
    for index in range(new.count):
        exact_key = _record_key(new.xyz, new.rgb, index)
        location_key = _xyz_key(new.xyz, index)
        if exact_remaining.get(exact_key, 0) > 0:
            exact_remaining[exact_key] -= 1
            xyz_remaining[location_key] -= 1
            unchanged += 1
        elif xyz_remaining.get(location_key, 0) > 0:
            xyz_remaining[location_key] -= 1
            changed_indices.append(index)
        else:
            added_indices.append(index)

    removed_indices: list[int] = []
    remaining = Counter(xyz_remaining)
    for index in range(old.count):
        key = _xyz_key(old.xyz, index)
        if remaining.get(key, 0) > 0:
            remaining[key] -= 1
            removed_indices.append(index)

    if len(changed_indices) != color_changed:
        raise AssertionError("color-change accounting is inconsistent")
    if len(added_indices) != sum(
        max(0, new_xyz[k] - old_xyz.get(k, 0)) for k in new_xyz
    ):
        raise AssertionError("addition accounting is inconsistent")
    if len(removed_indices) != sum(
        max(0, old_xyz[k] - new_xyz.get(k, 0)) for k in old_xyz
    ):
        raise AssertionError("removal accounting is inconsistent")

    return VersionDiff(
        old_hash=old.content_hash,
        new_hash=new.content_hash,
        added=len(added_indices),
        removed=len(removed_indices),
        color_changed=len(changed_indices),
        unchanged=unchanged,
        added_indices=tuple(added_indices),
        removed_indices=tuple(removed_indices),
        color_changed_indices=tuple(changed_indices),
        added_bounds=_bounds_for(new, added_indices),
        removed_bounds=_bounds_for(old, removed_indices),
        changed_bounds=_bounds_for(new, changed_indices),
    )
