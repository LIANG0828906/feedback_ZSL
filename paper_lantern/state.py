from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .png import encode_png_rgba
from .render import TileResult
from .scene import Scene
from .util import sha256_array, sha256_bytes, stable_float


@dataclass
class RenderState:
    rgba: np.ndarray
    depth: np.ndarray
    layer_rgba: np.ndarray
    visible_fragments: np.ndarray
    paper_fragments: np.ndarray
    effective_transmission_sum: np.ndarray
    cutout_fragments: np.ndarray
    shadow_sample_rays: int = 0
    shadow_occluder_tests: int = 0
    completed_blocks: list[int] = field(default_factory=list)
    block_summaries: list[dict] = field(default_factory=list)
    recovery_count: int = 0


@dataclass
class OutputArtifacts:
    rgba_u8: np.ndarray
    depth_f32: np.ndarray
    layer_u8: np.ndarray
    png_bytes: bytes
    preview_png_bytes: bytes
    payloads: dict[str, bytes]
    digests: dict[str, str]


def empty_state(scene: Scene) -> RenderState:
    height = scene.image.height
    width = scene.image.width
    layer_count = len(scene.layers)
    return RenderState(
        rgba=np.zeros((height, width, 4), dtype=np.float64),
        depth=np.zeros((height, width), dtype=np.float64),
        layer_rgba=np.zeros((layer_count, height, width, 4), dtype=np.float64),
        visible_fragments=np.zeros(layer_count, dtype=np.int64),
        paper_fragments=np.zeros(layer_count, dtype=np.int64),
        effective_transmission_sum=np.zeros(layer_count, dtype=np.float64),
        cutout_fragments=np.zeros(layer_count, dtype=np.int64),
    )


def block_bounds(scene: Scene, block: int) -> tuple[int, int, int, int, int, int]:
    columns = scene.tile_columns
    column = block % columns
    row = block // columns
    x_start = column * scene.image.tile_width
    y_start = row * scene.image.tile_height
    x_end = min(x_start + scene.image.tile_width, scene.image.width)
    y_end = min(y_start + scene.image.tile_height, scene.image.height)
    return column, row, x_start, y_start, x_end, y_end


def summarize_block_arrays(
    scene: Scene,
    rgba: np.ndarray,
    depth: np.ndarray,
    block: int,
) -> dict:
    column, row, x_start, y_start, x_end, y_end = block_bounds(scene, block)
    rgba_tile = np.asarray(rgba[y_start:y_end, x_start:x_end], dtype="<f8")
    depth_tile = np.asarray(depth[y_start:y_end, x_start:x_end], dtype="<f8")
    return _summarize_tile(rgba_tile, depth_tile, block, column, row)


def _summarize_tile(
    rgba_tile: np.ndarray,
    depth_tile: np.ndarray,
    block: int,
    column: int,
    row: int,
) -> dict:
    rgba_tile = np.asarray(rgba_tile, dtype="<f8")
    depth_tile = np.asarray(depth_tile, dtype="<f8")
    mean_rgb = np.mean(rgba_tile[..., :3], axis=(0, 1))
    mean_alpha = float(np.mean(rgba_tile[..., 3]))
    return {
        "block": block,
        "column": column,
        "row": row,
        "rgba_sha256": sha256_array(rgba_tile),
        "depth_sha256": sha256_array(depth_tile),
        "mean_rgb": [stable_float(value) for value in mean_rgb],
        "mean_alpha": stable_float(mean_alpha),
    }


def apply_tile(
    state: RenderState,
    scene: Scene,
    block: int,
    column: int,
    row: int,
    tile: TileResult,
) -> dict:
    if block != len(state.completed_blocks):
        raise RuntimeError("blocks must be applied in deterministic row-major order")
    x_start = column * scene.image.tile_width
    y_start = row * scene.image.tile_height
    x_end = x_start + tile.rgba.shape[1]
    y_end = y_start + tile.rgba.shape[0]
    state.rgba[y_start:y_end, x_start:x_end] = tile.rgba
    state.depth[y_start:y_end, x_start:x_end] = tile.depth
    state.layer_rgba[:, y_start:y_end, x_start:x_end] = tile.layer_rgba
    state.visible_fragments += tile.visible_fragments
    state.paper_fragments += tile.paper_fragments
    state.effective_transmission_sum += tile.effective_transmission_sum
    state.cutout_fragments += tile.cutout_fragments
    state.shadow_sample_rays += int(tile.shadow_sample_rays)
    state.shadow_occluder_tests += int(tile.shadow_occluder_tests)
    summary = _summarize_tile(tile.rgba, tile.depth, block, column, row)
    state.completed_blocks.append(block)
    state.block_summaries.append(summary)
    return summary


def _quantize_premultiplied(rgba: np.ndarray) -> np.ndarray:
    clipped = np.clip(rgba, 0.0, 1.0)
    quantized = np.floor(clipped * 255.0 + 0.5).astype(np.uint8)
    quantized[..., :3] = np.minimum(quantized[..., :3], quantized[..., 3:4])
    return quantized


def _straight_rgba(premultiplied: np.ndarray) -> np.ndarray:
    alpha = premultiplied[..., 3:4].astype(np.float64)
    rgb = premultiplied[..., :3].astype(np.float64)
    straight_rgb = np.zeros_like(rgb)
    nonzero = alpha[..., 0] > 0
    straight_rgb[nonzero] = np.clip(
        rgb[nonzero] * 255.0 / alpha[nonzero], 0.0, 255.0
    )
    straight = np.concatenate(
        (
            np.floor(straight_rgb + 0.5),
            alpha,
        ),
        axis=2,
    ).astype(np.uint8)
    return straight


def build_output_artifacts(scene: Scene, state: RenderState) -> OutputArtifacts:
    rgba_u8 = _quantize_premultiplied(state.rgba)
    depth_f32 = np.asarray(state.depth, dtype="<f4")
    straight = _straight_rgba(rgba_u8)
    png_bytes = encode_png_rgba(straight)
    background = scene.image.background[None, None, :]
    preview = np.clip(
        state.rgba[..., :3] + background * (1.0 - state.rgba[..., 3:4]),
        0.0,
        1.0,
    )
    preview_u8 = np.concatenate(
        (
            np.floor(preview * 255.0 + 0.5),
            np.full(state.rgba.shape[:2] + (1,), 255.0),
        ),
        axis=2,
    ).astype(np.uint8)
    preview_png_bytes = encode_png_rgba(preview_u8)
    layer_u8 = _quantize_premultiplied(
        np.moveaxis(state.layer_rgba, 0, 0)
    )
    payloads: dict[str, bytes] = {
        "image.rgba": rgba_u8.tobytes(order="C"),
        "image.png": png_bytes,
        "preview.png": preview_png_bytes,
        "depth.f32": depth_f32.tobytes(order="C"),
    }
    for layer_position, layer in enumerate(scene.layers):
        payloads[f"layers/layer_{layer.layer_index:02d}.rgba"] = np.ascontiguousarray(
            layer_u8[layer_position]
        ).tobytes(order="C")
    digests = {
        name: sha256_bytes(payload)
        for name, payload in sorted(payloads.items())
    }
    return OutputArtifacts(
        rgba_u8=rgba_u8,
        depth_f32=depth_f32,
        layer_u8=layer_u8,
        png_bytes=png_bytes,
        preview_png_bytes=preview_png_bytes,
        payloads=payloads,
        digests=digests,
    )

