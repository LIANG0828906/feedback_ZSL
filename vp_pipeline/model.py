"""Canonical internal compositing model."""

from __future__ import annotations

from dataclasses import dataclass, field, replace
from fractions import Fraction
from typing import Any, Iterable

from .errors import E_PARA, E_SCHEMA, PipelineError
from .serialization import canonical_sha256


@dataclass(frozen=True)
class Rect:
    x: int
    y: int
    width: int
    height: int

    def to_dict(self) -> dict[str, int]:
        return {"x": self.x, "y": self.y, "width": self.width, "height": self.height}


@dataclass(frozen=True)
class Canvas:
    width: int
    height: int
    channels: int

    def to_dict(self) -> dict[str, int]:
        return {
            "width": self.width,
            "height": self.height,
            "channels": self.channels,
        }


@dataclass(frozen=True)
class Timeline:
    frames: int
    window_size: int
    fps_num: int
    fps_den: int
    source_time_unit: str
    source_time_basis: dict[str, Any] = field(default_factory=dict)

    @property
    def fps(self) -> float:
        return self.fps_num / self.fps_den

    @property
    def frame_period_seconds(self) -> float:
        return self.fps_den / self.fps_num

    @property
    def window_count(self) -> int:
        return (self.frames + self.window_size - 1) // self.window_size

    def frame_time_seconds(self, frame_index: int) -> float:
        return frame_index * self.fps_den / self.fps_num

    def to_dict(self) -> dict[str, Any]:
        return {
            "frames": self.frames,
            "window_size": self.window_size,
            "fps_num": self.fps_num,
            "fps_den": self.fps_den,
            "source_time_unit": self.source_time_unit,
            "source_time_basis": self.source_time_basis,
        }


@dataclass(frozen=True)
class Layer:
    layer_id: str
    kind: str
    name: str
    stack_order: int
    parent_id: str | None
    color_linear: tuple[float, float, float]
    alpha: float
    opacity: float
    visible: bool
    visible_from: int
    visible_to: int
    alpha_mode: str
    source_color_space: str
    bounds: Rect
    source_version: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "layer_id": self.layer_id,
            "kind": self.kind,
            "name": self.name,
            "stack_order": self.stack_order,
            "parent_id": self.parent_id,
            "color_linear": list(self.color_linear),
            "alpha": self.alpha,
            "opacity": self.opacity,
            "visible": self.visible,
            "visible_from": self.visible_from,
            "visible_to": self.visible_to,
            "alpha_mode": self.alpha_mode,
            "source_color_space": self.source_color_space,
            "bounds": self.bounds.to_dict(),
            "source_version": self.source_version,
        }


@dataclass(frozen=True)
class SceneModel:
    scene_id: str
    source_version: str
    canvas: Canvas
    timeline: Timeline
    layers: tuple[Layer, ...]
    source_document_sha256: str = ""
    mapping_trace: tuple[dict[str, str], ...] = ()

    @property
    def layers_by_id(self) -> dict[str, Layer]:
        return {layer.layer_id: layer for layer in self.layers}

    @property
    def model_fingerprint(self) -> str:
        return canonical_sha256(self.to_dict(include_mapping=False))

    def to_dict(self, include_mapping: bool = True) -> dict[str, Any]:
        value: dict[str, Any] = {
            "scene_id": self.scene_id,
            "source_version": self.source_version,
            "canvas": self.canvas.to_dict(),
            "timeline": self.timeline.to_dict(),
            "layers": [layer.to_dict() for layer in self.layers],
            "source_document_sha256": self.source_document_sha256,
        }
        if include_mapping:
            value["mapping_trace"] = list(self.mapping_trace)
        return value

    def layer(self, layer_id: str) -> Layer:
        try:
            return self.layers_by_id[layer_id]
        except KeyError as exc:
            raise PipelineError(
                E_PARA,
                f"Unknown layer id {layer_id!r}",
                field_path="layer_id",
            ) from exc

    def replace_layer(self, layer_id: str, **changes: Any) -> "SceneModel":
        current = self.layer(layer_id)
        updated = replace(current, **changes)
        layers = tuple(
            sorted(
                (updated if layer.layer_id == layer_id else layer for layer in self.layers),
                key=lambda item: item.stack_order,
            )
        )
        return replace(self, layers=layers)

    def replace_timeline(self, **changes: Any) -> "SceneModel":
        return replace(self, timeline=replace(self.timeline, **changes))

    def validate(self) -> None:
        if not self.scene_id:
            raise PipelineError(E_SCHEMA, "scene_id is required", "scene_id")
        if self.canvas.width <= 0 or self.canvas.height <= 0:
            raise PipelineError(
                E_PARA, "Canvas dimensions must be positive", "canvas"
            )
        if self.canvas.channels != 4:
            raise PipelineError(
                E_SCHEMA,
                "This compositor requires four-channel RGBA scenes",
                "canvas.channels",
                channels=self.canvas.channels,
            )
        timeline = self.timeline
        if timeline.frames <= 0:
            raise PipelineError(E_PARA, "Frame count must be positive", "timeline.frames")
        if timeline.window_size <= 0:
            raise PipelineError(
                E_PARA, "Window size must be positive", "timeline.window_size"
            )
        if timeline.window_size > timeline.frames:
            raise PipelineError(
                E_PARA,
                "Window size cannot exceed frame count",
                "timeline.window_size",
            )
        if timeline.fps_num <= 0 or timeline.fps_den <= 0:
            raise PipelineError(
                E_PARA,
                "Frame rate numerator and denominator must be positive",
                "timeline.fps",
            )
        if not self.layers:
            raise PipelineError(E_SCHEMA, "At least one layer is required", "layers")
        ids = [layer.layer_id for layer in self.layers]
        if len(ids) != len(set(ids)):
            raise PipelineError(E_SCHEMA, "Layer ids must be unique", "layers")
        orders = [layer.stack_order for layer in self.layers]
        if len(orders) != len(set(orders)):
            raise PipelineError(
                E_SCHEMA, "Layer stack_order values must be unique", "layers"
            )
        known = set(ids)
        for index, layer in enumerate(self.layers):
            if layer.parent_id is not None and layer.parent_id not in known:
                raise PipelineError(
                    E_SCHEMA,
                    f"Unknown parent layer {layer.parent_id!r}",
                    f"layers[{index}].parent_id",
                )
            if layer.parent_id == layer.layer_id:
                raise PipelineError(
                    E_SCHEMA,
                    "A layer cannot be its own parent",
                    f"layers[{index}].parent_id",
                )
            if not 0.0 <= layer.alpha <= 1.0:
                raise PipelineError(
                    E_PARA,
                    "Alpha must be within [0, 1]",
                    f"layers[{index}].alpha",
                )
            if not 0.0 <= layer.opacity <= 1.0:
                raise PipelineError(
                    E_PARA,
                    "Opacity must be within [0, 1]",
                    f"layers[{index}].opacity",
                )
            if any(not 0.0 <= channel <= 1.0 for channel in layer.color_linear):
                raise PipelineError(
                    E_PARA,
                    "Linear RGB channels must be within [0, 1]",
                    f"layers[{index}].color_linear",
                )
            if not 0 <= layer.visible_from <= timeline.frames:
                raise PipelineError(
                    E_PARA,
                    "visible_from must be within the timeline",
                    f"layers[{index}].visible_from",
                )
            if not 0 <= layer.visible_to <= timeline.frames:
                raise PipelineError(
                    E_PARA,
                    "visible_to must be within the timeline",
                    f"layers[{index}].visible_to",
                )
            if layer.visible_to < layer.visible_from:
                raise PipelineError(
                    E_PARA,
                    "visible_to must not precede visible_from",
                    f"layers[{index}].visible_to",
                )
            if layer.bounds.width < 0 or layer.bounds.height < 0:
                raise PipelineError(
                    E_PARA,
                    "Layer bounds must be non-negative",
                    f"layers[{index}].bounds",
                )
        self._validate_acyclic()

    def _validate_acyclic(self) -> None:
        by_id = self.layers_by_id
        for layer in self.layers:
            seen: set[str] = set()
            cursor = layer
            while cursor.parent_id is not None:
                if cursor.layer_id in seen:
                    raise PipelineError(
                        E_SCHEMA,
                        "Layer hierarchy contains a cycle",
                        f"layers.{layer.layer_id}.parent_id",
                    )
                seen.add(cursor.layer_id)
                cursor = by_id[cursor.parent_id]

    def is_layer_visible(self, layer_id: str, frame_index: int) -> bool:
        cursor = self.layer(layer_id)
        seen: set[str] = set()
        while True:
            if cursor.layer_id in seen:
                return False
            seen.add(cursor.layer_id)
            if (
                not cursor.visible
                or frame_index < cursor.visible_from
                or frame_index >= cursor.visible_to
            ):
                return False
            if cursor.parent_id is None:
                return True
            cursor = self.layer(cursor.parent_id)

    def visible_layers(self, frame_index: int) -> tuple[Layer, ...]:
        return tuple(
            layer
            for layer in sorted(self.layers, key=lambda item: item.stack_order)
            if self.is_layer_visible(layer.layer_id, frame_index)
        )

    def window_ranges(self) -> tuple[tuple[int, int, int], ...]:
        ranges = []
        for index in range(self.timeline.window_count):
            start = index * self.timeline.window_size
            end = min(start + self.timeline.window_size, self.timeline.frames)
            ranges.append((index, start, end))
        return tuple(ranges)

    def window_affected_layer_ids(self, window_index: int) -> tuple[str, ...]:
        _, start, end = self.window_ranges()[window_index]
        affected = []
        for layer in self.layers:
            if any(self.is_layer_visible(layer.layer_id, frame) for frame in range(start, end)):
                affected.append(layer.layer_id)
        return tuple(affected)

    def window_snapshot(self, window_index: int) -> dict[str, Any]:
        _, start, end = self.window_ranges()[window_index]
        included_ids: set[str] = set()
        for layer in self.layers:
            if any(self.is_layer_visible(layer.layer_id, frame) for frame in range(start, end)):
                included_ids.add(layer.layer_id)
                cursor = layer
                while cursor.parent_id is not None:
                    cursor = self.layer(cursor.parent_id)
                    included_ids.add(cursor.layer_id)
        layers = []
        for layer in self.layers:
            if layer.layer_id not in included_ids:
                continue
            payload = layer.to_dict()
            payload.pop("name", None)
            payload.pop("kind", None)
            layers.append(payload)
        return {
            "scene_id": self.scene_id,
            "source_version": self.source_version,
            "canvas": self.canvas.to_dict(),
            "timeline": self.timeline.to_dict(),
            "window": {"index": window_index, "start": start, "end_exclusive": end},
            "layers": layers,
        }

    def tiled_layer_ids(self) -> Iterable[str]:
        for layer in self.layers:
            yield layer.layer_id


def timeline_from_seconds(
    frames: int,
    window_size: int,
    period_num: int,
    period_den: int,
    source_basis: dict[str, Any],
) -> Timeline:
    if period_num <= 0 or period_den <= 0:
        raise PipelineError(
            E_SCHEMA, "Frame period must be positive", "temporal.framePeriodSeconds"
        )
    rate = Fraction(period_den, period_num)
    return Timeline(
        frames=frames,
        window_size=window_size,
        fps_num=rate.numerator,
        fps_den=rate.denominator,
        source_time_unit="seconds",
        source_time_basis=source_basis,
    )
