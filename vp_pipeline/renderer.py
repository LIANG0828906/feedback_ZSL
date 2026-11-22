"""Deterministic frame compositing in linear light."""

from __future__ import annotations

import numpy as np

from .color import linear_to_srgb, quantize_rgba
from .errors import E_PARA, PipelineError
from .frame import FrameResult
from .model import Layer, SceneModel
from .version import ALGORITHM_VERSION

_LUMINANCE_WEIGHTS = np.asarray([0.2126, 0.7152, 0.0722], dtype=np.float64)


def _validate_frame_index(model: SceneModel, frame_index: int) -> None:
    if isinstance(frame_index, bool) or not isinstance(frame_index, int):
        raise PipelineError(
            E_PARA, "frame_index must be an integer", field_path="frame_index"
        )
    if frame_index < 0 or frame_index >= model.timeline.frames:
        raise PipelineError(
            E_PARA,
            "frame_index is outside the timeline",
            field_path="frame_index",
            frame_index=frame_index,
            frame_count=model.timeline.frames,
        )


def _layer_region(model: SceneModel, layer: Layer) -> tuple[slice, slice]:
    x0 = max(0, layer.bounds.x)
    y0 = max(0, layer.bounds.y)
    x1 = min(model.canvas.width, layer.bounds.x + layer.bounds.width)
    y1 = min(model.canvas.height, layer.bounds.y + layer.bounds.height)
    if x1 <= x0 or y1 <= y0:
        return slice(0, 0), slice(0, 0)
    return slice(y0, y1), slice(x0, x1)


def render_frame(model: SceneModel, frame_index: int) -> FrameResult:
    _validate_frame_index(model, frame_index)
    height = model.canvas.height
    width = model.canvas.width
    linear_premultiplied = np.zeros((height, width, 4), dtype=np.float64)
    attribution = np.full((height, width), -1, dtype=np.int16)
    visible_layers = model.visible_layers(frame_index)
    for layer in visible_layers:
        region_y, region_x = _layer_region(model, layer)
        if region_y.stop <= region_y.start or region_x.stop <= region_x.start:
            continue
        source_alpha = float(np.clip(layer.alpha * layer.opacity, 0.0, 1.0))
        if source_alpha <= 0.0:
            continue
        source_rgb = np.asarray(layer.color_linear, dtype=np.float64) * source_alpha
        inverse_alpha = 1.0 - source_alpha
        destination = linear_premultiplied[region_y, region_x]
        destination[..., :3] = source_rgb[None, None, :] + destination[..., :3] * inverse_alpha
        destination[..., 3] = source_alpha + destination[..., 3] * inverse_alpha
        attribution[region_y, region_x] = layer.stack_order

    alpha = linear_premultiplied[..., 3]
    straight_rgb = np.zeros((height, width, 3), dtype=np.float64)
    non_transparent = alpha > 1e-15
    straight_rgb[non_transparent] = (
        linear_premultiplied[non_transparent, :3]
        / alpha[non_transparent, None]
    )
    encoded_rgb = linear_to_srgb(straight_rgb)
    rgba = np.empty((height, width, 4), dtype=np.float64)
    rgba[..., :3] = encoded_rgb
    rgba[..., 3] = alpha
    pixels = quantize_rgba(rgba)
    if model.canvas.channels != 4:
        raise PipelineError(
            E_PARA,
            "Output channel count is derived from the internal model",
            field_path="canvas.channels",
            channels=model.canvas.channels,
        )
    premultiplied_rgb = linear_premultiplied[..., :3]
    luminance = np.sum(premultiplied_rgb * _LUMINANCE_WEIGHTS, axis=2)
    stats = FrameResult(
        frame_index=frame_index,
        width=width,
        height=height,
        channels=model.canvas.channels,
        fps_num=model.timeline.fps_num,
        fps_den=model.timeline.fps_den,
        pixels=np.ascontiguousarray(pixels),
        mean_alpha=float(np.mean(alpha)),
        mean_linear_luminance=float(np.mean(luminance)),
        opaque_pixels=int(np.count_nonzero(pixels[..., 3] == 255)),
        visible_layer_ids=tuple(layer.layer_id for layer in visible_layers),
        attribution=attribution,
    )
    return stats


def render_window(
    model: SceneModel,
    window_index: int,
    frame_hook: object | None = None,
) -> tuple[FrameResult, ...]:
    ranges = model.window_ranges()
    if window_index < 0 or window_index >= len(ranges):
        raise PipelineError(
            E_PARA,
            "window_index is outside the timeline",
            field_path="window_index",
            window_index=window_index,
        )
    _, start, end = ranges[window_index]
    frames = []
    for frame_index in range(start, end):
        if frame_hook is not None:
            frame_hook(frame_index)
        frames.append(render_frame(model, frame_index))
    return tuple(frames)


def render_all_frames(model: SceneModel) -> dict[int, FrameResult]:
    model.validate()
    return {
        frame_index: render_frame(model, frame_index)
        for frame_index in range(model.timeline.frames)
    }
