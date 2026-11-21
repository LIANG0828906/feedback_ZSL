from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import interp_failed
from .scene import Scene
from .util import sha256_json


@dataclass(frozen=True)
class GridChangeReport:
    rebuilt: bool
    old_count: int
    new_count: int
    count_delta: int
    added_coordinate_hashes: tuple[str, ...]
    removed_coordinate_hashes: tuple[str, ...]
    reason: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "rebuilt": self.rebuilt,
            "old_count": self.old_count,
            "new_count": self.new_count,
            "count_delta": self.count_delta,
            "added_count": len(self.added_coordinate_hashes),
            "removed_count": len(self.removed_coordinate_hashes),
            "added_coordinate_hashes": list(self.added_coordinate_hashes),
            "removed_coordinate_hashes": list(self.removed_coordinate_hashes),
            "reason": self.reason,
        }


@dataclass(frozen=True)
class ProbeGrid:
    scene_id: str
    minimum: tuple[float, float, float]
    maximum: tuple[float, float, float]
    spacing: float
    counts: tuple[int, int, int]
    coordinates: np.ndarray
    probe_ids: tuple[str, ...]
    coordinate_hashes: tuple[str, ...]
    index_by_id: dict[str, int]
    neighbors: dict[str, tuple[str, ...]]

    @property
    def probe_count(self) -> int:
        return len(self.probe_ids)

    @property
    def signature(self) -> dict[str, Any]:
        return {
            "scene_id": self.scene_id,
            "minimum": list(self.minimum),
            "maximum": list(self.maximum),
            "spacing": self.spacing,
            "counts": list(self.counts),
        }

    @property
    def content_hash(self) -> str:
        return sha256_json(
            {
                "signature": self.signature,
                "coordinate_hashes": list(self.coordinate_hashes),
            }
        )

    def position(self, probe_id: str) -> np.ndarray:
        return self.coordinates[self.index_by_id[probe_id]]

    def neighbor_indices(self, index: int) -> tuple[int, ...]:
        id_array = self.probe_ids
        probe_id = id_array[index]
        return tuple(self.index_by_id[item] for item in self.neighbors[probe_id])

    def cell_for_position(self, position: np.ndarray) -> tuple[tuple[int, int, int], np.ndarray]:
        position = np.asarray(position, dtype=np.float64)
        if position.shape != (3,) or not np.all(np.isfinite(position)):
            raise interp_failed("query position must be a finite 3D point", [])
        minimum = np.asarray(self.minimum, dtype=np.float64)
        lower = minimum - 1.0e-9
        upper = minimum + (np.asarray(self.counts) - 1) * self.spacing + 1.0e-9
        if np.any(position < lower) or np.any(position > upper):
            nearest = self._nearest_id(position)
            raise interp_failed(
                "query position lies outside the complete uniform probe grid",
                [nearest] if nearest else [],
            )
        scaled = (position - minimum) / self.spacing
        base = np.minimum(np.floor(scaled).astype(int), np.asarray(self.counts) - 2)
        fraction = scaled - base
        return tuple(int(item) for item in base), fraction  # type: ignore[return-value]

    def _nearest_id(self, position: np.ndarray) -> str | None:
        if not self.probe_ids:
            return None
        distance = np.linalg.norm(self.coordinates - position[None, :], axis=1)
        return self.probe_ids[int(np.argmin(distance))]

    def trilinear_indices(
        self, base: tuple[int, int, int]
    ) -> tuple[tuple[int, ...], tuple[float, ...]]:
        nx, ny, _ = self.counts
        x, y, z = base
        indices: list[int] = []
        for dz in (0, 1):
            for dy in (0, 1):
                for dx in (0, 1):
                    indices.append(((z + dz) * ny + (y + dy)) * nx + (x + dx))
        return tuple(indices), tuple()

    def probe_id_at(self, x: int, y: int, z: int) -> str:
        nx, ny, nz = self.counts
        if not (0 <= x < nx and 0 <= y < ny and 0 <= z < nz):
            raise IndexError("grid index out of range")
        return self.probe_ids[(z * ny + y) * nx + x]


def build_grid(scene: Scene, spacing: float) -> ProbeGrid:
    if not math.isfinite(spacing) or spacing <= 0.0:
        raise interp_failed("probe spacing must be finite and positive", [])
    extents = np.asarray(scene.bounds_max) - np.asarray(scene.bounds_min)
    counts = tuple(int(math.floor(float(extent) / spacing + 1.0e-9)) + 1 for extent in extents)
    if any(count < 2 for count in counts):
        raise interp_failed(
            "probe spacing leaves an incomplete interpolation axis; reduce spacing",
            [],
        )
    nx, ny, nz = counts
    minimum = np.asarray(scene.bounds_min, dtype=np.float64)
    coordinates = np.empty((nx * ny * nz, 3), dtype=np.float64)
    probe_ids: list[str] = []
    coordinate_hashes: list[str] = []
    index_by_id: dict[str, int] = {}
    cursor = 0
    for z in range(nz):
        for y in range(ny):
            for x in range(nx):
                coordinate = minimum + np.asarray((x, y, z), dtype=np.float64) * spacing
                coordinate = np.round(coordinate, 12)
                coordinate_hash = sha256_json(
                    {
                        "scene_id": scene.scene_id,
                        "coordinate": [float(item) for item in coordinate],
                    }
                )
                probe_id = f"p{x:04d}_{y:04d}_{z:04d}"
                coordinates[cursor] = coordinate
                probe_ids.append(probe_id)
                coordinate_hashes.append(coordinate_hash)
                index_by_id[probe_id] = cursor
                cursor += 1
    coordinates.setflags(write=False)

    neighbors: dict[str, tuple[str, ...]] = {}
    for z in range(nz):
        for y in range(ny):
            for x in range(nx):
                probe_id = f"p{x:04d}_{y:04d}_{z:04d}"
                linked: list[str] = []
                for dx, dy, dz in (
                    (-1, 0, 0),
                    (1, 0, 0),
                    (0, -1, 0),
                    (0, 1, 0),
                    (0, 0, -1),
                    (0, 0, 1),
                ):
                    qx, qy, qz = x + dx, y + dy, z + dz
                    if 0 <= qx < nx and 0 <= qy < ny and 0 <= qz < nz:
                        linked.append(f"p{qx:04d}_{qy:04d}_{qz:04d}")
                neighbors[probe_id] = tuple(linked)

    return ProbeGrid(
        scene_id=scene.scene_id,
        minimum=scene.bounds_min,
        maximum=scene.bounds_max,
        spacing=spacing,
        counts=counts,
        coordinates=coordinates,
        probe_ids=tuple(probe_ids),
        coordinate_hashes=tuple(coordinate_hashes),
        index_by_id=index_by_id,
        neighbors=neighbors,
    )


def grid_change(old: ProbeGrid | None, new: ProbeGrid) -> GridChangeReport:
    if old is None:
        return GridChangeReport(
            rebuilt=True,
            old_count=0,
            new_count=new.probe_count,
            count_delta=new.probe_count,
            added_coordinate_hashes=new.coordinate_hashes,
            removed_coordinate_hashes=(),
            reason="initial_grid",
        )
    rebuilt = old.signature != new.signature
    if not rebuilt:
        return GridChangeReport(
            rebuilt=False,
            old_count=old.probe_count,
            new_count=new.probe_count,
            count_delta=0,
            added_coordinate_hashes=(),
            removed_coordinate_hashes=(),
            reason="grid_signature_unchanged",
        )
    old_hashes = set(old.coordinate_hashes)
    new_hashes = set(new.coordinate_hashes)
    added = tuple(item for item in new.coordinate_hashes if item not in old_hashes)
    removed = tuple(item for item in old.coordinate_hashes if item not in new_hashes)
    return GridChangeReport(
        rebuilt=True,
        old_count=old.probe_count,
        new_count=new.probe_count,
        count_delta=new.probe_count - old.probe_count,
        added_coordinate_hashes=added,
        removed_coordinate_hashes=removed,
        reason="spacing_or_bounds_changed",
    )

