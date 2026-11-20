"""Regular probe lattice construction and deterministic neighbour topology."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .canonical import hash_arrays, hash_object
from .errors import ErrorCode, ProbeError
from .scene import Scene


_UNDIRECTED_OFFSETS: tuple[tuple[int, int, int], ...] = (
    (0, 0, 1),
    (0, 1, -1),
    (0, 1, 0),
    (0, 1, 1),
    (1, -1, -1),
    (1, -1, 0),
    (1, -1, 1),
    (1, 0, -1),
    (1, 0, 0),
    (1, 0, 1),
    (1, 1, -1),
    (1, 1, 0),
    (1, 1, 1),
)


@dataclass(frozen=True)
class ProbeGrid:
    dims: tuple[int, int, int]
    spacing: float
    origin: tuple[float, float, float]
    centers: np.ndarray
    neighbor_offsets: tuple[np.ndarray, ...]
    neighbor_edges: np.ndarray
    adjacency: tuple[np.ndarray, ...]
    structure_hash: str
    bounds_min: tuple[float, float, float]
    bounds_max: tuple[float, float, float]

    @property
    def count(self) -> int:
        return int(self.centers.shape[0])

    @property
    def edge_count(self) -> int:
        return int(self.neighbor_edges.shape[0])

    def index(self, ix: int, iy: int, iz: int) -> int:
        nx, ny, nz = self.dims
        if not (0 <= ix < nx and 0 <= iy < ny and 0 <= iz < nz):
            return -1
        return (ix * ny + iy) * nz + iz

    def coordinates(self, probe_id: int) -> tuple[int, int, int]:
        nx, ny, nz = self.dims
        iz = probe_id % nz
        iy = (probe_id // nz) % ny
        ix = probe_id // (ny * nz)
        return int(ix), int(iy), int(iz)

    def position(self, probe_id: int) -> np.ndarray:
        return self.centers[int(probe_id)].copy()

    def neighbors(self, probe_id: int) -> np.ndarray:
        return self.adjacency[int(probe_id)]

    def nearest_probe(self, position: np.ndarray | tuple[float, float, float]) -> int:
        point = np.asarray(position, dtype=np.float64)
        if point.shape != (3,) or not np.isfinite(point).all():
            raise ProbeError(ErrorCode.INTERP_FAILED, "probe query position must be a finite vec3")
        relative = (point - np.asarray(self.origin, dtype=np.float64)) / self.spacing
        indices = np.rint(relative).astype(np.int64)
        if np.any(indices < 0) or any(indices[axis] >= self.dims[axis] for axis in range(3)):
            raise ProbeError(
                ErrorCode.INTERP_FAILED,
                "probe query position lies outside the grid",
                details={"position": point.tolist()},
            )
        return self.index(int(indices[0]), int(indices[1]), int(indices[2]))


@dataclass(frozen=True)
class GridBuildResult:
    grid: ProbeGrid
    old_probe_count: int | None
    new_probe_count: int
    old_edge_count: int | None
    new_edge_count: int
    structure_hash: str
    rebuilt: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "old_probe_count": self.old_probe_count,
            "new_probe_count": self.new_probe_count,
            "old_edge_count": self.old_edge_count,
            "new_edge_count": self.new_edge_count,
            "structure_hash": self.structure_hash,
            "rebuilt": self.rebuilt,
        }


def _axis_positions(low: float, high: float, spacing: float) -> np.ndarray:
    extent = high - low
    count = int(math.floor(extent / spacing + 1e-9)) + 1
    if count < 1:
        count = 1
    positions = low + np.arange(count, dtype=np.float64) * spacing
    if positions[-1] > high + 1e-9:
        positions = positions[:-1]
    if positions.size == 0 or positions[-1] < low - 1e-9:
        raise ProbeError(ErrorCode.SCENE_INVALID, "spacing produced no probes")
    return positions


def build_probe_grid(
    scene: Scene,
    spacing: float | None = None,
    previous: ProbeGrid | None = None,
) -> GridBuildResult:
    actual_spacing = scene.spacing if spacing is None else float(spacing)
    if not math.isfinite(actual_spacing) or actual_spacing <= 0.0:
        raise ProbeError(ErrorCode.SCENE_INVALID, "probe spacing must be finite and positive")

    axes = [
        _axis_positions(scene.bounds_min[axis], scene.bounds_max[axis], actual_spacing)
        for axis in range(3)
    ]
    dims = tuple(int(axis.size) for axis in axes)
    if any(size <= 0 for size in dims):
        raise ProbeError(ErrorCode.SCENE_INVALID, "probe grid has an empty dimension")

    mesh = np.meshgrid(*axes, indexing="ij")
    centers = np.stack([axis.ravel(order="C") for axis in mesh], axis=1)
    centers.setflags(write=False)
    nx, ny, nz = dims

    edges: list[tuple[int, int]] = []
    for ix in range(nx):
        for iy in range(ny):
            for iz in range(nz):
                source = (ix * ny + iy) * nz + iz
                for dx, dy, dz in _UNDIRECTED_OFFSETS:
                    target = (
                        ((ix + dx) * ny + (iy + dy)) * nz + (iz + dz)
                        if 0 <= ix + dx < nx and 0 <= iy + dy < ny and 0 <= iz + dz < nz
                        else -1
                    )
                    if target >= 0:
                        edges.append((source, target))

    edge_array = np.asarray(edges, dtype=np.int64).reshape((-1, 2))
    if edges:
        edge_array.setflags(write=False)
    adjacency_lists: list[list[int]] = [[] for _ in range(centers.shape[0])]
    for source, target in edges:
        adjacency_lists[source].append(int(target))
        adjacency_lists[target].append(int(source))
    adjacency: list[np.ndarray] = []
    for values in adjacency_lists:
        item = np.asarray(values, dtype=np.int64)
        item.setflags(write=False)
        adjacency.append(item)

    neighbor_offsets = tuple(
        np.asarray([(dx, dy, dz) for dx, dy, dz in _UNDIRECTED_OFFSETS], dtype=np.int64)
    )
    structure_hash = hash_object(
        {
            "schema": "probe-grid-v1",
            "bounds_min": scene.bounds_min,
            "bounds_max": scene.bounds_max,
            "spacing": actual_spacing,
            "dims": dims,
            "centers_hash": hash_arrays([("centers", centers)]),
            "neighbor_edges_hash": hash_arrays([("edges", edge_array)]),
        }
    )
    grid = ProbeGrid(
        dims=dims,
        spacing=actual_spacing,
        origin=scene.bounds_min,
        centers=centers,
        neighbor_offsets=neighbor_offsets,
        neighbor_edges=edge_array,
        adjacency=tuple(adjacency),
        structure_hash=structure_hash,
        bounds_min=scene.bounds_min,
        bounds_max=scene.bounds_max,
    )
    changed = previous is None or previous.structure_hash != structure_hash
    return GridBuildResult(
        grid=grid,
        old_probe_count=None if previous is None else previous.count,
        new_probe_count=grid.count,
        old_edge_count=None if previous is None else previous.edge_count,
        new_edge_count=grid.edge_count,
        structure_hash=structure_hash,
        rebuilt=changed,
    )
