"""Local per-frame composition instruction parsing and validation."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import E_DECODE, E_SCHEMA, PipelineError
from .model import SceneModel


@dataclass(frozen=True)
class FrameInstruction:
    frame: int
    window: int
    sample_pixels: tuple[tuple[int, int], ...]


@dataclass(frozen=True)
class InstructionSet:
    scene_id: str
    window_size: int
    window_count: int
    frames: tuple[FrameInstruction, ...]


def load_frame_instructions(path: str | Path) -> InstructionSet:
    source = Path(path)
    try:
        document = json.loads(source.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PipelineError(
            E_DECODE,
            f"Frame instructions cannot be decoded: {exc}",
            field_path=str(source),
        ) from exc
    if not isinstance(document, dict):
        raise PipelineError(
            E_SCHEMA, "Instruction root must be an object", "frame_instructions"
        )
    scene_id = document.get("scene_id")
    if not isinstance(scene_id, str) or not scene_id:
        raise PipelineError(
            E_SCHEMA, "scene_id is required", "frame_instructions.scene_id"
        )
    window_size = document.get("window_size")
    window_count = document.get("window_count")
    if isinstance(window_size, bool) or not isinstance(window_size, int):
        raise PipelineError(
            E_SCHEMA,
            "window_size must be an integer",
            "frame_instructions.window_size",
        )
    if isinstance(window_count, bool) or not isinstance(window_count, int):
        raise PipelineError(
            E_SCHEMA,
            "window_count must be an integer",
            "frame_instructions.window_count",
        )
    raw_frames = document.get("instructions")
    if not isinstance(raw_frames, list):
        raise PipelineError(
            E_SCHEMA,
            "instructions must be an array",
            "frame_instructions.instructions",
        )
    frames = []
    for index, raw in enumerate(raw_frames):
        path_name = f"frame_instructions.instructions[{index}]"
        if not isinstance(raw, dict):
            raise PipelineError(E_SCHEMA, "Expected an object", path_name)
        frame = raw.get("frame")
        window = raw.get("window")
        pixels = raw.get("sample_pixels")
        if isinstance(frame, bool) or not isinstance(frame, int):
            raise PipelineError(E_SCHEMA, "frame must be an integer", f"{path_name}.frame")
        if isinstance(window, bool) or not isinstance(window, int):
            raise PipelineError(
                E_SCHEMA, "window must be an integer", f"{path_name}.window"
            )
        if not isinstance(pixels, list):
            raise PipelineError(
                E_SCHEMA,
                "sample_pixels must be an array",
                f"{path_name}.sample_pixels",
            )
        parsed_pixels = []
        for pixel_index, pixel in enumerate(pixels):
            if (
                not isinstance(pixel, list)
                or len(pixel) != 2
                or any(
                    isinstance(value, bool) or not isinstance(value, int)
                    for value in pixel
                )
            ):
                raise PipelineError(
                    E_SCHEMA,
                    "Each sample pixel must be [x, y]",
                    f"{path_name}.sample_pixels[{pixel_index}]",
                )
            parsed_pixels.append((pixel[0], pixel[1]))
        frames.append(
            FrameInstruction(
                frame=frame,
                window=window,
                sample_pixels=tuple(parsed_pixels),
            )
        )
    return InstructionSet(
        scene_id=scene_id,
        window_size=window_size,
        window_count=window_count,
        frames=tuple(frames),
    )


def validate_instructions(
    instructions: InstructionSet,
    model: SceneModel,
) -> None:
    if instructions.scene_id != model.scene_id:
        raise PipelineError(
            E_SCHEMA,
            "Instruction scene does not match the loaded model",
            "frame_instructions.scene_id",
        )
    if instructions.window_size != model.timeline.window_size:
        raise PipelineError(
            E_SCHEMA,
            "Instruction window size does not match the model",
            "frame_instructions.window_size",
        )
    if instructions.window_count != model.timeline.window_count:
        raise PipelineError(
            E_SCHEMA,
            "Instruction window count does not match the model",
            "frame_instructions.window_count",
        )
    if [item.frame for item in instructions.frames] != list(
        range(model.timeline.frames)
    ):
        raise PipelineError(
            E_SCHEMA,
            "Instructions must cover every frame exactly once in order",
            "frame_instructions.instructions",
        )
    for index, item in enumerate(instructions.frames):
        expected_window = item.frame // model.timeline.window_size
        if item.window != expected_window:
            raise PipelineError(
                E_SCHEMA,
                "Instruction window does not match the fixed window mapping",
                f"frame_instructions.instructions[{index}].window",
            )
        for pixel_index, (x, y) in enumerate(item.sample_pixels):
            if not (0 <= x < model.canvas.width and 0 <= y < model.canvas.height):
                raise PipelineError(
                    E_SCHEMA,
                    "Sample pixel is outside the canvas",
                    f"frame_instructions.instructions[{index}]."
                    f"sample_pixels[{pixel_index}]",
                )
