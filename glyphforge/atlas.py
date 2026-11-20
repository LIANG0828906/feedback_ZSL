from __future__ import annotations

import binascii
import struct
import zlib
from collections.abc import Iterable
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import ErrorCode, GlyphForgeError
from .util import atomic_write_bytes, sha256_bytes


@dataclass(frozen=True)
class AtlasConfig:
    width: int = 512
    height: int = 256
    padding: int = 2

    def validate(self) -> None:
        if self.width <= 0 or self.height <= 0:
            raise GlyphForgeError(
                ErrorCode.E_ATLAS_OVERFLOW,
                "atlas dimensions must be positive",
                {"atlas": {"width": self.width, "height": self.height}},
            )
        if self.padding < 0 or self.padding * 2 >= self.width or self.padding * 2 >= self.height:
            raise GlyphForgeError(
                ErrorCode.E_ATLAS_OVERFLOW,
                "atlas padding leaves no usable area",
                {
                    "atlas": {
                        "width": self.width,
                        "height": self.height,
                        "padding": self.padding,
                    }
                },
            )


@dataclass(frozen=True)
class AtlasGlyph:
    name: str
    unicode: tuple[int, ...]
    coverage: np.ndarray
    advance: float
    font_bbox: tuple[float, float, float, float]
    pixel_bbox: tuple[int, int, int, int]
    coverage_hash: str


@dataclass(frozen=True)
class AtlasGlyphMetrics:
    name: str
    unicode: tuple[int, ...]
    offset: tuple[int, int]
    size: tuple[int, int]
    advance: float
    font_bbox: tuple[float, float, float, float]
    pixel_bbox: tuple[int, int, int, int]
    coverage_hash: str

    def to_dict(self) -> dict:
        return {
            "glyph": self.name,
            "unicode": list(self.unicode),
            "offset": list(self.offset),
            "size": list(self.size),
            "advance": self.advance,
            "bbox": list(self.font_bbox),
            "pixel_bbox": list(self.pixel_bbox),
            "coverage_hash": self.coverage_hash,
        }


@dataclass(frozen=True)
class AtlasResult:
    width: int
    height: int
    alpha: np.ndarray
    metrics: tuple[AtlasGlyphMetrics, ...]
    alpha_hash: str

    @property
    def rgba(self) -> np.ndarray:
        image = np.zeros((self.height, self.width, 4), dtype=np.uint8)
        image[:, :, 0:3] = 255
        image[:, :, 3] = self.alpha
        return image


def _overflow(glyph: AtlasGlyph, message: str, **details) -> GlyphForgeError:
    descriptor = {
        "glyph": glyph.name,
        "size": [int(glyph.coverage.shape[1]), int(glyph.coverage.shape[0])],
    }
    descriptor.update(details)
    return GlyphForgeError(ErrorCode.E_ATLAS_OVERFLOW, message, descriptor)


def build_atlas(
    glyphs: Iterable[AtlasGlyph],
    config: AtlasConfig,
) -> AtlasResult:
    config.validate()
    alpha = np.zeros((config.height, config.width), dtype=np.uint8)
    metrics: list[AtlasGlyphMetrics] = []
    cursor_x = config.padding
    cursor_y = config.padding
    row_height = 0
    usable_width = config.width - config.padding * 2
    usable_height = config.height - config.padding * 2

    for glyph in glyphs:
        height, width = glyph.coverage.shape
        if width > usable_width or height > usable_height:
            raise _overflow(
                glyph,
                "glyph bitmap does not fit within the atlas",
                atlas={"width": config.width, "height": config.height},
            )
        if cursor_x + width + config.padding > config.width and cursor_x > config.padding:
            cursor_x = config.padding
            cursor_y += row_height + config.padding
            row_height = 0
        if cursor_y + height + config.padding > config.height:
            raise _overflow(
                glyph,
                "atlas has insufficient vertical space",
                atlas={"width": config.width, "height": config.height},
                required_y=cursor_y + height + config.padding,
            )
        if width and height:
            alpha[
                cursor_y : cursor_y + height,
                cursor_x : cursor_x + width,
            ] = glyph.coverage
        metrics.append(
            AtlasGlyphMetrics(
                name=glyph.name,
                unicode=glyph.unicode,
                offset=(cursor_x, cursor_y),
                size=(width, height),
                advance=glyph.advance,
                font_bbox=glyph.font_bbox,
                pixel_bbox=glyph.pixel_bbox,
                coverage_hash=glyph.coverage_hash,
            )
        )
        cursor_x += width + config.padding
        row_height = max(row_height, height)

    return AtlasResult(
        width=config.width,
        height=config.height,
        alpha=alpha,
        metrics=tuple(metrics),
        alpha_hash=sha256_bytes(alpha.tobytes()),
    )


def _png_chunk(kind: bytes, payload: bytes) -> bytes:
    return (
        struct.pack(">I", len(payload))
        + kind
        + payload
        + struct.pack(">I", binascii.crc32(kind + payload) & 0xFFFFFFFF)
    )


def encode_rgba_png(rgba: np.ndarray) -> bytes:
    if rgba.ndim != 3 or rgba.shape[2] != 4 or rgba.dtype != np.uint8:
        raise ValueError("PNG input must be an HxWx4 uint8 array")
    height, width, _ = rgba.shape
    header = struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0)
    scanlines = bytearray()
    for row in rgba:
        scanlines.append(0)
        scanlines.extend(row.tobytes())
    compressed = zlib.compress(bytes(scanlines), level=9)
    return (
        b"\x89PNG\r\n\x1a\n"
        + _png_chunk(b"IHDR", header)
        + _png_chunk(b"IDAT", compressed)
        + _png_chunk(b"IEND", b"")
    )


def encode_pgm(coverage: np.ndarray) -> bytes:
    if coverage.ndim != 2 or coverage.dtype != np.uint8:
        raise ValueError("PGM input must be a two-dimensional uint8 array")
    height, width = coverage.shape
    return f"P5\n{width} {height}\n255\n".encode("ascii") + coverage.tobytes()


def write_atlas_png(path: str | Path, result: AtlasResult) -> None:
    atomic_write_bytes(path, encode_rgba_png(result.rgba))


def write_coverage_pgm(path: str | Path, coverage: np.ndarray) -> None:
    atomic_write_bytes(path, encode_pgm(coverage))
