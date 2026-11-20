from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any

import numpy as np

from .errors import E_INVALID_ARGUMENT, TerrainError
from .heightfield import HeightField
from .material import MaterialMap


@dataclass(frozen=True)
class Block:
    id: str
    chunk_row: int
    chunk_col: int
    row0: int
    row1: int
    col0: int
    col1: int

    @property
    def rows(self) -> int:
        return self.row1 - self.row0 + 1

    @property
    def cols(self) -> int:
        return self.col1 - self.col0 + 1

    @property
    def quad_rows(self) -> int:
        return self.row1 - self.row0

    @property
    def quad_cols(self) -> int:
        return self.col1 - self.col0

    @property
    def center_row(self) -> float:
        return (self.row0 + self.row1) * 0.5

    @property
    def center_col(self) -> float:
        return (self.col0 + self.col1) * 0.5

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "chunk_row": self.chunk_row,
            "chunk_col": self.chunk_col,
            "row0": self.row0,
            "row1": self.row1,
            "col0": self.col0,
            "col1": self.col1,
        }


@dataclass
class QuadNode:
    id: str
    depth: int
    row0: int
    row1: int
    col0: int
    col1: int
    chunk_row0: int
    chunk_row1: int
    chunk_col0: int
    chunk_col1: int
    min_height: float
    max_height: float
    truth_source: str
    children: list["QuadNode"] = field(default_factory=list)

    @property
    def relief(self) -> float:
        return self.max_height - self.min_height

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "depth": self.depth,
            "row_range": [self.row0, self.row1],
            "col_range": [self.col0, self.col1],
            "chunk_row_range": [self.chunk_row0, self.chunk_row1],
            "chunk_col_range": [self.chunk_col0, self.chunk_col1],
            "min_height": round(self.min_height, 9),
            "max_height": round(self.max_height, 9),
            "relief": round(self.relief, 9),
            "truth_source": self.truth_source,
            "children": [child.id for child in self.children],
        }


@dataclass(frozen=True)
class TreeBuildReport:
    old_node_count: int
    new_node_count: int
    old_block_count: int
    new_block_count: int
    max_height_delta: float
    block_size: int
    spacing: float
    reason: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "old_node_count": self.old_node_count,
            "new_node_count": self.new_node_count,
            "old_block_count": self.old_block_count,
            "new_block_count": self.new_block_count,
            "max_height_delta": self.max_height_delta,
            "block_size": self.block_size,
            "spacing": self.spacing,
            "reason": self.reason,
        }


def _block_id(row: int, col: int) -> str:
    return f"B{row:04d}_{col:04d}"


class Terrain:
    """Chunked heightfield with a deterministic chunk quadtree."""

    def __init__(
        self,
        heightfield: HeightField,
        material: MaterialMap | None = None,
        block_size: int = 16,
    ) -> None:
        if block_size < 1:
            raise TerrainError(
                E_INVALID_ARGUMENT, "block size must be positive", block_size=block_size
            )
        if material is not None and (
            material.width != heightfield.width or material.height != heightfield.height
        ):
            raise TerrainError(
                E_INVALID_ARGUMENT,
                "material image dimensions must match the heightfield",
                heightfield=[heightfield.height, heightfield.width],
                material=[material.height, material.width],
            )
        self.heightfield = heightfield
        self.material = material
        self.block_size = int(block_size)
        self.blocks: list[Block] = []
        self.block_by_id: dict[str, Block] = {}
        self.quadtree_root: QuadNode | None = None
        self.node_by_id: dict[str, QuadNode] = {}
        self._build_reason = "initial"
        self._previous_node_count = 0
        self._previous_block_count = 0
        self._last_build_report: TreeBuildReport | None = None
        self._build_tree()

    @property
    def width(self) -> int:
        return self.heightfield.width

    @property
    def height(self) -> int:
        return self.heightfield.height

    @property
    def spacing(self) -> float:
        return self.heightfield.spacing

    @property
    def node_count(self) -> int:
        return len(self.node_by_id)

    @property
    def last_build_report(self) -> TreeBuildReport:
        assert self._last_build_report is not None
        return self._last_build_report

    def _make_blocks(self) -> None:
        height_quads = self.height - 1
        width_quads = self.width - 1
        chunks_y = int(math.ceil(height_quads / self.block_size))
        chunks_x = int(math.ceil(width_quads / self.block_size))
        blocks: list[Block] = []
        for chunk_row in range(chunks_y):
            for chunk_col in range(chunks_x):
                row0 = chunk_row * self.block_size
                col0 = chunk_col * self.block_size
                row1 = min(row0 + self.block_size, height_quads)
                col1 = min(col0 + self.block_size, width_quads)
                blocks.append(
                    Block(
                        id=_block_id(chunk_row, chunk_col),
                        chunk_row=chunk_row,
                        chunk_col=chunk_col,
                        row0=row0,
                        row1=row1,
                        col0=col0,
                        col1=col1,
                    )
                )
        self.blocks = blocks
        self.block_by_id = {block.id: block for block in blocks}

    def _build_quadtree(self) -> None:
        self.node_by_id = {}
        if not self.blocks:
            self.quadtree_root = None
            return
        chunk_rows = max(block.chunk_row for block in self.blocks) + 1
        chunk_cols = max(block.chunk_col for block in self.blocks) + 1

        def build(
            cr0: int, cr1: int, cc0: int, cc1: int, depth: int
        ) -> QuadNode:
            selected_blocks = [
                block
                for block in self.blocks
                if cr0 <= block.chunk_row <= cr1 and cc0 <= block.chunk_col <= cc1
            ]
            row0 = min(block.row0 for block in selected_blocks)
            row1 = max(block.row1 for block in selected_blocks)
            col0 = min(block.col0 for block in selected_blocks)
            col1 = max(block.col1 for block in selected_blocks)
            patch = self.heightfield.heights[row0 : row1 + 1, col0 : col1 + 1]
            node = QuadNode(
                id=f"Q{depth}:{cr0}-{cr1}:{cc0}-{cc1}",
                depth=depth,
                row0=row0,
                row1=row1,
                col0=col0,
                col1=col1,
                chunk_row0=cr0,
                chunk_row1=cr1,
                chunk_col0=cc0,
                chunk_col1=cc1,
                min_height=float(np.min(patch)),
                max_height=float(np.max(patch)),
                truth_source="heightfield"
                if cr0 == cr1 and cc0 == cc1
                else "derived_from_heightfield",
            )
            self.node_by_id[node.id] = node
            if cr0 == cr1 and cc0 == cc1:
                return node
            mid_r = (cr0 + cr1) // 2
            mid_c = (cc0 + cc1) // 2
            ranges = [
                (cr0, mid_r, cc0, mid_c),
                (cr0, mid_r, mid_c + 1, cc1),
                (mid_r + 1, cr1, cc0, mid_c),
                (mid_r + 1, cr1, mid_c + 1, cc1),
            ]
            for child_r0, child_r1, child_c0, child_c1 in ranges:
                if child_r0 <= child_r1 and child_c0 <= child_c1:
                    node.children.append(
                        build(child_r0, child_r1, child_c0, child_c1, depth + 1)
                    )
            return node

        self.quadtree_root = build(0, chunk_rows - 1, 0, chunk_cols - 1, 0)

    def _build_tree(self) -> None:
        old_nodes = self._previous_node_count
        old_blocks = self._previous_block_count
        self._make_blocks()
        self._build_quadtree()
        report = TreeBuildReport(
            old_node_count=old_nodes,
            new_node_count=self.node_count,
            old_block_count=old_blocks,
            new_block_count=len(self.blocks),
            max_height_delta=0.0,
            block_size=self.block_size,
            spacing=self.spacing,
            reason=self._build_reason,
        )
        self._previous_node_count = self.node_count
        self._previous_block_count = len(self.blocks)
        self._last_build_report = report
        self._build_reason = "rebuild"

    def rebuild(self, block_size: int | None = None, spacing: float | None = None, reason: str = "rebuild") -> TreeBuildReport:
        if block_size is not None:
            if block_size < 1:
                raise TerrainError(
                    E_INVALID_ARGUMENT,
                    "block size must be positive",
                    block_size=block_size,
                )
            self.block_size = int(block_size)
        if spacing is not None:
            if not math.isfinite(spacing) or spacing <= 0.0:
                raise TerrainError(
                    E_INVALID_ARGUMENT,
                    "grid spacing must be finite and positive",
                    spacing=spacing,
                )
            self.heightfield = HeightField(
                heights=self.heightfield.heights,
                spacing=float(spacing),
                quantization_step=self.heightfield.quantization_step,
                valid_min=self.heightfield.valid_min,
                valid_max=self.heightfield.valid_max,
                source_digest=self.heightfield.source_digest,
            )
        self._build_reason = reason
        self._build_tree()
        return self.last_build_report

    def node_tree(self) -> dict[str, Any] | None:
        if self.quadtree_root is None:
            return None

        def visit(node: QuadNode) -> dict[str, Any]:
            result = node.to_dict()
            result["children_nodes"] = [visit(child) for child in node.children]
            return result

        return visit(self.quadtree_root)

    def block_heights(self, block_or_id: Block | str) -> np.ndarray:
        block = (
            block_or_id
            if isinstance(block_or_id, Block)
            else self.block_by_id[block_or_id]
        )
        return self.heightfield.heights[block.row0 : block.row1 + 1, block.col0 : block.col1 + 1]

