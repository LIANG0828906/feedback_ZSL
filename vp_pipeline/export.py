"""Deterministic cross-version scene and frame-pack export."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping, Sequence

import numpy as np

from .color import linear_straight_to_encoded
from .errors import E_DECODE, E_PARA, E_SCHEMA, E_VER, PipelineError
from .frame import FrameResult
from .loader import SUPPORTED_VERSIONS, load_scene
from .model import Layer, SceneModel
from .serialization import (
    atomic_write_bytes,
    bytes_sha256,
    canonical_sha256,
    pretty_json_bytes,
)
from .version import ALGORITHM_VERSION, EXPORT_FRAME_FORMAT


@dataclass(frozen=True)
class ExportResult:
    target_version: str
    output_directory: Path
    scene_path: Path
    frames_path: Path | None
    metadata_path: Path | None
    warnings: tuple[dict[str, Any], ...]
    degradation_trace: tuple[dict[str, Any], ...]
    scene_sha256: str
    frames_sha256: str | None

    @property
    def warning_codes(self) -> tuple[str, ...]:
        return tuple(str(item["code"]) for item in self.warnings)


def _lossy(
    layer: Layer,
    field_path: str,
    message: str,
    trace: dict[str, Any],
) -> tuple[dict[str, Any], dict[str, Any]]:
    warning = {
        "code": "E_LOSSY",
        "layer_id": layer.layer_id,
        "field_path": field_path,
        "message": message,
    }
    degradation = {
        "code": "E_LOSSY",
        "layer_id": layer.layer_id,
        "field_path": field_path,
        **trace,
    }
    return warning, degradation


def _effective_interval(
    model: SceneModel,
    layer: Layer,
) -> tuple[bool, int, int]:
    visible = layer.visible
    start = layer.visible_from
    end = layer.visible_to
    cursor = layer
    seen: set[str] = set()
    while cursor.parent_id is not None:
        if cursor.layer_id in seen:
            break
        seen.add(cursor.layer_id)
        cursor = model.layer(cursor.parent_id)
        visible = visible and cursor.visible
        start = max(start, cursor.visible_from)
        end = min(end, cursor.visible_to)
    if end < start:
        end = start
    return visible, start, end


def _v1_document(
    model: SceneModel,
) -> tuple[dict[str, Any], list[dict[str, Any]], list[dict[str, Any]]]:
    warnings: list[dict[str, Any]] = []
    trace: list[dict[str, Any]] = []
    layers = []
    for layer in model.layers:
        visible, visible_from, visible_to = _effective_interval(model, layer)
        if layer.parent_id is not None:
            warning, degradation = _lossy(
                layer,
                "parent_id",
                "vpfc/1 cannot express hierarchy; hierarchy is flattened",
                {
                    "source_field": "parent_id",
                    "source_value": layer.parent_id,
                    "target_field": None,
                    "action": "flattened",
                    "effective_visibility": {
                        "visible": visible,
                        "from": visible_from,
                        "to_exclusive": visible_to,
                    },
                },
            )
            warnings.append(warning)
            trace.append(degradation)
        if layer.alpha_mode != "premultiplied":
            warning, degradation = _lossy(
                layer,
                "alpha_mode",
                "vpfc/1 has one global premultiplied-alpha convention",
                {
                    "source_field": "alpha_mode",
                    "source_value": layer.alpha_mode,
                    "target_field": "alpha_mode",
                    "target_value": "premultiplied",
                    "action": "converted",
                },
            )
            warnings.append(warning)
            trace.append(degradation)
        if layer.source_color_space != "srgb":
            warning, degradation = _lossy(
                layer,
                "source_color_space",
                "vpfc/1 stores RGB in sRGB encoding",
                {
                    "source_field": "source_color_space",
                    "source_value": layer.source_color_space,
                    "target_field": "color_space",
                    "target_value": "srgb",
                    "action": "converted",
                },
            )
            warnings.append(warning)
            trace.append(degradation)
        encoded = linear_straight_to_encoded(
            layer.color_linear,
            layer.alpha,
            "srgb",
            "premultiplied",
        )
        x = layer.bounds.x
        y = layer.bounds.y
        layers.append(
            {
                "id": layer.layer_id,
                "type": layer.kind,
                "name": layer.name,
                "order": layer.stack_order,
                "visible": visible,
                "visible_from": visible_from,
                "visible_until": visible_to,
                "bounds": {
                    "x": x,
                    "y": y,
                    "w": layer.bounds.width,
                    "h": layer.bounds.height,
                },
                "rgba_srgb_premultiplied": [*encoded, layer.alpha],
                "opacity": layer.opacity,
            }
        )
    document = {
        "format": {
            "name": "virtual-production-composite",
            "version": "vpfc/1",
        },
        "sceneId": model.scene_id,
        "canvas": model.canvas.to_dict(),
        "timeline": {
            "frame_count": model.timeline.frames,
            "window_size": model.timeline.window_size,
            "time_base": {
                "unit": "frames",
                "fps": {
                    "num": model.timeline.fps_num,
                    "den": model.timeline.fps_den,
                },
            },
        },
        "color_space": "srgb",
        "alpha_mode": "premultiplied",
        "layers": layers,
    }
    return document, warnings, trace


def _v2_document(
    model: SceneModel,
) -> tuple[dict[str, Any], list[dict[str, Any]], list[dict[str, Any]]]:
    warnings: list[dict[str, Any]] = []
    trace: list[dict[str, Any]] = []
    ticks_per_frame = model.timeline.fps_den
    ticks_per_second = model.timeline.fps_num
    layers = []
    for layer in model.layers:
        if not layer.visible:
            warning, degradation = _lossy(
                layer,
                "visible",
                "vpfc/2 expresses visibility only as a frame interval",
                {
                    "source_field": "visible",
                    "source_value": False,
                    "target_field": "visibility",
                    "target_value": "empty interval",
                    "action": "normalized",
                },
            )
            warnings.append(warning)
            trace.append(degradation)
        encoded = linear_straight_to_encoded(
            layer.color_linear,
            layer.alpha,
            layer.source_color_space,
            layer.alpha_mode,
        )
        start = layer.visible_from
        end = layer.visible_to if layer.visible else layer.visible_from
        layers.append(
            {
                "layerId": layer.layer_id,
                "layerType": layer.kind,
                "displayName": layer.name,
                "zIndex": layer.stack_order,
                "parentId": layer.parent_id,
                "visibility": {
                    "startTick": start * ticks_per_frame,
                    "endTickExclusive": end * ticks_per_frame,
                },
                "bounds": {
                    "left": layer.bounds.x,
                    "top": layer.bounds.y,
                    "width": layer.bounds.width,
                    "height": layer.bounds.height,
                },
                "rgba": {
                    "transfer": layer.source_color_space,
                    "alphaMode": layer.alpha_mode,
                    "values": [*encoded, layer.alpha],
                },
                "opacity": layer.opacity,
            }
        )
    document = {
        "schema": {"vendor": "VP Stage Tools", "revision": "vpfc/2"},
        "sceneId": model.scene_id,
        "canvasWidth": model.canvas.width,
        "canvasHeight": model.canvas.height,
        "channelCount": model.canvas.channels,
        "frameCount": model.timeline.frames,
        "windowSize": model.timeline.window_size,
        "frameRate": {
            "num": model.timeline.fps_num,
            "den": model.timeline.fps_den,
        },
        "timeBase": {
            "unit": "ticks",
            "ticksPerSecond": ticks_per_second,
            "ticksPerFrame": ticks_per_frame,
        },
        "layers": layers,
    }
    return document, warnings, trace


def _v3_document(
    model: SceneModel,
) -> tuple[dict[str, Any], list[dict[str, Any]], list[dict[str, Any]]]:
    warnings: list[dict[str, Any]] = []
    trace: list[dict[str, Any]] = []
    by_parent: dict[str | None, list[Layer]] = {}
    for layer in model.layers:
        by_parent.setdefault(layer.parent_id, []).append(layer)

    def build(layer: Layer) -> dict[str, Any]:
        if not layer.visible:
            warning, degradation = _lossy(
                layer,
                "visible",
                "vpfc/3 expresses visibility only as a frame interval",
                {
                    "source_field": "visible",
                    "source_value": False,
                    "target_field": "visibility",
                    "target_value": "empty interval",
                    "action": "normalized",
                },
            )
            warnings.append(warning)
            trace.append(degradation)
        encoded = linear_straight_to_encoded(
            layer.color_linear,
            layer.alpha,
            layer.source_color_space,
            layer.alpha_mode,
        )
        node: dict[str, Any] = {
            "id": layer.layer_id,
            "kind": layer.kind,
            "name": layer.name,
            "stackIndex": layer.stack_order,
            "visibility": {
                "fromFrame": layer.visible_from,
                "toFrameExclusive": (
                    layer.visible_to if layer.visible else layer.visible_from
                ),
            },
            "bounds": [
                layer.bounds.x,
                layer.bounds.y,
                layer.bounds.width,
                layer.bounds.height,
            ],
            "alpha": {"mode": layer.alpha_mode},
            "color": {
                "space": layer.source_color_space,
                "rgba": [*encoded, layer.alpha],
            },
            "opacity": layer.opacity,
        }
        children = by_parent.get(layer.layer_id, [])
        if children:
            node["children"] = [build(child) for child in children]
        return node

    roots = [build(layer) for layer in by_parent.get(None, [])]
    document = {
        "document": {"kind": "vp-foreground-composite", "revision": "vpfc/3"},
        "sceneId": model.scene_id,
        "canvas": model.canvas.to_dict(),
        "temporal": {
            "frameCount": model.timeline.frames,
            "windowSize": model.timeline.window_size,
            "framePeriodSeconds": {
                "num": model.timeline.fps_den,
                "den": model.timeline.fps_num,
            },
        },
        "layers": roots,
    }
    return document, warnings, trace


def build_target_document(
    model: SceneModel,
    target_version: str,
) -> tuple[dict[str, Any], list[dict[str, Any]], list[dict[str, Any]]]:
    model.validate()
    if target_version not in SUPPORTED_VERSIONS:
        raise PipelineError(
            E_VER,
            f"Unsupported target version {target_version!r}",
            field_path="target_version",
            supported=list(SUPPORTED_VERSIONS),
        )
    if target_version == "vpfc/1":
        return _v1_document(model)
    if target_version == "vpfc/2":
        return _v2_document(model)
    return _v3_document(model)


def _normalize_frames(
    model: SceneModel,
    frames: Mapping[int, FrameResult],
) -> list[FrameResult]:
    if not frames:
        raise PipelineError(
            E_PARA, "At least one frame is required for export", "frames"
        )
    selected = []
    for frame_index in sorted(frames):
        frame = frames[frame_index]
        if frame_index < 0 or frame_index >= model.timeline.frames:
            raise PipelineError(
                E_PARA,
                "Frame index is outside the scene timeline",
                field_path="frames",
            )
        if (
            frame.width != model.canvas.width
            or frame.height != model.canvas.height
            or frame.channels != model.canvas.channels
        ):
            raise PipelineError(
                E_SCHEMA,
                "Frame dimensions do not match the internal model",
                field_path=f"frames[{frame_index}]",
            )
        if (
            frame.fps_num != model.timeline.fps_num
            or frame.fps_den != model.timeline.fps_den
        ):
            raise PipelineError(
                E_SCHEMA,
                "Frame time base does not match the internal model",
                field_path=f"frames[{frame_index}].fps",
            )
        selected.append(frame)
    return selected


def export_scene(
    model: SceneModel,
    target_version: str,
    output_directory: str | Path,
) -> ExportResult:
    output = Path(output_directory)
    output.mkdir(parents=True, exist_ok=True)
    document, warnings, trace = build_target_document(model, target_version)
    scene_bytes = pretty_json_bytes(document)
    scene_path = output / "scene.json"
    atomic_write_bytes(scene_path, scene_bytes)
    return ExportResult(
        target_version=target_version,
        output_directory=output,
        scene_path=scene_path,
        frames_path=None,
        metadata_path=None,
        warnings=tuple(warnings),
        degradation_trace=tuple(trace),
        scene_sha256=bytes_sha256(scene_bytes),
        frames_sha256=None,
    )


def export_bundle(
    model: SceneModel,
    frames: Mapping[int, FrameResult],
    target_version: str,
    output_directory: str | Path,
    *,
    window_indices: Sequence[int] | None = None,
) -> ExportResult:
    if window_indices is not None:
        allowed = set(window_indices)
        ranges = model.window_ranges()
        frame_filter: set[int] = set()
        for index in allowed:
            if index < 0 or index >= len(ranges):
                raise PipelineError(
                    E_PARA,
                    "window index is outside the timeline",
                    field_path="window_indices",
                )
            _, start, end = ranges[index]
            frame_filter.update(range(start, end))
        frames = {
            frame_index: frame
            for frame_index, frame in frames.items()
            if frame_index in frame_filter
        }
    selected_frames = _normalize_frames(model, frames)
    output = Path(output_directory)
    output.mkdir(parents=True, exist_ok=True)
    document, warnings, trace = build_target_document(model, target_version)
    scene_bytes = pretty_json_bytes(document)
    scene_path = output / "scene.json"
    atomic_write_bytes(scene_path, scene_bytes)

    frame_bytes = bytearray()
    frame_records = []
    for frame in selected_frames:
        raw = frame.pixel_bytes
        offset = len(frame_bytes)
        frame_bytes.extend(raw)
        frame_records.append(
            {
                "offset": offset,
                "size": len(raw),
                "pixel_sha256": bytes_sha256(raw),
                "stats": frame.stats_dict(),
            }
        )
    frames_bytes = bytes(frame_bytes)
    frames_path = output / "frames.bin"
    atomic_write_bytes(frames_path, frames_bytes)
    metadata = {
        "format": EXPORT_FRAME_FORMAT,
        "algorithm_version": ALGORITHM_VERSION,
        "scene_id": model.scene_id,
        "target_version": target_version,
        "frame_count": len(selected_frames),
        "frames": frame_records,
    }
    metadata_bytes = pretty_json_bytes(metadata)
    metadata_path = output / "frames_meta.json"
    atomic_write_bytes(metadata_path, metadata_bytes)
    report = {
        "code": "E_LOSSY" if warnings else "OK",
        "target_version": target_version,
        "scene_sha256": bytes_sha256(scene_bytes),
        "frames_sha256": bytes_sha256(frames_bytes),
        "warnings": warnings,
        "degradation_trace": trace,
    }
    report_bytes = pretty_json_bytes(report)
    atomic_write_bytes(output / "export_report.json", report_bytes)
    return ExportResult(
        target_version=target_version,
        output_directory=output,
        scene_path=scene_path,
        frames_path=frames_path,
        metadata_path=metadata_path,
        warnings=tuple(warnings),
        degradation_trace=tuple(trace),
        scene_sha256=bytes_sha256(scene_bytes),
        frames_sha256=bytes_sha256(frames_bytes),
    )


def load_export_bundle(
    output_directory: str | Path,
) -> tuple[SceneModel, dict[int, FrameResult], dict[str, Any]]:
    output = Path(output_directory)
    try:
        scene = load_scene(output / "scene.json")
        metadata = json.loads(
            (output / "frames_meta.json").read_text(encoding="utf-8")
        )
        data = (output / "frames.bin").read_bytes()
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PipelineError(
            E_DECODE,
            f"Export bundle cannot be decoded: {exc}",
            field_path=str(output),
        ) from exc
    if metadata.get("format") != EXPORT_FRAME_FORMAT:
        raise PipelineError(
            E_SCHEMA,
            "Unsupported frame-pack format",
            field_path="frames_meta.format",
        )
    frames: dict[int, FrameResult] = {}
    cursor = 0
    for record in metadata.get("frames", []):
        offset = int(record["offset"])
        size = int(record["size"])
        if offset != cursor or offset + size > len(data):
            raise PipelineError(
                E_DECODE,
                "Invalid frame byte range",
                field_path="frames_meta.frames",
            )
        raw = data[offset : offset + size]
        if bytes_sha256(raw) != record["pixel_sha256"]:
            raise PipelineError(
                E_DECODE,
                "Frame pixel hash mismatch",
                field_path="frames.bin",
            )
        stats = record["stats"]
        pixels = np.frombuffer(raw, dtype=np.uint8).reshape(
            int(stats["height"]),
            int(stats["width"]),
            int(stats["channels"]),
        )
        frame = FrameResult.from_pixels(pixels.copy(), stats)
        if frame.content_hash != stats["content_hash"]:
            raise PipelineError(
                E_DECODE,
                "Frame content hash mismatch",
                field_path="frames.bin",
            )
        frames[frame.frame_index] = frame
        cursor += size
    if cursor != len(data):
        raise PipelineError(
            E_DECODE,
            "Trailing bytes in frame pack",
            field_path="frames.bin",
        )
    report_path = output / "export_report.json"
    report = (
        json.loads(report_path.read_text(encoding="utf-8"))
        if report_path.exists()
        else {}
    )
    return scene, frames, report
