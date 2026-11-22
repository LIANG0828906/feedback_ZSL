from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class Block:
    block_id: int
    row: int
    col: int
    y0: int
    y1: int
    x0: int
    x1: int

    @property
    def height(self) -> int:
        return self.y1 - self.y0

    @property
    def width(self) -> int:
        return self.x1 - self.x0

    def as_dict(self) -> dict[str, int]:
        return {
            "block_id": self.block_id,
            "row": self.row,
            "col": self.col,
            "y0": self.y0,
            "y1": self.y1,
            "x0": self.x0,
            "x1": self.x1,
        }


def make_blocks(height: int, width: int, tile_size: int) -> tuple[Block, ...]:
    blocks: list[Block] = []
    block_id = 0
    for row, y0 in enumerate(range(0, height, tile_size)):
        for col, x0 in enumerate(range(0, width, tile_size)):
            y1 = min(height, y0 + tile_size)
            x1 = min(width, x0 + tile_size)
            blocks.append(
                Block(
                    block_id=block_id,
                    row=row,
                    col=col,
                    y0=y0,
                    y1=y1,
                    x0=x0,
                    x1=x1,
                )
            )
            block_id += 1
    return tuple(blocks)


def expanded_region(
    block: Block,
    band: int,
    height: int,
    width: int,
) -> tuple[int, int, int, int]:
    return (
        max(0, block.y0 - band),
        min(height, block.y1 + band),
        max(0, block.x0 - band),
        min(width, block.x1 + band),
    )


def block_at(
    blocks: tuple[Block, ...],
    tile_size: int,
    y: int,
    x: int,
) -> Block:
    if not blocks:
        raise IndexError("no blocks")
    row = y // tile_size
    col = x // tile_size
    cols = max(block.col for block in blocks) + 1
    rows = max(block.row for block in blocks) + 1
    row = min(max(row, 0), rows - 1)
    col = min(max(col, 0), cols - 1)
    block_id = row * cols + col
    block_id = min(max(block_id, 0), len(blocks) - 1)
    return blocks[block_id]


def grid_shape(blocks: tuple[Block, ...]) -> tuple[int, int]:
    if not blocks:
        return (0, 0)
    rows = max(block.row for block in blocks) + 1
    cols = max(block.col for block in blocks) + 1
    return rows, cols
