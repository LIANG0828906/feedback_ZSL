"""Tone posterization, outline extraction and final stylization stages."""

from __future__ import annotations

import numpy as np


def palette(level_count: int) -> np.ndarray:
    """A deterministic cool-shadow to warm-highlight palette."""
    anchors = np.asarray(
        [
            [0.05, 0.07, 0.14],
            [0.16, 0.20, 0.34],
            [0.31, 0.37, 0.46],
            [0.66, 0.55, 0.40],
            [0.94, 0.77, 0.51],
            [1.00, 0.93, 0.72],
        ],
        dtype=np.float64,
    )
    if level_count <= 1:
        return anchors[-1:].copy()
    positions = np.linspace(0.0, 1.0, level_count)
    source = np.linspace(0.0, 1.0, anchors.shape[0])
    return np.stack(
        [np.interp(positions, source, anchors[:, channel]) for channel in range(3)],
        axis=1,
    )


def brightness(rgb: np.ndarray) -> np.ndarray:
    return (
        rgb[..., 0].astype(np.float64) * 0.2126
        + rgb[..., 1].astype(np.float64) * 0.7152
        + rgb[..., 2].astype(np.float64) * 0.0722
    )


def posterize(rgb: np.ndarray, level_count: int) -> tuple[np.ndarray, np.ndarray]:
    if level_count < 2:
        raise ValueError("level_count must be >= 2")
    lum = brightness(rgb)
    scaled = np.clip(lum, 0.0, 1.0) * float(level_count)
    levels = np.floor(scaled).astype(np.uint8)
    levels = np.minimum(levels, np.uint8(level_count - 1))
    target = (levels.astype(np.float64) + 0.5) / float(level_count)
    ratio = target / np.maximum(lum, 1e-5)
    tint = palette(level_count)[levels]
    source = rgb.astype(np.float64)
    styled = source * (0.52 + 0.48 * np.clip(ratio, 0.28, 2.6))[..., None]
    styled = styled * 0.86 + tint * 0.14
    styled = np.clip(styled, 0.0, 1.0)
    return levels, (styled * 255.0 + 0.5).astype(np.uint8)


def outline_from_levels(levels: np.ndarray) -> np.ndarray:
    """1-pixel level-boundary outline; image borders never contribute."""
    mask = np.zeros(levels.shape, dtype=bool)
    if levels.shape[1] > 1:
        horizontal = levels[:, 1:] != levels[:, :-1]
        mask[:, 1:] |= horizontal
        mask[:, :-1] |= horizontal
    if levels.shape[0] > 1:
        vertical = levels[1:, :] != levels[:-1, :]
        mask[1:, :] |= vertical
        mask[:-1, :] |= vertical
    return mask


def outline_from_padded_levels(levels: np.ndarray) -> np.ndarray:
    """Outline for a tile with a one-pixel halo, returned without the halo."""
    if levels.ndim != 2 or levels.shape[0] < 3 or levels.shape[1] < 3:
        return outline_from_levels(levels)
    mask = outline_from_levels(levels)
    return mask[1:-1, 1:-1]


def level_preview(levels: np.ndarray, level_count: int) -> np.ndarray:
    colors = (palette(level_count) * 255.0 + 0.5).astype(np.uint8)
    return colors[levels]


def outline_preview(outline: np.ndarray) -> np.ndarray:
    out = np.full((*outline.shape, 3), 18, dtype=np.uint8)
    out[outline] = np.asarray([244, 238, 220], dtype=np.uint8)
    return out


def coverage_preview(coverage: np.ndarray) -> np.ndarray:
    out = np.zeros((*coverage.shape, 3), dtype=np.uint8)
    out[coverage] = np.asarray([255, 183, 65], dtype=np.uint8)
    return out


def compose_style(
    poster_rgb: np.ndarray,
    brush_rgba: np.ndarray,
    outline: np.ndarray,
) -> np.ndarray:
    base = poster_rgb.astype(np.float64)
    brush_rgb = brush_rgba[..., :3].astype(np.float64)
    alpha = (brush_rgba[..., 3].astype(np.float64) / 255.0) * 0.72
    final = base * (1.0 - alpha[..., None]) + brush_rgb * alpha[..., None]
    ink = np.asarray([18.0, 15.0, 22.0], dtype=np.float64)
    final[outline] = final[outline] * 0.12 + ink * 0.88
    return (np.clip(final, 0.0, 255.0) + 0.5).astype(np.uint8)

