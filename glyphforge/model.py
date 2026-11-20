from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass, field

import numpy as np


@dataclass(frozen=True)
class Affine:
    a: float = 1.0
    b: float = 0.0
    c: float = 0.0
    d: float = 1.0
    e: float = 0.0
    f: float = 0.0

    @classmethod
    def from_sequence(cls, values: Iterable[float]) -> Affine:
        items = tuple(float(value) for value in values)
        if len(items) != 6:
            raise ValueError("affine transform must contain six numbers")
        return cls(*items)

    def apply(self, points: np.ndarray) -> np.ndarray:
        x = points[:, 0]
        y = points[:, 1]
        return np.column_stack((self.a * x + self.c * y + self.e,
                                self.b * x + self.d * y + self.f))

    def determinant(self) -> float:
        return self.a * self.d - self.b * self.c

    def to_list(self) -> list[float]:
        return [self.a, self.b, self.c, self.d, self.e, self.f]


@dataclass(frozen=True)
class GlyphContour:
    index: int
    points: np.ndarray
    direction: str
    source: str = "local"

    @property
    def signed_area(self) -> float:
        points = self.points
        if len(points) < 3:
            return 0.0
        x = points[:, 0]
        y = points[:, 1]
        return float(0.5 * np.sum(x * np.roll(y, -1) - np.roll(x, -1) * y))


@dataclass(frozen=True)
class Glyph:
    name: str
    unicode: tuple[int, ...]
    advance: float
    contours: tuple[GlyphContour, ...]
    bbox: tuple[float, float, float, float]
    source_kind: str = "simple"

    def bbox_pixels(
        self,
        units_per_em: float,
        pixel_size: float,
        offset_x: float,
        offset_y: float,
    ) -> tuple[float, float, float, float]:
        scale = pixel_size / units_per_em
        x0, y0, x1, y1 = self.bbox
        return (
            x0 * scale + offset_x,
            -y1 * scale + offset_y,
            x1 * scale + offset_x,
            -y0 * scale + offset_y,
        )


@dataclass(frozen=True)
class Font:
    path: str
    format: str
    units_per_em: float
    ascender: float
    descender: float
    glyphs: dict[str, Glyph]
    unicode_map: dict[int, str]
    source_hash: str
    raw: dict = field(default_factory=dict, compare=False, repr=False)

    def glyph_for_codepoint(self, codepoint: int) -> Glyph | None:
        name = self.unicode_map.get(codepoint)
        return self.glyphs.get(name) if name is not None else None
