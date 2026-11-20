"""Screen chunk table construction."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class Chunk:
    index: int
    id: str
    row: int
    col: int
    x0: int
    y0: int
    x1: int
    y1: int

    @property
    def width(self) -> int:
        return self.x1 - self.x0

    @property
    def height(self) -> int:
        return self.y1 - self.y0

    @property
    def bounds(self) -> tuple[int, int, int, int]:
        return self.x0, self.y0, self.x1, self.y1

    @property
    def content_shape(self) -> tuple[int, int, int]:
        return self.height, self.width, 3

    @property
    def transmittance_shape(self) -> tuple[int, int]:
        return self.height, self.width

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "id": self.id,
            "row": self.row,
            "col": self.col,
            "x0": self.x0,
            "y0": self.y0,
            "x1": self.x1,
            "y1": self.y1,
        }


def build_chunk_table(width: int, height: int, chunk_width: int, chunk_height: int) -> list[Chunk]:
    chunks: list[Chunk] = []
    cols = (width + chunk_width - 1) // chunk_width
    rows = (height + chunk_height - 1) // chunk_height
    index = 0
    for row in range(rows):
        y0 = row * chunk_height
        y1 = min(height, y0 + chunk_height)
        for col in range(cols):
            x0 = col * chunk_width
            x1 = min(width, x0 + chunk_width)
            chunks.append(
                Chunk(
                    index=index,
                    id=f"c{index:04d}",
                    row=row,
                    col=col,
                    x0=x0,
                    y0=y0,
                    x1=x1,
                    y1=y1,
                )
            )
            index += 1
    return chunks


def chunking_summary(width: int, height: int, config: object, chunks: list[Chunk]) -> dict[str, Any]:
    return {
        "chunk_width": config.chunk_width,
        "chunk_height": config.chunk_height,
        "cols": (width + config.chunk_width - 1) // config.chunk_width,
        "rows": (height + config.chunk_height - 1) // config.chunk_height,
        "count": len(chunks),
        "chunks": [chunk.to_dict() for chunk in chunks],
    }


def assemble_chunks(
    width: int,
    height: int,
    chunks: list[Chunk],
    outputs: dict[str, tuple[object, object]],
    fill: float = 0.0,
) -> tuple[object, object]:
    import numpy as np

    image = np.full((height, width, 3), fill, dtype=np.float32)
    transmittance = np.ones((height, width), dtype=np.float32)
    for chunk in chunks:
        if chunk.id not in outputs:
            continue
        value = outputs[chunk.id]
        if hasattr(value, "rgb") and hasattr(value, "transmittance"):
            rgb, trans = value.rgb, value.transmittance
        else:
            rgb, trans = value
        image[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1] = np.asarray(rgb, dtype=np.float32)
        transmittance[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1] = np.asarray(
            trans, dtype=np.float32
        )
    return image, transmittance
