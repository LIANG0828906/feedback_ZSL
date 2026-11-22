"""Pixel-exact comparison and localized difference reporting."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Mapping

import numpy as np

from .errors import E_DIFF, PipelineError
from .frame import FrameResult
from .model import SceneModel


@dataclass(frozen=True)
class DiffReport:
    frame_index: int
    window_index: int
    x: int
    y: int | None
    channel: int | None
    actual: int | None
    expected: int | None
    layer_id: str | None
    message: str

    def to_dict(self) -> dict[str, object]:
        return {
            "frame_index": self.frame_index,
            "window_index": self.window_index,
            "x": self.x,
            "y": self.y,
            "channel": self.channel,
            "actual": self.actual,
            "expected": self.expected,
            "layer_id": self.layer_id,
            "message": self.message,
        }


def layer_at_pixel(
    model: SceneModel,
    frame_index: int,
    x: int,
    y: int,
) -> str | None:
    top_layer: str | None = None
    top_order = -1
    for layer in model.visible_layers(frame_index):
        if (
            layer.bounds.x <= x < layer.bounds.x + layer.bounds.width
            and layer.bounds.y <= y < layer.bounds.y + layer.bounds.height
            and layer.stack_order > top_order
            and layer.alpha * layer.opacity > 0.0
        ):
            top_layer = layer.layer_id
            top_order = layer.stack_order
    return top_layer


def first_difference(
    actual: Mapping[int, FrameResult],
    expected: Mapping[int, FrameResult],
    model: SceneModel,
) -> DiffReport | None:
    all_frames = sorted(set(actual) | set(expected))
    for frame_index in all_frames:
        if frame_index not in actual or frame_index not in expected:
            window_index = frame_index // model.timeline.window_size
            return DiffReport(
                frame_index=frame_index,
                window_index=window_index,
                x=-1,
                y=None,
                channel=None,
                actual=None,
                expected=None,
                layer_id=None,
                message="Frame is missing from one result set",
            )
        actual_frame = actual[frame_index]
        expected_frame = expected[frame_index]
        if (
            actual_frame.width != expected_frame.width
            or actual_frame.height != expected_frame.height
            or actual_frame.channels != expected_frame.channels
            or actual_frame.fps_num != expected_frame.fps_num
            or actual_frame.fps_den != expected_frame.fps_den
        ):
            window_index = frame_index // model.timeline.window_size
            return DiffReport(
                frame_index=frame_index,
                window_index=window_index,
                x=-1,
                y=None,
                channel=None,
                actual=None,
                expected=None,
                layer_id=None,
                message="Frame metadata does not match",
            )
        if np.array_equal(actual_frame.pixels, expected_frame.pixels):
            continue
        delta = actual_frame.pixels.astype(np.int16) - expected_frame.pixels.astype(
            np.int16
        )
        mismatch = np.argwhere(delta != 0)
        y, x, channel = (int(value) for value in mismatch[0])
        window_index = frame_index // model.timeline.window_size
        return DiffReport(
            frame_index=frame_index,
            window_index=window_index,
            x=x,
            y=y,
            channel=channel,
            actual=int(actual_frame.pixels[y, x, channel]),
            expected=int(expected_frame.pixels[y, x, channel]),
            layer_id=layer_at_pixel(model, frame_index, x, y),
            message=(
                f"First pixel mismatch at frame {frame_index}, window "
                f"{window_index}, pixel ({x},{y}), channel {channel}"
            ),
        )
    return None


def assert_frames_equal(
    actual: Mapping[int, FrameResult],
    expected: Mapping[int, FrameResult],
    model: SceneModel,
) -> None:
    report = first_difference(actual, expected, model)
    if report is not None:
        raise PipelineError(
            E_DIFF,
            report.message,
            field_path="frames",
            details=report.to_dict(),
        )


def difference_heatmap(
    actual: FrameResult,
    expected: FrameResult,
) -> np.ndarray:
    if actual.pixels.shape != expected.pixels.shape:
        raise PipelineError(
            E_DIFF,
            "Cannot build a heat map for mismatched frame shapes",
            field_path="frames",
        )
    delta = np.abs(
        actual.pixels.astype(np.int16) - expected.pixels.astype(np.int16)
    )
    return np.max(delta, axis=2).astype(np.uint8)
