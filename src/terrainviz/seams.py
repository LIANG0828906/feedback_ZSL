from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any

import numpy as np

from .errors import E_CRACK_UNRESOLVED, E_INVALID_ARGUMENT, TerrainError
from .lod import LodDecision, LodSelection, selection_for_level, stride_for_level
from .terrain import Block, Terrain


@dataclass(frozen=True)
class SeamRecord:
    id: str
    block_a: str
    block_b: str
    edge_a: str
    edge_b: str
    level_a: int
    level_b: int
    pre_repair_deviation: float
    post_repair_deviation: float
    tolerance: float
    sample_count: int
    repair_method: str
    closed: bool
    refined: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "blocks": [self.block_a, self.block_b],
            "edges": [self.edge_a, self.edge_b],
            "levels": [self.level_a, self.level_b],
            "pre_repair_deviation": round(self.pre_repair_deviation, 12),
            "post_repair_deviation": round(self.post_repair_deviation, 12),
            "tolerance": self.tolerance,
            "sample_count": self.sample_count,
            "repair_method": self.repair_method,
            "closed": self.closed,
            "refined": self.refined,
        }


@dataclass
class SeamRepairResult:
    decision: LodDecision
    seams: list[SeamRecord]
    boundary_samples: dict[str, dict[str, np.ndarray]]
    adjusted_block_ids: list[str] = field(default_factory=list)

    @property
    def status(self) -> str:
        if not self.seams:
            return "no_adjacent_blocks"
        return "closed" if all(seam.closed for seam in self.seams) else "unresolved"

    def summary(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "method": "matched_vertices",
            "seam_count": len(self.seams),
            "closed_seams": sum(1 for seam in self.seams if seam.closed),
            "max_pre_repair_deviation": round(
                max((seam.pre_repair_deviation for seam in self.seams), default=0.0), 12
            ),
            "max_post_repair_deviation": round(
                max((seam.post_repair_deviation for seam in self.seams), default=0.0), 12
            ),
            "tolerance": self.seams[0].tolerance if self.seams else None,
            "adjusted_block_ids": list(self.adjusted_block_ids),
        }

    def to_dict(self) -> dict[str, Any]:
        return {"summary": self.summary(), "seams": [seam.to_dict() for seam in self.seams]}


def _base_positions(block: Block, edge: str, level: int) -> np.ndarray:
    stride = stride_for_level(level)
    if edge in ("E", "W"):
        return np.arange(block.row0, block.row1 + 1, stride, dtype=np.int64)
    return np.arange(block.col0, block.col1 + 1, stride, dtype=np.int64)


def _boundary_heights(
    terrain: Terrain, block: Block, edge: str, positions: np.ndarray
) -> np.ndarray:
    if edge in ("E", "W"):
        col = block.col1 if edge == "E" else block.col0
        return terrain.heightfield.heights[positions, col].astype(np.float64)
    row = block.row1 if edge == "S" else block.row0
    return terrain.heightfield.heights[row, positions].astype(np.float64)


def _union_positions(a: np.ndarray, b: np.ndarray) -> np.ndarray:
    return np.unique(np.concatenate([a, b])).astype(np.int64, copy=False)


def _deviation_for_profile(
    base_positions: np.ndarray,
    heights: np.ndarray,
    union_positions: np.ndarray,
    exact_heights: np.ndarray,
) -> float:
    profile = np.interp(
        union_positions.astype(np.float64),
        base_positions.astype(np.float64),
        heights,
    )
    return float(np.max(np.abs(profile - exact_heights), initial=0.0))


def _pair_adjacencies(terrain: Terrain) -> list[tuple[Block, str, Block, str]]:
    pairs: list[tuple[Block, str, Block, str]] = []
    for block in sorted(terrain.blocks, key=lambda item: item.id):
        east_id = f"B{block.chunk_row:04d}_{block.chunk_col + 1:04d}"
        south_id = f"B{block.chunk_row + 1:04d}_{block.chunk_col:04d}"
        east = terrain.block_by_id.get(east_id)
        south = terrain.block_by_id.get(south_id)
        if east is not None and east.row0 == block.row0 and east.row1 == block.row1:
            pairs.append((block, "E", east, "W"))
        if south is not None and south.col0 == block.col0 and south.col1 == block.col1:
            pairs.append((block, "S", south, "N"))
    return pairs


def _pair_mismatch(
    terrain: Terrain,
    pair: tuple[Block, str, Block, str],
    selections: dict[str, LodSelection],
) -> tuple[float, np.ndarray]:
    a, edge_a, b, edge_b = pair
    level_a = selections[a.id].level
    level_b = selections[b.id].level
    positions_a = _base_positions(a, edge_a, level_a)
    positions_b = _base_positions(b, edge_b, level_b)
    union = _union_positions(positions_a, positions_b)
    exact = _boundary_heights(terrain, a, edge_a, union)
    heights_a = _boundary_heights(terrain, a, edge_a, positions_a)
    heights_b = _boundary_heights(terrain, b, edge_b, positions_b)
    deviation_a = _deviation_for_profile(positions_a, heights_a, union, exact)
    deviation_b = _deviation_for_profile(positions_b, heights_b, union, exact)
    return max(deviation_a, deviation_b), union


def _seam_id(a: Block, b: Block) -> str:
    left, right = sorted((a.id, b.id))
    return f"S:{left}:{right}"


def repair_seams(
    terrain: Terrain,
    decision: LodDecision,
    tolerance: float,
    max_refinement_passes: int = 256,
) -> SeamRepairResult:
    if not math.isfinite(tolerance) or tolerance < 0.0:
        raise TerrainError(
            E_INVALID_ARGUMENT,
            "seam tolerance must be finite and non-negative",
            tolerance=tolerance,
        )
    if max_refinement_passes < 0:
        raise TerrainError(
            E_INVALID_ARGUMENT,
            "max refinement passes must be non-negative",
            max_refinement_passes=max_refinement_passes,
        )
    pairs = _pair_adjacencies(terrain)
    selections = dict(decision.selections)
    adjusted: set[str] = set()
    initial_levels = {key: value.level for key, value in selections.items()}

    for pass_index in range(max_refinement_passes + 1):
        unresolved: list[tuple[float, tuple[Block, str, Block, str]]] = []
        for pair in pairs:
            mismatch, _ = _pair_mismatch(terrain, pair, selections)
            if mismatch > tolerance + 1e-12:
                unresolved.append((mismatch, pair))
        if not unresolved:
            break
        if pass_index == max_refinement_passes:
            mismatch, pair = max(unresolved, key=lambda item: (item[0], item[1][0].id))
            a, edge_a, b, edge_b = pair
            raise TerrainError(
                E_CRACK_UNRESOLVED,
                "seam deviation remains above tolerance after refinement",
                seam_id=_seam_id(a, b),
                edge_a=edge_a,
                edge_b=edge_b,
                block_a=a.id,
                block_b=b.id,
                deviation=mismatch,
                tolerance=tolerance,
            )
        mismatch, pair = max(
            unresolved, key=lambda item: (item[0], item[1][0].id, item[1][1])
        )
        a, edge_a, b, edge_b = pair
        level_a = selections[a.id].level
        level_b = selections[b.id].level
        if level_a == 0 and level_b == 0:
            raise TerrainError(
                E_CRACK_UNRESOLVED,
                "fully refined adjacent blocks still disagree at the seam",
                seam_id=_seam_id(a, b),
                edge_a=edge_a,
                edge_b=edge_b,
                block_a=a.id,
                block_b=b.id,
                deviation=mismatch,
                tolerance=tolerance,
            )
        if level_a > level_b:
            refine_id = a.id
        elif level_b > level_a:
            refine_id = b.id
        else:
            refine_id = max(a.id, b.id)
        block_to_refine = terrain.block_by_id[refine_id]
        new_level = selections[refine_id].level - 1
        selections[refine_id] = selection_for_level(
            terrain,
            decision.camera,
            block_to_refine,
            new_level,
            decision.config,
            fallback=selections[refine_id].fallback,
            seam_adjusted=True,
        )
        adjusted.add(refine_id)

    boundary_samples: dict[str, dict[str, np.ndarray]] = {
        block.id: {
            edge: _base_positions(block, edge, selections[block.id].level)
            for edge in ("N", "S", "E", "W")
        }
        for block in terrain.blocks
    }
    seam_records: list[SeamRecord] = []
    for pair in pairs:
        a, edge_a, b, edge_b = pair
        level_a = selections[a.id].level
        level_b = selections[b.id].level
        positions_a = _base_positions(a, edge_a, level_a)
        positions_b = _base_positions(b, edge_b, level_b)
        union = _union_positions(positions_a, positions_b)
        exact = _boundary_heights(terrain, a, edge_a, union)
        pre_a = _deviation_for_profile(
            positions_a,
            _boundary_heights(terrain, a, edge_a, positions_a),
            union,
            exact,
        )
        pre_b = _deviation_for_profile(
            positions_b,
            _boundary_heights(terrain, b, edge_b, positions_b),
            union,
            exact,
        )
        matched_a = _boundary_heights(terrain, a, edge_a, union)
        matched_b = _boundary_heights(terrain, b, edge_b, union)
        post = float(np.max(np.abs(matched_a - matched_b), initial=0.0))
        boundary_samples[a.id][edge_a] = union
        boundary_samples[b.id][edge_b] = union
        refined = any(
            selections[block_id].level != initial_levels[block_id]
            for block_id in (a.id, b.id)
        )
        seam_records.append(
            SeamRecord(
                id=_seam_id(a, b),
                block_a=a.id,
                block_b=b.id,
                edge_a=edge_a,
                edge_b=edge_b,
                level_a=level_a,
                level_b=level_b,
                pre_repair_deviation=max(pre_a, pre_b),
                post_repair_deviation=post,
                tolerance=float(tolerance),
                sample_count=int(union.size),
                repair_method="matched_vertices",
                closed=post <= tolerance + 1e-12,
                refined=refined,
            )
        )
        if post > tolerance + 1e-12:
            raise TerrainError(
                E_CRACK_UNRESOLVED,
                "matched-vertex seam repair could not meet tolerance",
                seam_id=_seam_id(a, b),
                edge_a=edge_a,
                edge_b=edge_b,
                block_a=a.id,
                block_b=b.id,
                deviation=post,
                tolerance=tolerance,
                sample_count=int(union.size),
            )

    updated = LodDecision(
        config=decision.config,
        camera=decision.camera,
        selections={key: selections[key] for key in sorted(selections)},
    )
    return SeamRepairResult(
        decision=updated,
        seams=sorted(seam_records, key=lambda item: item.id),
        boundary_samples=boundary_samples,
        adjusted_block_ids=sorted(adjusted),
    )

