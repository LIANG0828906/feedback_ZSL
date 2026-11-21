"""Tile seam validation against a whole-screen reference render."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import AppError, ErrorCode
from .render import RenderArtifact, render_reference


@dataclass(slots=True)
class SeamCheck:
    ok: bool
    checked_pixels: int
    max_color_delta: float
    max_transmittance_delta: float
    first_mismatch: dict[str, Any] | None
    reference_color: np.ndarray
    reference_transmittance: np.ndarray


def stage_map_for_artifact(artifact: RenderArtifact) -> np.ndarray:
    stage_map = np.zeros(
        (artifact.scene.height, artifact.scene.width), dtype=np.int32
    )
    for result in artifact.tiles:
        tile = result.tile
        stage_map[tile.y0 : tile.y1, tile.x0 : tile.x1] = result.stage_index
    return stage_map


def _boundary_mask(scene_width: int, scene_height: int, artifact: RenderArtifact) -> np.ndarray:
    mask = np.zeros((scene_height, scene_width), dtype=bool)
    for result in artifact.tiles:
        tile = result.tile
        if tile.x1 < scene_width:
            mask[tile.y0 : tile.y1, tile.x1 - 1] = True
            mask[tile.y0 : tile.y1, tile.x1] = True
        if tile.y1 < scene_height:
            mask[tile.y1 - 1, tile.x0 : tile.x1] = True
            mask[tile.y1, tile.x0 : tile.x1] = True
    return mask


def check_seams(
    artifact: RenderArtifact,
    *,
    raise_on_error: bool = True,
) -> SeamCheck:
    stage_map = stage_map_for_artifact(artifact)
    ref_color, ref_transmittance, _ = render_reference(
        artifact.scene,
        artifact.config,
        stage_map,
        artifact.precision,
    )
    color_delta = np.abs(artifact.color - ref_color)
    transmittance_delta = np.abs(
        artifact.transmittance - ref_transmittance
    )
    mask = _boundary_mask(
        artifact.scene.width, artifact.scene.height, artifact
    )
    if not np.any(mask):
        mask[:] = True
    color_scalar = np.max(color_delta, axis=2)
    max_color = float(np.max(color_scalar[mask]))
    max_transmittance = float(np.max(transmittance_delta[mask]))
    violation = (
        (color_scalar[mask] > artifact.config.seam_tolerance)
        | (transmittance_delta[mask] > artifact.config.seam_tolerance)
    )
    first_mismatch: dict[str, Any] | None = None
    if np.any(violation):
        coordinates = np.argwhere(mask & (
            (color_scalar > artifact.config.seam_tolerance)
            | (transmittance_delta > artifact.config.seam_tolerance)
        ))
        y, x = int(coordinates[0][0]), int(coordinates[0][1])
        first_mismatch = {
            "x": x,
            "y": y,
            "tiled_color": [
                float(value) for value in artifact.color[y, x]
            ],
            "reference_color": [float(value) for value in ref_color[y, x]],
            "color_delta": [
                float(value) for value in color_delta[y, x]
            ],
            "tiled_transmittance": float(artifact.transmittance[y, x]),
            "reference_transmittance": float(ref_transmittance[y, x]),
            "transmittance_delta": float(
                transmittance_delta[y, x]
            ),
            "neighbor_side_values": _neighbor_values(
                artifact.color, x, y
            ),
        }
    result = SeamCheck(
        ok=not bool(np.any(violation)),
        checked_pixels=int(np.count_nonzero(mask)),
        max_color_delta=max_color,
        max_transmittance_delta=max_transmittance,
        first_mismatch=first_mismatch,
        reference_color=ref_color,
        reference_transmittance=ref_transmittance,
    )
    if raise_on_error and not result.ok:
        assert first_mismatch is not None
        raise AppError(
            ErrorCode.E_SEAM_UNRESOLVED,
            "tile boundary accumulation differs from whole-screen reference",
            field="seam",
            details={
                **first_mismatch,
                "tolerance": artifact.config.seam_tolerance,
                "checked_pixels": result.checked_pixels,
            },
        )
    return result


def _neighbor_values(
    color: np.ndarray, x: int, y: int
) -> dict[str, list[float] | None]:
    height, width, _ = color.shape

    def value_at(cx: int, cy: int) -> list[float]:
        return [float(component) for component in color[cy, cx]]

    return {
        "left": value_at(x - 1, y) if x > 0 else None,
        "right": value_at(x + 1, y) if x + 1 < width else None,
        "top": value_at(x, y - 1) if y > 0 else None,
        "bottom": value_at(x, y + 1) if y + 1 < height else None,
    }
