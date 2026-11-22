from __future__ import annotations

import copy
from dataclasses import dataclass, field
from typing import Any

from .models import InputEvent, stable_digest


def _scale(value: int, source_total: int, target_total: int) -> int:
    return int(value) * int(target_total) // int(source_total)


@dataclass
class DesktopModel:
    declared_width: int
    declared_height: int
    width: int
    height: int
    window_offsets: dict[int, list[int]] = field(default_factory=dict)
    active_window: int | None = None
    cursor_override: tuple[int, int] | None = None
    key_buffer: str = ""
    scroll_offset: int = 0
    click_count: int = 0
    drag_count: int = 0
    key_count: int = 0
    scroll_count: int = 0
    last_event_seq: int = 0
    last_frame_seq: int = 0
    revision: int = 0
    scene: dict[str, Any] = field(default_factory=dict)
    last_clip_trace: list[dict[str, Any]] = field(default_factory=list)

    def observe_scene(self, scene: dict[str, Any], frame_seq: int) -> None:
        self.scene = copy.deepcopy(scene)
        self.last_frame_seq = frame_seq

    def _clip_axis(
        self, field_name: str, value: int, source_total: int, target_total: int
    ) -> tuple[int, dict[str, Any] | None]:
        mapped_raw = _scale(value, source_total, target_total)
        clipped = max(0, min(target_total - 1, mapped_raw))
        if clipped == value and mapped_raw == value:
            return clipped, None
        trace = {
            "field": field_name,
            "original": value,
            "mapped_raw": mapped_raw,
            "clipped": clipped,
            "reason": (
                "below_min"
                if mapped_raw < 0
                else "above_max"
                if mapped_raw >= target_total
                else "coordinate_scale"
            ),
        }
        return clipped, trace

    def map_point(
        self, x: int, y: int, prefix: str = ""
    ) -> tuple[tuple[int, int], list[dict[str, Any]]]:
        mapped_x, trace_x = self._clip_axis(
            f"{prefix}x", x, self.declared_width, self.width
        )
        mapped_y, trace_y = self._clip_axis(
            f"{prefix}y", y, self.declared_height, self.height
        )
        traces = [trace for trace in (trace_x, trace_y) if trace is not None]
        return (mapped_x, mapped_y), traces

    def _window_rect(self, window: dict[str, Any]) -> tuple[int, int, int, int]:
        offset = self.window_offsets.get(int(window["id"]), [0, 0])
        return (
            _scale(int(window["x"]), self.declared_width, self.width)
            + int(offset[0]),
            _scale(int(window["y"]), self.declared_height, self.height)
            + int(offset[1]),
            max(1, _scale(int(window["w"]), self.declared_width, self.width)),
            max(1, _scale(int(window["h"]), self.declared_height, self.height)),
        )

    def hit_test(self, x: int, y: int) -> int | None:
        for window in reversed(self.scene.get("windows", [])):
            wx, wy, ww, wh = self._window_rect(window)
            if wx <= x < wx + ww and wy <= y < wy + wh:
                return int(window["id"])
        return None

    def apply(self, event: InputEvent) -> tuple[list[int], list[dict[str, Any]]]:
        start, traces = self.map_point(event.x, event.y)
        self.cursor_override = start
        hit = self.hit_test(*start)
        mapped: list[int] = [start[0], start[1]]
        if event.kind == "click":
            if hit is not None:
                self.active_window = hit
            self.click_count += 1
        elif event.kind == "drag":
            assert event.to_x is not None and event.to_y is not None
            end, end_traces = self.map_point(event.to_x, event.to_y, "to_")
            traces.extend(end_traces)
            target = hit if hit is not None else self.active_window
            if target is not None:
                self.active_window = target
                offset = self.window_offsets.setdefault(int(target), [0, 0])
                offset[0] += end[0] - start[0]
                offset[1] += end[1] - start[1]
            self.cursor_override = end
            self.drag_count += 1
            mapped.extend([end[0], end[1]])
        elif event.kind == "key":
            if hit is not None:
                self.active_window = hit
            assert event.key is not None
            self.key_buffer = (self.key_buffer + event.key.upper())[-64:]
            self.key_count += 1
        elif event.kind == "scroll":
            if hit is not None:
                self.active_window = hit
            assert event.delta is not None
            self.scroll_offset += event.delta * 4
            self.scroll_count += 1
        self.last_event_seq = event.seq
        self.last_clip_trace = traces
        self.revision += 1
        return mapped, traces

    def render_modifiers(self, frame_seq: int) -> dict[str, Any]:
        cursor = self.cursor_override
        if cursor is None:
            source_cursor = self.scene.get(
                "cursor", [self.declared_width // 2, self.declared_height // 2]
            )
            cursor = (
                _scale(int(source_cursor[0]), self.declared_width, self.width),
                _scale(int(source_cursor[1]), self.declared_height, self.height),
            )
        return {
            "window_offsets": {
                str(key): list(value)
                for key, value in self.window_offsets.items()
            },
            "active_window": self.active_window,
            "cursor": list(cursor),
            "key_buffer": self.key_buffer,
            "scroll_offset": self.scroll_offset,
            "frame_seq": frame_seq,
        }

    def snapshot(self) -> dict[str, Any]:
        windows = []
        for window in self.scene.get("windows", []):
            offset = self.window_offsets.get(int(window["id"]), [0, 0])
            windows.append(
                {
                    "id": int(window["id"]),
                    "title": str(window["title"]),
                    "offset": [int(offset[0]), int(offset[1])],
                }
            )
        return {
            "declared": {
                "width": self.declared_width,
                "height": self.declared_height,
            },
            "canvas": {"width": self.width, "height": self.height},
            "windows": windows,
            "active_window": self.active_window,
            "cursor": (
                list(self.cursor_override)
                if self.cursor_override is not None
                else None
            ),
            "key_buffer": self.key_buffer,
            "scroll_offset": self.scroll_offset,
            "counts": {
                "click": self.click_count,
                "drag": self.drag_count,
                "key": self.key_count,
                "scroll": self.scroll_count,
            },
            "last_event_seq": self.last_event_seq,
            "last_frame_seq": self.last_frame_seq,
            "revision": self.revision,
            "last_clip_trace": self.last_clip_trace,
        }

    def digest(self) -> str:
        return stable_digest(self.snapshot())

    def to_checkpoint(self) -> dict[str, Any]:
        return {
            "declared_width": self.declared_width,
            "declared_height": self.declared_height,
            "width": self.width,
            "height": self.height,
            "window_offsets": {
                str(key): list(value)
                for key, value in self.window_offsets.items()
            },
            "active_window": self.active_window,
            "cursor_override": (
                list(self.cursor_override)
                if self.cursor_override is not None
                else None
            ),
            "key_buffer": self.key_buffer,
            "scroll_offset": self.scroll_offset,
            "click_count": self.click_count,
            "drag_count": self.drag_count,
            "key_count": self.key_count,
            "scroll_count": self.scroll_count,
            "last_event_seq": self.last_event_seq,
            "last_frame_seq": self.last_frame_seq,
            "revision": self.revision,
            "scene": copy.deepcopy(self.scene),
            "last_clip_trace": copy.deepcopy(self.last_clip_trace),
        }

    @classmethod
    def from_checkpoint(cls, data: dict[str, Any]) -> "DesktopModel":
        model = cls(
            declared_width=int(data["declared_width"]),
            declared_height=int(data["declared_height"]),
            width=int(data["width"]),
            height=int(data["height"]),
            window_offsets={
                int(key): [int(v[0]), int(v[1])]
                for key, v in data.get("window_offsets", {}).items()
            },
            active_window=(
                None
                if data.get("active_window") is None
                else int(data["active_window"])
            ),
            cursor_override=(
                None
                if data.get("cursor_override") is None
                else (
                    int(data["cursor_override"][0]),
                    int(data["cursor_override"][1]),
                )
            ),
            key_buffer=str(data.get("key_buffer", "")),
            scroll_offset=int(data.get("scroll_offset", 0)),
            click_count=int(data.get("click_count", 0)),
            drag_count=int(data.get("drag_count", 0)),
            key_count=int(data.get("key_count", 0)),
            scroll_count=int(data.get("scroll_count", 0)),
            last_event_seq=int(data.get("last_event_seq", 0)),
            last_frame_seq=int(data.get("last_frame_seq", 0)),
            revision=int(data.get("revision", 0)),
            scene=copy.deepcopy(data.get("scene", {})),
            last_clip_trace=copy.deepcopy(data.get("last_clip_trace", [])),
        )
        return model
