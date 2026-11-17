"""Chunk grid partitioning and adjacency construction.

Chunks tile the global vertex grid and *share* boundary vertices: a chunk of
`chunk_verts` vertices per side advances the grid by `chunk_verts - 1`, so
neighbouring chunks overlap exactly on their shared edge and no vertex is
missed or duplicated in the union.
"""

from dataclasses import dataclass


@dataclass(frozen=True)
class ChunkGrid:
    chunk_verts: int  # vertices per chunk side (>= 2)
    chunks_x: int
    chunks_y: int

    @property
    def verts_x(self):
        return self.chunks_x * (self.chunk_verts - 1) + 1

    @property
    def verts_y(self):
        return self.chunks_y * (self.chunk_verts - 1) + 1

    @property
    def chunk_count(self):
        return self.chunks_x * self.chunks_y

    @property
    def total_vertices(self):
        return self.verts_x * self.verts_y


def build_grid(chunk_verts, chunks_x, chunks_y):
    if chunk_verts < 2:
        raise ValueError("chunk_verts must be >= 2")
    if chunks_x < 1 or chunks_y < 1:
        raise ValueError("chunk counts must be >= 1")
    return ChunkGrid(chunk_verts, chunks_x, chunks_y)


def chunk_origin(grid, cx, cy):
    """Global vertex index of a chunk's local (0, 0) vertex."""
    return cx * (grid.chunk_verts - 1), cy * (grid.chunk_verts - 1)


def iter_chunks(grid):
    for cy in range(grid.chunks_y):
        for cx in range(grid.chunks_x):
            yield cx, cy


def neighbors(grid, cx, cy):
    """Four-neighbourhood adjacency; missing edges (world border) are None."""
    result = {}
    for name, dx, dy in (("west", -1, 0), ("east", 1, 0),
                         ("south", 0, -1), ("north", 0, 1)):
        nx, ny = cx + dx, cy + dy
        if 0 <= nx < grid.chunks_x and 0 <= ny < grid.chunks_y:
            result[name] = [nx, ny]
        else:
            result[name] = None
    return result


def chunk_bbox(positions):
    """Axis-aligned bounding box of an (n, n, 3) position array."""
    lo = positions.reshape(-1, 3).min(axis=0)
    hi = positions.reshape(-1, 3).max(axis=0)
    return {"min": [float(v) for v in lo], "max": [float(v) for v in hi]}
