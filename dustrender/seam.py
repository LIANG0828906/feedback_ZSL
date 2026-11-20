"""Chunk-to-truth seam validation and localization."""

from __future__ import annotations

from typing import Any

import numpy as np

from .errors import E_SEAM_UNRESOLVED, DustRenderError
from .renderer import ChunkResult


def compare_seams(
    scene: object,
    chunks: list[Any],
    results: dict[str, ChunkResult],
    truth: ChunkResult,
    tolerance: float,
) -> tuple[dict[str, Any], DustRenderError | None]:
    violations: list[dict[str, Any]] = []
    max_scatter = 0.0
    max_trans = 0.0
    pairs: set[tuple[str, str]] = set()
    for chunk in chunks:
        result = results.get(chunk.id)
        if result is None:
            continue
        scatter = np.abs(
            result.rgb.astype(np.float64)
            - truth.rgb[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1].astype(np.float64)
        )
        trans = np.abs(
            result.transmittance.astype(np.float64)
            - truth.transmittance[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1].astype(
                np.float64
            )
        )
        max_scatter = max(max_scatter, float(np.max(scatter)))
        max_trans = max(max_trans, float(np.max(trans)))
        pixel_error = np.max(scatter, axis=2)
        for local_y, local_x in np.argwhere(pixel_error > tolerance):
            y = chunk.y0 + int(local_y)
            x = chunk.x0 + int(local_x)
            neighbor = _neighbor_chunk(chunks, chunk, x, y)
            if neighbor is not None:
                pairs.add(tuple(sorted((chunk.id, neighbor.id))))
            violations.append(
                {
                    "x": x,
                    "y": y,
                    "chunk": chunk.id,
                    "chunk_pair": [chunk.id, neighbor.id] if neighbor else [chunk.id],
                    "scattering_error": float(pixel_error[local_y, local_x]),
                    "transmittance_error": float(trans[local_y, local_x]),
                }
            )
            if len(violations) >= 256:
                break
    doc = {
        "status": "passed" if not violations else "failed",
        "tolerance": tolerance,
        "violation_count": len(violations),
        "max_scattering_abs_error": max_scatter,
        "max_transmittance_abs_error": max_trans,
        "chunk_pairs": [list(pair) for pair in sorted(pairs)],
        "seams": violations,
    }
    if not violations:
        return doc, None
    return doc, DustRenderError(
        E_SEAM_UNRESOLVED,
        "chunked render differs from single-chunk truth beyond tolerance",
        "$config.seam_tolerance",
        {
            "tolerance": tolerance,
            "violations": violations[:32],
            "chunk_pairs": doc["chunk_pairs"],
            "max_scattering_abs_error": max_scatter,
            "max_transmittance_abs_error": max_trans,
        },
    )


def _neighbor_chunk(chunks: list[Any], chunk: Any, x: int, y: int) -> Any | None:
    for candidate in chunks:
        if candidate.id == chunk.id:
            continue
        adjacent_x = candidate.x1 == chunk.x0 or candidate.x0 == chunk.x1
        adjacent_y = candidate.y1 == chunk.y0 or candidate.y0 == chunk.y1
        overlaps_x = not (candidate.x1 <= chunk.x0 or candidate.x0 >= chunk.x1)
        overlaps_y = not (candidate.y1 <= chunk.y0 or candidate.y0 >= chunk.y1)
        if (adjacent_x and overlaps_y) or (adjacent_y and overlaps_x):
            return candidate
    return None
