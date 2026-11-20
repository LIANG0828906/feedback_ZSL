from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass
from typing import Any, Iterable

import numpy as np

from .material import MaterialMap
from .terrain import Block, Terrain


DEFAULT_NORMAL = np.array([0.0, 1.0, 0.0], dtype=np.float64)


@dataclass
class NormalEntry:
    normals: np.ndarray
    signature: str
    digest: str
    material_indices: frozenset[int]
    default_count: int


@dataclass
class NormalUpdate:
    reused_blocks: list[str]
    recomputed_blocks: list[str]
    normal_list: list[dict[str, Any]]

    def to_dict(self) -> dict[str, Any]:
        return {
            "reused_blocks": list(self.reused_blocks),
            "recomputed_blocks": list(self.recomputed_blocks),
            "normal_list": [dict(item) for item in self.normal_list],
        }


def compute_normals(heights: np.ndarray, spacing: float) -> np.ndarray:
    patch = np.asarray(heights, dtype=np.float64)
    rows, cols = patch.shape
    d_row = np.empty_like(patch)
    d_col = np.empty_like(patch)

    if rows == 1:
        d_row.fill(0.0)
    else:
        d_row[1:-1, :] = (patch[2:, :] - patch[:-2, :]) / (2.0 * spacing)
        d_row[0, :] = (patch[1, :] - patch[0, :]) / spacing
        d_row[-1, :] = (patch[-1, :] - patch[-2, :]) / spacing

    if cols == 1:
        d_col.fill(0.0)
    else:
        d_col[:, 1:-1] = (patch[:, 2:] - patch[:, :-2]) / (2.0 * spacing)
        d_col[:, 0] = (patch[:, 1] - patch[:, 0]) / spacing
        d_col[:, -1] = (patch[:, -1] - patch[:, -2]) / spacing

    normals = np.empty((rows, cols, 3), dtype=np.float64)
    normals[..., 0] = -d_col
    normals[..., 1] = 1.0
    normals[..., 2] = -d_row
    lengths = np.linalg.norm(normals, axis=2)
    zero_gradient = lengths <= 1e-15
    normals[zero_gradient] = DEFAULT_NORMAL
    nonzero = ~zero_gradient
    normals[nonzero] /= lengths[nonzero, None]
    return normals


class NormalManager:
    """Per-block normal cache keyed by height, spacing, and material content."""

    def __init__(self) -> None:
        self._cache: dict[str, NormalEntry] = {}

    def clear(self) -> None:
        self._cache.clear()

    def invalidate_blocks(self, block_ids: Iterable[str]) -> None:
        for block_id in block_ids:
            self._cache.pop(block_id, None)

    def invalidate_material_ids(
        self, changed_material_ids: Iterable[int]
    ) -> list[str]:
        changed = set(int(value) for value in changed_material_ids)
        affected = [
            block_id
            for block_id, entry in self._cache.items()
            if entry.material_indices & changed
        ]
        self.invalidate_blocks(affected)
        return sorted(affected)

    def ensure(
        self,
        terrain: Terrain,
        block_ids: Iterable[str],
        material: MaterialMap | None = None,
    ) -> NormalUpdate:
        reused: list[str] = []
        recomputed: list[str] = []
        list_entries: list[dict[str, Any]] = []
        for block_id in sorted(set(block_ids)):
            block: Block = terrain.block_by_id[block_id]
            patch = terrain.block_heights(block)
            patch_bytes = np.ascontiguousarray(patch).tobytes(order="C")
            material_indices = frozenset(
                int(value)
                for value in np.unique(
                    material.indices[block.row0 : block.row1 + 1, block.col0 : block.col1 + 1]
                )
            ) if material is not None else frozenset()
            material_bytes = (
                material.indices[block.row0 : block.row1 + 1, block.col0 : block.col1 + 1].tobytes(order="C")
                if material is not None
                else b""
            )
            signature = hashlib.sha256(
                patch_bytes
                + material_bytes
                + np.float64(terrain.spacing).tobytes()
            ).hexdigest()
            existing = self._cache.get(block_id)
            if existing is not None and existing.signature == signature:
                entry = existing
                reused.append(block_id)
            else:
                normals = compute_normals(patch, terrain.spacing)
                default_count = int(
                    np.count_nonzero(
                        np.all(np.isclose(normals, DEFAULT_NORMAL, atol=1e-15), axis=2)
                    )
                )
                entry = NormalEntry(
                    normals=normals,
                    signature=signature,
                    digest=hashlib.sha256(normals.tobytes(order="C")).hexdigest(),
                    material_indices=material_indices,
                    default_count=default_count,
                )
                self._cache[block_id] = entry
                recomputed.append(block_id)
            list_entries.append(
                {
                    "block_id": block_id,
                    "digest": entry.digest,
                    "count": int(entry.normals.shape[0] * entry.normals.shape[1]),
                    "default_normal_count": entry.default_count,
                    "normalized": bool(
                        np.allclose(
                            np.linalg.norm(entry.normals, axis=2), 1.0, atol=1e-12
                        )
                    ),
                }
            )
        return NormalUpdate(
            reused_blocks=sorted(reused),
            recomputed_blocks=sorted(recomputed),
            normal_list=list_entries,
        )

    def get(self, block_id: str) -> NormalEntry:
        return self._cache[block_id]

    def sample(self, block_id: str, row: float, col: float, block: Block) -> np.ndarray:
        entry = self._cache[block_id]
        local_row = min(max(row - block.row0, 0.0), block.rows - 1)
        local_col = min(max(col - block.col0, 0.0), block.cols - 1)
        r0 = min(int(math.floor(local_row)), block.rows - 1)
        c0 = min(int(math.floor(local_col)), block.cols - 1)
        r1 = min(r0 + 1, block.rows - 1)
        c1 = min(c0 + 1, block.cols - 1)
        fr = local_row - r0
        fc = local_col - c0
        top = entry.normals[r0, c0] * (1.0 - fc) + entry.normals[r0, c1] * fc
        bottom = entry.normals[r1, c0] * (1.0 - fc) + entry.normals[r1, c1] * fc
        normal = top * (1.0 - fr) + bottom * fr
        length = float(np.linalg.norm(normal))
        if length <= 1e-15:
            return DEFAULT_NORMAL.copy()
        return normal / length

