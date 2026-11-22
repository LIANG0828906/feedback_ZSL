"""Unified loaders for the three supported foreground-composite formats."""

from __future__ import annotations

import json
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Any

import numpy as np

from .color import srgb_to_linear
from .errors import E_DECODE, E_PARA, E_SCHEMA, E_VER, PipelineError
from .model import Canvas, Layer, Rect, SceneModel, Timeline
from .serialization import bytes_sha256

SUPPORTED_VERSIONS = ("vpfc/1", "vpfc/2", "vpfc/3")


@dataclass(frozen=True)
class LoadResult:
    model: SceneModel
    mapping_trace: tuple[dict[str, str], ...]
    warnings: tuple[dict[str, Any], ...] = ()


def _entry(source_path: str, internal_path: str, transform: str) -> dict[str, str]:
    return {
        "source_path": source_path,
        "internal_path": internal_path,
        "transform": transform,
    }


def _read_json(path: Path) -> tuple[dict[str, Any], str]:
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise PipelineError(
            E_DECODE, f"Cannot read sample: {exc}", field_path=str(path)
        ) from exc
    try:
        value = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PipelineError(
            E_DECODE,
            f"JSON decoding failed: {exc}",
            field_path="$",
        ) from exc
    if not isinstance(value, dict):
        raise PipelineError(E_DECODE, "Sample root must be an object", field_path="$")
    return value, bytes_sha256(raw)


def _require(mapping: dict[str, Any], key: str, path: str) -> Any:
    if key not in mapping:
        raise PipelineError(
            E_SCHEMA, f"Required field {path} is missing", field_path=path
        )
    return mapping[key]


def _as_object(value: Any, path: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise PipelineError(E_SCHEMA, "Expected an object", field_path=path)
    return value


def _as_list(value: Any, path: str) -> list[Any]:
    if not isinstance(value, list):
        raise PipelineError(E_SCHEMA, "Expected an array", field_path=path)
    return value


def _as_str(value: Any, path: str) -> str:
    if not isinstance(value, str) or not value:
        raise PipelineError(E_SCHEMA, "Expected a non-empty string", field_path=path)
    return value


def _as_bool(value: Any, path: str) -> bool:
    if not isinstance(value, bool):
        raise PipelineError(E_SCHEMA, "Expected a boolean", field_path=path)
    return value


def _as_int(value: Any, path: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise PipelineError(E_SCHEMA, "Expected an integer", field_path=path)
    return value


def _as_float(value: Any, path: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise PipelineError(E_SCHEMA, "Expected a number", field_path=path)
    number = float(value)
    if not np.isfinite(number):
        raise PipelineError(E_SCHEMA, "Expected a finite number", field_path=path)
    return number


def _as_ranges(values: Any, length: int, path: str) -> list[float]:
    items = _as_list(values, path)
    if len(items) != length:
        raise PipelineError(
            E_SCHEMA, f"Expected {length} values", field_path=path
        )
    return [_as_float(item, f"{path}[{index}]") for index, item in enumerate(items)]


def _unit_rgba(values: Any, path: str) -> tuple[float, float, float, float]:
    red, green, blue, alpha = _as_ranges(values, 4, path)
    for index, value in enumerate((red, green, blue, alpha)):
        if not 0.0 <= value <= 1.0:
            raise PipelineError(
                E_SCHEMA,
                "RGBA values must be within [0, 1]",
                field_path=f"{path}[{index}]",
            )
    return red, green, blue, alpha


def _decode_rgba(
    rgba: tuple[float, float, float, float],
    transfer: str,
    alpha_mode: str,
    path: str,
) -> tuple[tuple[float, float, float], float]:
    if transfer not in {"srgb", "linear"}:
        raise PipelineError(
            E_SCHEMA,
            f"Unsupported transfer function {transfer!r}",
            field_path=path,
        )
    if alpha_mode not in {"straight", "premultiplied"}:
        raise PipelineError(
            E_SCHEMA,
            f"Cannot determine alpha convention from {alpha_mode!r}",
            field_path=path,
        )
    red, green, blue, alpha = rgba
    linear = (
        tuple(float(value) for value in srgb_to_linear(np.asarray((red, green, blue))))
        if transfer == "srgb"
        else (red, green, blue)
    )
    if alpha_mode == "premultiplied":
        if alpha <= 0.0:
            if max(linear) > 1e-12:
                raise PipelineError(
                    E_SCHEMA,
                    "Transparent premultiplied pixels must have zero RGB",
                    field_path=path,
                )
            return (0.0, 0.0, 0.0), alpha
        if any(channel > alpha + 1e-9 for channel in linear):
            raise PipelineError(
                E_SCHEMA,
                "Premultiplied RGB exceeds alpha",
                field_path=path,
            )
        return (
            min(1.0, linear[0] / alpha),
            min(1.0, linear[1] / alpha),
            min(1.0, linear[2] / alpha),
        ), alpha
    return linear, alpha


def _detect_version(document: dict[str, Any]) -> tuple[str, str]:
    candidates = (
        ("format", "version"),
        ("schema", "revision"),
        ("document", "revision"),
    )
    raw_version: Any = None
    version_path = "format.version"
    found = False
    for outer, inner in candidates:
        outer_value = document.get(outer)
        if isinstance(outer_value, dict) and inner in outer_value:
            raw_version = outer_value[inner]
            version_path = f"{outer}.{inner}"
            found = True
            break
    if not found:
        raise PipelineError(
            E_SCHEMA,
            "Required format version field is missing",
            field_path="format.version",
        )
    if isinstance(raw_version, bool):
        normalized = ""
    else:
        normalized = str(raw_version)
    aliases = {
        "vpfc/1": "vpfc/1",
        "vpfc/2": "vpfc/2",
        "vpfc/3": "vpfc/3",
        "1": "vpfc/1",
        "2": "vpfc/2",
        "3": "vpfc/3",
    }
    version = aliases.get(normalized)
    if version is None:
        raise PipelineError(
            E_VER,
            f"Unknown foreground-composite version {raw_version!r}",
            field_path=version_path,
            supported=list(SUPPORTED_VERSIONS),
        )
    return version, version_path


def _parse_canvas(document: dict[str, Any], version: str) -> Canvas:
    if version == "vpfc/1":
        canvas = _as_object(_require(document, "canvas", "canvas"), "canvas")
        width = _as_int(_require(canvas, "width", "canvas.width"), "canvas.width")
        height = _as_int(_require(canvas, "height", "canvas.height"), "canvas.height")
        channels = _as_int(
            _require(canvas, "channels", "canvas.channels"), "canvas.channels"
        )
        return Canvas(width, height, channels)
    if version == "vpfc/2":
        return Canvas(
            _as_int(
                _require(document, "canvasWidth", "canvasWidth"), "canvasWidth"
            ),
            _as_int(
                _require(document, "canvasHeight", "canvasHeight"), "canvasHeight"
            ),
            _as_int(
                _require(document, "channelCount", "channelCount"), "channelCount"
            ),
        )
    canvas = _as_object(_require(document, "canvas", "canvas"), "canvas")
    return Canvas(
        _as_int(_require(canvas, "width", "canvas.width"), "canvas.width"),
        _as_int(_require(canvas, "height", "canvas.height"), "canvas.height"),
        _as_int(_require(canvas, "channels", "canvas.channels"), "canvas.channels"),
    )


def _parse_timeline(document: dict[str, Any], version: str) -> Timeline:
    if version == "vpfc/1":
        timeline = _as_object(
            _require(document, "timeline", "timeline"), "timeline"
        )
        frames = _as_int(
            _require(timeline, "frame_count", "timeline.frame_count"),
            "timeline.frame_count",
        )
        window_size = _as_int(
            _require(timeline, "window_size", "timeline.window_size"),
            "timeline.window_size",
        )
        time_base = _as_object(
            _require(timeline, "time_base", "timeline.time_base"),
            "timeline.time_base",
        )
        unit = _as_str(
            _require(time_base, "unit", "timeline.time_base.unit"),
            "timeline.time_base.unit",
        )
        fps = _as_object(
            _require(time_base, "fps", "timeline.time_base.fps"),
            "timeline.time_base.fps",
        )
        numerator = _as_int(
            _require(fps, "num", "timeline.time_base.fps.num"),
            "timeline.time_base.fps.num",
        )
        denominator = _as_int(
            _require(fps, "den", "timeline.time_base.fps.den"),
            "timeline.time_base.fps.den",
        )
        return Timeline(
            frames,
            window_size,
            numerator,
            denominator,
            unit,
            {"unit": unit, "fps": {"num": numerator, "den": denominator}},
        )
    if version == "vpfc/2":
        frames = _as_int(
            _require(document, "frameCount", "frameCount"), "frameCount"
        )
        window_size = _as_int(
            _require(document, "windowSize", "windowSize"), "windowSize"
        )
        frame_rate = _as_object(
            _require(document, "frameRate", "frameRate"), "frameRate"
        )
        rate_num = _as_int(
            _require(frame_rate, "num", "frameRate.num"), "frameRate.num"
        )
        rate_den = _as_int(
            _require(frame_rate, "den", "frameRate.den"), "frameRate.den"
        )
        time_base = _as_object(
            _require(document, "timeBase", "timeBase"), "timeBase"
        )
        unit = _as_str(
            _require(time_base, "unit", "timeBase.unit"), "timeBase.unit"
        )
        ticks_per_second = _as_int(
            _require(
                time_base, "ticksPerSecond", "timeBase.ticksPerSecond"
            ),
            "timeBase.ticksPerSecond",
        )
        ticks_per_frame = _as_int(
            _require(time_base, "ticksPerFrame", "timeBase.ticksPerFrame"),
            "timeBase.ticksPerFrame",
        )
        if ticks_per_frame <= 0 or ticks_per_second <= 0:
            raise PipelineError(
                E_SCHEMA,
                "Tick rates must be positive",
                field_path="timeBase",
            )
        tick_rate = Fraction(ticks_per_second, ticks_per_frame)
        declared_rate = Fraction(rate_num, rate_den)
        if tick_rate != declared_rate:
            raise PipelineError(
                E_SCHEMA,
                "frameRate and tick-based time base disagree",
                field_path="frameRate",
                tick_fps=f"{tick_rate.numerator}/{tick_rate.denominator}",
                declared_fps=f"{declared_rate.numerator}/{declared_rate.denominator}",
            )
        return Timeline(
            frames,
            window_size,
            rate_num,
            rate_den,
            unit,
            {
                "unit": unit,
                "ticksPerSecond": ticks_per_second,
                "ticksPerFrame": ticks_per_frame,
                "frameRate": {"num": rate_num, "den": rate_den},
            },
        )
    temporal = _as_object(
        _require(document, "temporal", "temporal"), "temporal"
    )
    frames = _as_int(
        _require(temporal, "frameCount", "temporal.frameCount"),
        "temporal.frameCount",
    )
    window_size = _as_int(
        _require(temporal, "windowSize", "temporal.windowSize"),
        "temporal.windowSize",
    )
    period = _as_object(
        _require(
            temporal,
            "framePeriodSeconds",
            "temporal.framePeriodSeconds",
        ),
        "temporal.framePeriodSeconds",
    )
    period_num = _as_int(
        _require(period, "num", "temporal.framePeriodSeconds.num"),
        "temporal.framePeriodSeconds.num",
    )
    period_den = _as_int(
        _require(period, "den", "temporal.framePeriodSeconds.den"),
        "temporal.framePeriodSeconds.den",
    )
    if period_num <= 0 or period_den <= 0:
        raise PipelineError(
            E_SCHEMA,
            "Frame period must be positive",
            field_path="temporal.framePeriodSeconds",
        )
    rate = Fraction(period_den, period_num)
    return Timeline(
        frames,
        window_size,
        rate.numerator,
        rate.denominator,
        "seconds",
        {"unit": "seconds", "framePeriodSeconds": {"num": period_num, "den": period_den}},
    )


def _base_mapping(version: str, version_path: str) -> list[dict[str, str]]:
    trace = [_entry(version_path, "source_version", "version_token_to_canonical")]
    if version == "vpfc/1":
        trace.extend(
            [
                _entry("canvas.width", "canvas.width", "identity"),
                _entry("canvas.height", "canvas.height", "identity"),
                _entry("canvas.channels", "canvas.channels", "identity"),
                _entry("timeline.frame_count", "timeline.frames", "identity"),
                _entry("timeline.window_size", "timeline.window_size", "identity"),
                _entry(
                    "timeline.time_base.fps.num", "timeline.fps_num", "identity"
                ),
                _entry(
                    "timeline.time_base.fps.den", "timeline.fps_den", "identity"
                ),
                _entry(
                    "timeline.time_base.unit",
                    "timeline.source_time_unit",
                    "identity",
                ),
            ]
        )
    elif version == "vpfc/2":
        trace.extend(
            [
                _entry("canvasWidth", "canvas.width", "identity"),
                _entry("canvasHeight", "canvas.height", "identity"),
                _entry("channelCount", "canvas.channels", "identity"),
                _entry("frameCount", "timeline.frames", "identity"),
                _entry("windowSize", "timeline.window_size", "identity"),
                _entry("frameRate.num", "timeline.fps_num", "identity"),
                _entry("frameRate.den", "timeline.fps_den", "identity"),
                _entry("timeBase.unit", "timeline.source_time_unit", "identity"),
                _entry(
                    "timeBase.ticksPerFrame",
                    "timeline.source_time_basis.ticksPerFrame",
                    "identity",
                ),
                _entry(
                    "timeBase.ticksPerSecond",
                    "timeline.source_time_basis.ticksPerSecond",
                    "identity",
                ),
            ]
        )
    else:
        trace.extend(
            [
                _entry("canvas.width", "canvas.width", "identity"),
                _entry("canvas.height", "canvas.height", "identity"),
                _entry("canvas.channels", "canvas.channels", "identity"),
                _entry("temporal.frameCount", "timeline.frames", "identity"),
                _entry("temporal.windowSize", "timeline.window_size", "identity"),
                _entry(
                    "temporal.framePeriodSeconds.num",
                    "timeline.fps_den",
                    "period_numerator_to_denominator",
                ),
                _entry(
                    "temporal.framePeriodSeconds.den",
                    "timeline.fps_num",
                    "period_denominator_to_numerator",
                ),
                _entry(
                    "temporal.framePeriodSeconds",
                    "timeline.source_time_unit",
                    "seconds_unit",
                ),
            ]
        )
    return trace


def _parse_v1(
    document: dict[str, Any], sha: str, version_path: str
) -> LoadResult:
    canvas = _parse_canvas(document, "vpfc/1")
    timeline = _parse_timeline(document, "vpfc/1")
    scene_id = _as_str(_require(document, "sceneId", "sceneId"), "sceneId")
    color_space = _as_str(
        _require(document, "color_space", "color_space"), "color_space"
    )
    if color_space != "srgb":
        raise PipelineError(
            E_SCHEMA,
            "vpfc/1 only defines an sRGB encoded store",
            field_path="color_space",
        )
    alpha_mode = _as_str(
        _require(document, "alpha_mode", "alpha_mode"), "alpha_mode"
    )
    if alpha_mode != "premultiplied":
        raise PipelineError(
            E_SCHEMA,
            "vpfc/1 global alpha semantics are fixed to premultiplied",
            field_path="alpha_mode",
        )
    raw_layers = _as_list(_require(document, "layers", "layers"), "layers")
    layers: list[Layer] = []
    trace = _base_mapping("vpfc/1", version_path)
    trace.extend(
        [
            _entry("sceneId", "scene_id", "identity"),
            _entry("alpha_mode", "layers[*].alpha_mode", "broadcast"),
            _entry("color_space", "layers[*].source_color_space", "broadcast"),
        ]
    )
    for index, raw in enumerate(raw_layers):
        path = f"layers[{index}]"
        item = _as_object(raw, path)
        layer_id = _as_str(_require(item, "id", f"{path}.id"), f"{path}.id")
        kind = _as_str(
            _require(item, "type", f"{path}.type"), f"{path}.type"
        )
        name = _as_str(
            _require(item, "name", f"{path}.name"), f"{path}.name"
        )
        order = _as_int(
            _require(item, "order", f"{path}.order"), f"{path}.order"
        )
        visible = _as_bool(
            _require(item, "visible", f"{path}.visible"), f"{path}.visible"
        )
        visible_from = _as_int(
            _require(item, "visible_from", f"{path}.visible_from"),
            f"{path}.visible_from",
        )
        visible_to = _as_int(
            _require(item, "visible_until", f"{path}.visible_until"),
            f"{path}.visible_until",
        )
        bounds_obj = _as_object(
            _require(item, "bounds", f"{path}.bounds"), f"{path}.bounds"
        )
        bounds = Rect(
            _as_int(_require(bounds_obj, "x", f"{path}.bounds.x"), f"{path}.bounds.x"),
            _as_int(_require(bounds_obj, "y", f"{path}.bounds.y"), f"{path}.bounds.y"),
            _as_int(
                _require(bounds_obj, "w", f"{path}.bounds.w"), f"{path}.bounds.w"
            ),
            _as_int(
                _require(bounds_obj, "h", f"{path}.bounds.h"), f"{path}.bounds.h"
            ),
        )
        rgba = _unit_rgba(
            _require(
                item,
                "rgba_srgb_premultiplied",
                f"{path}.rgba_srgb_premultiplied",
            ),
            f"{path}.rgba_srgb_premultiplied",
        )
        linear, alpha = _decode_rgba(
            rgba,
            "srgb",
            "premultiplied",
            f"{path}.rgba_srgb_premultiplied",
        )
        opacity = _as_float(
            _require(item, "opacity", f"{path}.opacity"), f"{path}.opacity"
        )
        layers.append(
            Layer(
                layer_id=layer_id,
                kind=kind,
                name=name,
                stack_order=order,
                parent_id=None,
                color_linear=linear,
                alpha=alpha,
                opacity=opacity,
                visible=visible,
                visible_from=visible_from,
                visible_to=visible_to,
                alpha_mode="premultiplied",
                source_color_space="srgb",
                bounds=bounds,
                source_version="vpfc/1",
            )
        )
        trace.extend(
            [
                _entry(f"{path}.id", f"layers[{index}].layer_id", "identity"),
                _entry(f"{path}.type", f"layers[{index}].kind", "identity"),
                _entry(f"{path}.name", f"layers[{index}].name", "identity"),
                _entry(f"{path}.order", f"layers[{index}].stack_order", "identity"),
                _entry(f"{path}.visible", f"layers[{index}].visible", "identity"),
                _entry(
                    f"{path}.visible_from",
                    f"layers[{index}].visible_from",
                    "identity",
                ),
                _entry(
                    f"{path}.visible_until",
                    f"layers[{index}].visible_to",
                    "exclusive_until_to_to",
                ),
                _entry(
                    f"{path}.bounds",
                    f"layers[{index}].bounds",
                    "object_to_rect",
                ),
                _entry(
                    f"{path}.rgba_srgb_premultiplied",
                    f"layers[{index}].color_linear",
                    "srgb_premultiplied_to_linear_straight",
                ),
                _entry(
                    f"{path}.rgba_srgb_premultiplied[3]",
                    f"layers[{index}].alpha",
                    "channel_index_3_to_alpha",
                ),
                _entry(f"{path}.opacity", f"layers[{index}].opacity", "identity"),
            ]
        )
    layers.sort(key=lambda layer: layer.stack_order)
    model = SceneModel(
        scene_id=scene_id,
        source_version="vpfc/1",
        canvas=canvas,
        timeline=timeline,
        layers=tuple(layers),
        source_document_sha256=sha,
        mapping_trace=tuple(trace),
    )
    _validate_loaded_model(model)
    return LoadResult(model, tuple(trace))


def _parse_v2(
    document: dict[str, Any], sha: str, version_path: str
) -> LoadResult:
    canvas = _parse_canvas(document, "vpfc/2")
    timeline = _parse_timeline(document, "vpfc/2")
    scene_id = _as_str(_require(document, "sceneId", "sceneId"), "sceneId")
    raw_layers = _as_list(_require(document, "layers", "layers"), "layers")
    time_base = _as_object(
        _require(document, "timeBase", "timeBase"), "timeBase"
    )
    ticks_per_frame = _as_int(
        _require(time_base, "ticksPerFrame", "timeBase.ticksPerFrame"),
        "timeBase.ticksPerFrame",
    )
    layers: list[Layer] = []
    trace = _base_mapping("vpfc/2", version_path)
    trace.append(_entry("sceneId", "scene_id", "identity"))
    seen_orders: set[int] = set()
    for index, raw in enumerate(raw_layers):
        path = f"layers[{index}]"
        item = _as_object(raw, path)
        layer_id = _as_str(
            _require(item, "layerId", f"{path}.layerId"), f"{path}.layerId"
        )
        kind = _as_str(
            _require(item, "layerType", f"{path}.layerType"),
            f"{path}.layerType",
        )
        name = _as_str(
            _require(item, "displayName", f"{path}.displayName"),
            f"{path}.displayName",
        )
        stack_order = _as_int(
            _require(item, "zIndex", f"{path}.zIndex"), f"{path}.zIndex"
        )
        if stack_order in seen_orders:
            raise PipelineError(
                E_SCHEMA,
                "zIndex values must be unique",
                field_path=f"{path}.zIndex",
            )
        seen_orders.add(stack_order)
        parent_raw = _require(item, "parentId", f"{path}.parentId")
        parent_id = (
            None
            if parent_raw is None
            else _as_str(parent_raw, f"{path}.parentId")
        )
        visibility = _as_object(
            _require(item, "visibility", f"{path}.visibility"),
            f"{path}.visibility",
        )
        start_tick = _as_int(
            _require(
                visibility,
                "startTick",
                f"{path}.visibility.startTick",
            ),
            f"{path}.visibility.startTick",
        )
        end_tick = _as_int(
            _require(
                visibility,
                "endTickExclusive",
                f"{path}.visibility.endTickExclusive",
            ),
            f"{path}.visibility.endTickExclusive",
        )
        if (
            start_tick % ticks_per_frame != 0
            or end_tick % ticks_per_frame != 0
        ):
            raise PipelineError(
                E_SCHEMA,
                "Visibility ticks must align to whole frames",
                field_path=f"{path}.visibility",
            )
        visible_from = start_tick // ticks_per_frame
        visible_to = end_tick // ticks_per_frame
        bounds_obj = _as_object(
            _require(item, "bounds", f"{path}.bounds"), f"{path}.bounds"
        )
        bounds = Rect(
            _as_int(
                _require(bounds_obj, "left", f"{path}.bounds.left"),
                f"{path}.bounds.left",
            ),
            _as_int(
                _require(bounds_obj, "top", f"{path}.bounds.top"),
                f"{path}.bounds.top",
            ),
            _as_int(
                _require(bounds_obj, "width", f"{path}.bounds.width"),
                f"{path}.bounds.width",
            ),
            _as_int(
                _require(bounds_obj, "height", f"{path}.bounds.height"),
                f"{path}.bounds.height",
            ),
        )
        rgba_obj = _as_object(
            _require(item, "rgba", f"{path}.rgba"), f"{path}.rgba"
        )
        transfer = _as_str(
            _require(rgba_obj, "transfer", f"{path}.rgba.transfer"),
            f"{path}.rgba.transfer",
        )
        alpha_mode = _as_str(
            _require(rgba_obj, "alphaMode", f"{path}.rgba.alphaMode"),
            f"{path}.rgba.alphaMode",
        )
        if alpha_mode not in {"straight", "premultiplied"}:
            raise PipelineError(
                E_SCHEMA,
                f"Cannot determine alpha convention from {alpha_mode!r}",
                field_path=f"{path}.rgba.alphaMode",
            )
        if transfer not in {"srgb", "linear"}:
            raise PipelineError(
                E_SCHEMA,
                f"Unsupported transfer function {transfer!r}",
                field_path=f"{path}.rgba.transfer",
            )
        rgba = _unit_rgba(
            _require(rgba_obj, "values", f"{path}.rgba.values"),
            f"{path}.rgba.values",
        )
        linear, alpha = _decode_rgba(
            rgba, transfer, alpha_mode, f"{path}.rgba.values"
        )
        opacity = _as_float(
            _require(item, "opacity", f"{path}.opacity"), f"{path}.opacity"
        )
        layers.append(
            Layer(
                layer_id=layer_id,
                kind=kind,
                name=name,
                stack_order=stack_order,
                parent_id=parent_id,
                color_linear=linear,
                alpha=alpha,
                opacity=opacity,
                visible=True,
                visible_from=visible_from,
                visible_to=visible_to,
                alpha_mode=alpha_mode,
                source_color_space=transfer,
                bounds=bounds,
                source_version="vpfc/2",
            )
        )
        trace.extend(
            [
                _entry(f"{path}.layerId", f"layers[{index}].layer_id", "identity"),
                _entry(f"{path}.layerType", f"layers[{index}].kind", "identity"),
                _entry(f"{path}.displayName", f"layers[{index}].name", "identity"),
                _entry(f"{path}.zIndex", f"layers[{index}].stack_order", "identity"),
                _entry(
                    f"{path}.parentId",
                    f"layers[{index}].parent_id",
                    "identity",
                ),
                _entry(
                    f"{path}.visibility.startTick",
                    f"layers[{index}].visible_from",
                    "tick_to_frame",
                ),
                _entry(
                    f"{path}.visibility.endTickExclusive",
                    f"layers[{index}].visible_to",
                    "tick_to_frame_exclusive",
                ),
                _entry(
                    f"{path}.bounds",
                    f"layers[{index}].bounds",
                    "left_top_width_height_to_rect",
                ),
                _entry(
                    f"{path}.rgba.transfer",
                    f"layers[{index}].source_color_space",
                    "identity",
                ),
                _entry(
                    f"{path}.rgba.alphaMode",
                    f"layers[{index}].alpha_mode",
                    "identity",
                ),
                _entry(
                    f"{path}.rgba.values",
                    f"layers[{index}].color_linear",
                    "declared_transfer_and_alpha_to_linear_straight",
                ),
                _entry(
                    f"{path}.rgba.values[3]",
                    f"layers[{index}].alpha",
                    "channel_index_3_to_alpha",
                ),
                _entry(f"{path}.opacity", f"layers[{index}].opacity", "identity"),
            ]
        )
    layers.sort(key=lambda layer: layer.stack_order)
    model = SceneModel(
        scene_id=scene_id,
        source_version="vpfc/2",
        canvas=canvas,
        timeline=timeline,
        layers=tuple(layers),
        source_document_sha256=sha,
        mapping_trace=tuple(trace),
    )
    _validate_loaded_model(model)
    return LoadResult(model, tuple(trace))


def _parse_v3(
    document: dict[str, Any], sha: str, version_path: str
) -> LoadResult:
    canvas = _parse_canvas(document, "vpfc/3")
    timeline = _parse_timeline(document, "vpfc/3")
    scene_id = _as_str(_require(document, "sceneId", "sceneId"), "sceneId")
    raw_layers = _as_list(_require(document, "layers", "layers"), "layers")
    layers: list[Layer] = []
    trace = _base_mapping("vpfc/3", version_path)
    trace.append(_entry("sceneId", "scene_id", "identity"))

    def parse_node(
        raw: Any,
        path: str,
        parent_id: str | None,
    ) -> None:
        item = _as_object(raw, path)
        layer_id = _as_str(_require(item, "id", f"{path}.id"), f"{path}.id")
        kind = _as_str(_require(item, "kind", f"{path}.kind"), f"{path}.kind")
        name = _as_str(_require(item, "name", f"{path}.name"), f"{path}.name")
        stack_order = _as_int(
            _require(item, "stackIndex", f"{path}.stackIndex"),
            f"{path}.stackIndex",
        )
        visibility = _as_object(
            _require(item, "visibility", f"{path}.visibility"),
            f"{path}.visibility",
        )
        visible_from = _as_int(
            _require(
                visibility, "fromFrame", f"{path}.visibility.fromFrame"
            ),
            f"{path}.visibility.fromFrame",
        )
        visible_to = _as_int(
            _require(
                visibility,
                "toFrameExclusive",
                f"{path}.visibility.toFrameExclusive",
            ),
            f"{path}.visibility.toFrameExclusive",
        )
        bounds_values = _as_list(
            _require(item, "bounds", f"{path}.bounds"), f"{path}.bounds"
        )
        if len(bounds_values) != 4:
            raise PipelineError(
                E_SCHEMA,
                "bounds must contain [x, y, width, height]",
                field_path=f"{path}.bounds",
            )
        bounds = Rect(
            *[
                _as_int(value, f"{path}.bounds[{index}]")
                for index, value in enumerate(bounds_values)
            ]
        )
        alpha_obj = _as_object(
            _require(item, "alpha", f"{path}.alpha"), f"{path}.alpha"
        )
        alpha_mode = _as_str(
            _require(alpha_obj, "mode", f"{path}.alpha.mode"),
            f"{path}.alpha.mode",
        )
        if alpha_mode not in {"straight", "premultiplied"}:
            raise PipelineError(
                E_SCHEMA,
                f"Cannot determine alpha convention from {alpha_mode!r}",
                field_path=f"{path}.alpha.mode",
            )
        color_obj = _as_object(
            _require(item, "color", f"{path}.color"), f"{path}.color"
        )
        transfer = _as_str(
            _require(color_obj, "space", f"{path}.color.space"),
            f"{path}.color.space",
        )
        if transfer not in {"srgb", "linear"}:
            raise PipelineError(
                E_SCHEMA,
                f"Unsupported transfer function {transfer!r}",
                field_path=f"{path}.color.space",
            )
        rgba = _unit_rgba(
            _require(color_obj, "rgba", f"{path}.color.rgba"),
            f"{path}.color.rgba",
        )
        linear, alpha = _decode_rgba(
            rgba, transfer, alpha_mode, f"{path}.color.rgba"
        )
        opacity = _as_float(
            _require(item, "opacity", f"{path}.opacity"), f"{path}.opacity"
        )
        layer_index = len(layers)
        layers.append(
            Layer(
                layer_id=layer_id,
                kind=kind,
                name=name,
                stack_order=stack_order,
                parent_id=parent_id,
                color_linear=linear,
                alpha=alpha,
                opacity=opacity,
                visible=True,
                visible_from=visible_from,
                visible_to=visible_to,
                alpha_mode=alpha_mode,
                source_color_space=transfer,
                bounds=bounds,
                source_version="vpfc/3",
            )
        )
        trace.extend(
            [
                _entry(f"{path}.id", f"layers[{layer_index}].layer_id", "identity"),
                _entry(f"{path}.kind", f"layers[{layer_index}].kind", "identity"),
                _entry(f"{path}.name", f"layers[{layer_index}].name", "identity"),
                _entry(
                    f"{path}.stackIndex",
                    f"layers[{layer_index}].stack_order",
                    "identity",
                ),
                _entry(
                    f"{path}.visibility.fromFrame",
                    f"layers[{layer_index}].visible_from",
                    "identity",
                ),
                _entry(
                    f"{path}.visibility.toFrameExclusive",
                    f"layers[{layer_index}].visible_to",
                    "identity",
                ),
                _entry(
                    f"{path}.bounds",
                    f"layers[{layer_index}].bounds",
                    "array_to_rect",
                ),
                _entry(
                    f"{path}.alpha.mode",
                    f"layers[{layer_index}].alpha_mode",
                    "identity",
                ),
                _entry(
                    f"{path}.color.space",
                    f"layers[{layer_index}].source_color_space",
                    "identity",
                ),
                _entry(
                    f"{path}.color.rgba",
                    f"layers[{layer_index}].color_linear",
                    "declared_transfer_and_alpha_to_linear_straight",
                ),
                _entry(
                    f"{path}.color.rgba[3]",
                    f"layers[{layer_index}].alpha",
                    "channel_index_3_to_alpha",
                ),
                _entry(
                    f"{path}.opacity",
                    f"layers[{layer_index}].opacity",
                    "identity",
                ),
            ]
        )
        if parent_id is not None:
            trace.append(
                _entry(
                    f"{path}.__nesting__",
                    f"layers[{layer_index}].parent_id",
                    "nesting_to_parent_id",
                )
            )
        children_raw = item.get("children", [])
        children = _as_list(children_raw, f"{path}.children")
        for child_index, child in enumerate(children):
            parse_node(child, f"{path}.children[{child_index}]", layer_id)

    for index, raw in enumerate(raw_layers):
        parse_node(raw, f"layers[{index}]", None)
    layers.sort(key=lambda layer: layer.stack_order)
    model = SceneModel(
        scene_id=scene_id,
        source_version="vpfc/3",
        canvas=canvas,
        timeline=timeline,
        layers=tuple(layers),
        source_document_sha256=sha,
        mapping_trace=tuple(trace),
    )
    _validate_loaded_model(model)
    return LoadResult(model, tuple(trace))


def _validate_loaded_model(model: SceneModel) -> None:
    try:
        model.validate()
    except PipelineError as exc:
        if exc.code == E_PARA:
            raise PipelineError(
                E_SCHEMA,
                exc.message,
                field_path=exc.field_path,
                details=exc.details,
            ) from exc
        raise


def load_scene_result(path: str | Path) -> LoadResult:
    source_path = Path(path)
    document, sha = _read_json(source_path)
    version, version_path = _detect_version(document)
    if version == "vpfc/1":
        result = _parse_v1(document, sha, version_path)
    elif version == "vpfc/2":
        result = _parse_v2(document, sha, version_path)
    else:
        result = _parse_v3(document, sha, version_path)
    return LoadResult(
        model=result.model,
        mapping_trace=result.mapping_trace,
        warnings=result.warnings,
    )


def load_scene(path: str | Path) -> SceneModel:
    return load_scene_result(path).model
