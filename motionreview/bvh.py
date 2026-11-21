"""Strict BVH parser and deterministic forward-kinematics evaluator."""

from __future__ import annotations

import math
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable, Sequence

import numpy as np

from .errors import BVHInvalidError
from .hashing import float_hex_array, sha256_json

_CHANNELS = {
    "Xposition",
    "Yposition",
    "Zposition",
    "Zrotation",
    "Xrotation",
    "Yrotation",
}
_POSITION_CHANNELS = {"Xposition", "Yposition", "Zposition"}


@dataclass(frozen=True)
class Joint:
    index: int
    name: str
    parent: int | None
    offset: tuple[float, float, float]
    channels: tuple[str, ...]
    end_offset: tuple[float, float, float] | None = None


@dataclass
class BVHDocument:
    joints: tuple[Joint, ...]
    frame_values: np.ndarray
    frame_time: float
    clip_id: str = "clip"
    source: str | None = None

    def __post_init__(self) -> None:
        validate_joint_table(self.joints, clip_id=self.clip_id)
        values = np.asarray(self.frame_values, dtype=np.float64)
        if values.ndim != 2:
            raise BVHInvalidError(
                "frame data must be a two-dimensional array",
                clip_id=self.clip_id,
            )
        expected = self.channel_count
        if values.shape[1] != expected:
            raise BVHInvalidError(
                "motion channel count does not match hierarchy",
                clip_id=self.clip_id,
                frame=0 if values.shape[0] else None,
                joint=self._joint_at_channel(values.shape[1]),
                details={
                    "expected_channels": expected,
                    "actual_channels": int(values.shape[1]),
                },
            )
        if values.shape[0] < 1:
            raise BVHInvalidError(
                "motion contains no frames",
                clip_id=self.clip_id,
                frame=0,
            )
        if not np.all(np.isfinite(values)):
            bad = np.argwhere(~np.isfinite(values))
            first = bad[0]
            raise BVHInvalidError(
                "motion contains a non-finite channel value",
                clip_id=self.clip_id,
                frame=int(first[0]),
                joint=self._joint_at_channel(int(first[1])),
                details={"channel_index": int(first[1])},
            )
        self.frame_values = values
        self._channel_slices = _build_channel_slices(self.joints)

    @property
    def frame_count(self) -> int:
        return int(self.frame_values.shape[0])

    @property
    def channel_count(self) -> int:
        return sum(len(joint.channels) for joint in self.joints)

    @property
    def channel_names(self) -> tuple[str, ...]:
        return tuple(
            channel for joint in self.joints for channel in joint.channels
        )

    @property
    def content_hash(self) -> str:
        skeleton = [
            {
                "index": joint.index,
                "name": joint.name,
                "parent": joint.parent,
                "offset": [float(v).hex() for v in joint.offset],
                "channels": list(joint.channels),
                "end_offset": (
                    [float(v).hex() for v in joint.end_offset]
                    if joint.end_offset is not None
                    else None
                ),
            }
            for joint in self.joints
        ]
        return sha256_json(
            {
                "schema": "bvh-document-v1",
                "clip_id": self.clip_id,
                "frame_time": float(self.frame_time).hex(),
                "frame_count": self.frame_count,
                "skeleton": skeleton,
                "motion_hex": float_hex_array(self.frame_values),
            }
        )

    def joint_index(self, joint: int | str) -> int:
        if isinstance(joint, int):
            if 0 <= joint < len(self.joints):
                return joint
            raise BVHInvalidError(
                "joint index out of range",
                clip_id=self.clip_id,
                joint=str(joint),
            )
        for item in self.joints:
            if item.name == joint:
                return item.index
        raise BVHInvalidError(
            f"unknown joint: {joint}",
            clip_id=self.clip_id,
            joint=str(joint),
        )

    def channel_slice(self, joint: int | str) -> slice:
        index = self.joint_index(joint)
        return self._channel_slices[index]

    def channel_offset(
        self, joint: int | str, channel: str
    ) -> int:
        joint_index = self.joint_index(joint)
        item = self.joints[joint_index]
        if channel not in item.channels:
            raise BVHInvalidError(
                f"joint '{item.name}' does not declare channel '{channel}'",
                clip_id=self.clip_id,
                joint=item.name,
            )
        return self._channel_slices[joint_index].start + item.channels.index(
            channel
        )

    def _joint_at_channel(self, channel_index: int) -> str | None:
        if not self.joints:
            return None
        total = 0
        for joint in self.joints:
            total += len(joint.channels)
            if channel_index < total:
                return joint.name
        return self.joints[-1].name

    def evaluate_fk(
        self, frames: int | Iterable[int] | None = None
    ) -> np.ndarray:
        return evaluate_fk(self, frames)

    def joint_world_positions(self, frame: int) -> np.ndarray:
        return evaluate_fk(self, frame)[:, :3, 3]

    @classmethod
    def from_data(
        cls,
        joints: Sequence[Joint],
        frame_values: Sequence[Sequence[float]] | np.ndarray,
        frame_time: float,
        *,
        clip_id: str = "clip",
        source: str | None = None,
    ) -> "BVHDocument":
        return cls(
            joints=tuple(joints),
            frame_values=np.asarray(frame_values, dtype=np.float64),
            frame_time=float(frame_time),
            clip_id=clip_id,
            source=source,
        )


def _build_channel_slices(joints: Sequence[Joint]) -> tuple[slice, ...]:
    start = 0
    result: list[slice] = []
    for joint in joints:
        stop = start + len(joint.channels)
        result.append(slice(start, stop))
        start = stop
    return tuple(result)


def validate_joint_table(
    joints: Sequence[Joint], *, clip_id: str | None = None
) -> None:
    """Validate direct joint-table construction as well as parsed input."""

    if not joints:
        raise BVHInvalidError(
            "hierarchy contains no joints", clip_id=clip_id, joint="ROOT"
        )
    names: set[str] = set()
    for expected_index, joint in enumerate(joints):
        if joint.index != expected_index:
            raise BVHInvalidError(
                "joint index is not ordered",
                clip_id=clip_id,
                joint=joint.name,
                details={
                    "declared_index": joint.index,
                    "expected_index": expected_index,
                },
            )
        if not joint.name or any(ch.isspace() for ch in joint.name):
            raise BVHInvalidError(
                "joint name must be non-empty and contain no whitespace",
                clip_id=clip_id,
                joint=joint.name or "<empty>",
            )
        if joint.name in names:
            raise BVHInvalidError(
                "duplicate joint name",
                clip_id=clip_id,
                joint=joint.name,
            )
        names.add(joint.name)
        if expected_index == 0:
            if joint.parent is not None:
                raise BVHInvalidError(
                    "root joint must not have a parent",
                    clip_id=clip_id,
                    joint=joint.name,
                    details={"parent": joint.parent},
                )
        else:
            if joint.parent is None:
                raise BVHInvalidError(
                    "non-root joint is missing a parent",
                    clip_id=clip_id,
                    joint=joint.name,
                )
            if (
                not isinstance(joint.parent, int)
                or joint.parent < 0
                or joint.parent >= expected_index
            ):
                raise BVHInvalidError(
                    "joint parent reference is out of bounds",
                    clip_id=clip_id,
                    joint=joint.name,
                    details={
                        "parent": joint.parent,
                        "valid_range": [0, expected_index - 1],
                    },
                )
        if len(joint.offset) != 3 or not all(
            math.isfinite(float(v)) for v in joint.offset
        ):
            raise BVHInvalidError(
                "joint offset must contain three finite numbers",
                clip_id=clip_id,
                joint=joint.name,
            )
        if not joint.channels:
            raise BVHInvalidError(
                "joint must declare at least one channel",
                clip_id=clip_id,
                joint=joint.name,
            )
        invalid = [ch for ch in joint.channels if ch not in _CHANNELS]
        if invalid:
            raise BVHInvalidError(
                "joint declares unknown channel",
                clip_id=clip_id,
                joint=joint.name,
                details={"invalid_channels": invalid},
            )


def _clean_lines(text: str) -> list[tuple[int, str]]:
    result: list[tuple[int, str]] = []
    for line_number, raw in enumerate(text.splitlines(), 1):
        line = raw.strip()
        if not line:
            continue
        result.append((line_number, line))
    return result


class _BVHParser:
    def __init__(self, lines: list[tuple[int, str]]) -> None:
        self.lines = lines
        self.cursor = 0
        self.joints: list[Joint] = []

    def _next(self) -> tuple[int, str]:
        if self.cursor >= len(self.lines):
            raise BVHInvalidError(
                "unexpected end of BVH input",
                details={"line": self.lines[-1][0] if self.lines else 0},
            )
        item = self.lines[self.cursor]
        self.cursor += 1
        return item

    def _expect_keyword(self, keyword: str) -> tuple[int, str, list[str]]:
        line_number, line = self._next()
        parts = line.split()
        if not parts or parts[0] != keyword:
            raise BVHInvalidError(
                f"expected {keyword}",
                details={"line": line_number, "content": line},
            )
        return line_number, line, parts

    def parse_hierarchy(self) -> None:
        _, _, parts = self._expect_keyword("HIERARCHY")
        _, _, root_parts = self._expect_keyword("ROOT")
        if len(root_parts) != 2:
            raise BVHInvalidError(
                "ROOT must be followed by exactly one joint name"
            )
        self._parse_joint_body(root_parts[1], None)

    def _parse_joint_body(
        self, name: str, parent: int | None
    ) -> None:
        if (
            self.cursor < len(self.lines)
            and self.lines[self.cursor][1] == "{"
        ):
            self.cursor += 1
        else:
            line_number = (
                self.lines[self.cursor][0]
                if self.cursor < len(self.lines)
                else 0
            )
            raise BVHInvalidError(
                "joint block is missing an opening brace",
                joint=name,
                details={"line": line_number},
            )
        index = len(self.joints)
        offset: tuple[float, float, float] | None = None
        channels: tuple[str, ...] | None = None
        end_offset: tuple[float, float, float] | None = None
        joint_added = False

        while True:
            line_number, line = self._next()
            parts = line.split()
            keyword = parts[0]
            if keyword == "OFFSET":
                if len(parts) != 4:
                    raise BVHInvalidError(
                        "OFFSET requires three values",
                        joint=name,
                        details={"line": line_number},
                    )
                try:
                    values = tuple(float(v) for v in parts[1:])
                except ValueError as exc:
                    raise BVHInvalidError(
                        "OFFSET contains a non-numeric value",
                        joint=name,
                        details={"line": line_number},
                    ) from exc
                if not all(math.isfinite(v) for v in values):
                    raise BVHInvalidError(
                        "OFFSET contains a non-finite value",
                        joint=name,
                        details={"line": line_number},
                    )
                offset = (values[0], values[1], values[2])
            elif keyword == "CHANNELS":
                if len(parts) < 2:
                    raise BVHInvalidError(
                        "CHANNELS requires a count",
                        joint=name,
                        details={"line": line_number},
                    )
                try:
                    count = int(parts[1])
                except ValueError as exc:
                    raise BVHInvalidError(
                        "CHANNELS count is not an integer",
                        joint=name,
                        details={"line": line_number},
                    ) from exc
                declared = tuple(parts[2:])
                if count != len(declared):
                    raise BVHInvalidError(
                        "declared channel count does not match channel list",
                        joint=name,
                        details={
                            "declared_count": count,
                            "listed_channels": len(declared),
                            "line": line_number,
                        },
                    )
                channels = declared
            elif keyword == "JOINT":
                if len(parts) != 2:
                    raise BVHInvalidError(
                        "JOINT must be followed by exactly one joint name",
                        joint=name,
                        details={"line": line_number},
                    )
                if not joint_added:
                    self._add_joint(index, name, parent, offset, channels, end_offset)
                    joint_added = True
                self._parse_joint_body(parts[1], len(self.joints) - 1)
            elif keyword == "End":
                if len(parts) != 2 or parts[1] != "Site":
                    raise BVHInvalidError(
                        "expected 'End Site'",
                        joint=name,
                        details={"line": line_number},
                    )
                if (
                    self.cursor < len(self.lines)
                    and self.lines[self.cursor][1] == "{"
                ):
                    self.cursor += 1
                _, _, end_parts = self._expect_keyword("OFFSET")
                if len(end_parts) != 4:
                    raise BVHInvalidError(
                        "End Site OFFSET requires three values",
                        joint=name,
                        details={"line": line_number},
                    )
                try:
                    end_values = tuple(float(v) for v in end_parts[1:])
                except ValueError as exc:
                    raise BVHInvalidError(
                        "End Site OFFSET contains a non-numeric value",
                        joint=name,
                        details={"line": line_number},
                    ) from exc
                end_offset = (
                    end_values[0],
                    end_values[1],
                    end_values[2],
                )
                _, _, close_parts = self._expect_keyword("}")
                if len(close_parts) != 1:
                    raise BVHInvalidError(
                        "unexpected content after End Site block",
                        joint=name,
                        details={"line": line_number},
                    )
            elif keyword == "}":
                if len(parts) != 1:
                    raise BVHInvalidError(
                        "unexpected content after joint close brace",
                        joint=name,
                        details={"line": line_number},
                    )
                if not joint_added:
                    self._add_joint(index, name, parent, offset, channels, end_offset)
                    joint_added = True
                return
            else:
                raise BVHInvalidError(
                    f"unexpected keyword in joint block: {keyword}",
                    joint=name,
                    details={"line": line_number},
                )

    def _add_joint(
        self,
        index: int,
        name: str,
        parent: int | None,
        offset: tuple[float, float, float] | None,
        channels: tuple[str, ...] | None,
        end_offset: tuple[float, float, float] | None,
    ) -> None:
        if offset is None:
            raise BVHInvalidError("joint is missing OFFSET", joint=name)
        if channels is None:
            raise BVHInvalidError("joint is missing CHANNELS", joint=name)
        self.joints.append(
            Joint(
                index=index,
                name=name,
                parent=parent,
                offset=offset,
                channels=channels,
                end_offset=end_offset,
            )
        )


def parse_bvh(text: str, *, clip_id: str | None = None) -> BVHDocument:
    """Parse strict BVH text and validate hierarchy/motion dimensions."""

    lines = _clean_lines(text)
    if not lines:
        raise BVHInvalidError("empty BVH input", clip_id=clip_id)
    parser = _BVHParser(lines)
    parser.parse_hierarchy()
    validate_joint_table(parser.joints, clip_id=clip_id)

    frame_count: int | None = None
    frame_time: float | None = None
    motion_start: int | None = None
    if parser.cursor >= len(lines):
        raise BVHInvalidError(
            "BVH is missing MOTION section", clip_id=clip_id
        )
    line_number, line = lines[parser.cursor]
    parser.cursor += 1
    parts = line.split()
    if len(parts) != 1 or parts[0] not in {"MOTION", "MOTION:"}:
        raise BVHInvalidError(
            "expected MOTION:",
            clip_id=clip_id,
            details={"line": line_number},
        )
    line_number, line = lines[parser.cursor]
    parser.cursor += 1
    match = re.fullmatch(r"Frames:\s*(\d+)", line)
    if not match:
        raise BVHInvalidError(
            "expected 'Frames: <non-negative integer>'",
            clip_id=clip_id,
            details={"line": line_number},
        )
    frame_count = int(match.group(1))
    line_number, line = lines[parser.cursor]
    parser.cursor += 1
    match = re.fullmatch(
        r"Frame Time:\s*([-+0-9.eE]+)", line
    )
    if not match:
        raise BVHInvalidError(
            "expected 'Frame Time: <number>'",
            clip_id=clip_id,
            details={"line": line_number},
        )
    try:
        frame_time = float(match.group(1))
    except ValueError as exc:
        raise BVHInvalidError(
            "Frame Time is not numeric", clip_id=clip_id
        ) from exc
    if not math.isfinite(frame_time) or frame_time <= 0.0:
        raise BVHInvalidError(
            "Frame Time must be a finite positive number", clip_id=clip_id
        )
    motion_start = parser.cursor
    expected_channels = sum(len(j.channels) for j in parser.joints)

    remaining = lines[motion_start:]
    if frame_count <= 0:
        raise BVHInvalidError(
            "Frames must be a positive integer",
            clip_id=clip_id,
            frame=0,
            details={"declared_frames": frame_count},
        )
    if len(remaining) < frame_count:
        first_missing = len(remaining)
        raise BVHInvalidError(
            "frame data is missing",
            clip_id=clip_id,
            frame=first_missing,
            joint=parser.joints[0].name,
            details={
                "declared_frames": frame_count,
                "available_frames": len(remaining),
            },
        )
    if len(remaining) > frame_count:
        raise BVHInvalidError(
            "frame data contains more lines than declared",
            clip_id=clip_id,
            frame=frame_count,
            joint=parser.joints[0].name,
            details={
                "declared_frames": frame_count,
                "available_frames": len(remaining),
            },
        )

    channel_starts: list[tuple[int, Joint]] = []
    start = 0
    for joint in parser.joints:
        channel_starts.append((start, joint))
        start += len(joint.channels)
    frames: list[list[float]] = []
    for frame_index, (_, frame_line) in enumerate(remaining):
        tokens = frame_line.split()
        if len(tokens) != expected_channels:
            offending_joint = parser.joints[0].name
            for channel_start, joint in channel_starts:
                if len(tokens) <= channel_start:
                    offending_joint = joint.name
                    break
                if len(tokens) < channel_start + len(joint.channels):
                    offending_joint = joint.name
                    break
            else:
                offending_joint = parser.joints[-1].name
            raise BVHInvalidError(
                "frame channel count does not match hierarchy",
                clip_id=clip_id,
                frame=frame_index,
                joint=offending_joint,
                details={
                    "expected_channels": expected_channels,
                    "actual_channels": len(tokens),
                },
            )
        try:
            row = [float(token) for token in tokens]
        except ValueError as exc:
            raise BVHInvalidError(
                "frame contains a non-numeric channel value",
                clip_id=clip_id,
                frame=frame_index,
                joint=parser.joints[0].name,
                details={"line": frame_line},
            ) from exc
        if not all(math.isfinite(v) for v in row):
            bad_index = next(
                index for index, value in enumerate(row) if not math.isfinite(value)
            )
            offending = parser.joints[0].name
            for channel_start, joint in channel_starts:
                if channel_start <= bad_index < channel_start + len(joint.channels):
                    offending = joint.name
                    break
            raise BVHInvalidError(
                "frame contains a non-finite channel value",
                clip_id=clip_id,
                frame=frame_index,
                joint=offending,
                details={"channel_index": bad_index},
            )
        frames.append(row)

    actual_clip_id = clip_id or "clip"
    return BVHDocument.from_data(
        parser.joints,
        frames,
        frame_time,
        clip_id=actual_clip_id,
    )


def parse_bvh_file(
    path: str | Path, *, clip_id: str | None = None
) -> BVHDocument:
    file_path = Path(path)
    text = file_path.read_text(encoding="utf-8")
    document = parse_bvh(text, clip_id=clip_id or file_path.stem)
    document.source = str(file_path)
    return document


def _axis_rotation(axis: str, angle: float) -> np.ndarray:
    c = math.cos(angle)
    s = math.sin(angle)
    if axis == "X":
        return np.array(
            [[1.0, 0.0, 0.0], [0.0, c, -s], [0.0, s, c]],
            dtype=np.float64,
        )
    if axis == "Y":
        return np.array(
            [[c, 0.0, s], [0.0, 1.0, 0.0], [-s, 0.0, c]],
            dtype=np.float64,
        )
    return np.array(
        [[c, -s, 0.0], [s, c, 0.0], [0.0, 0.0, 1.0]],
        dtype=np.float64,
    )


def _local_transform(
    joint: Joint, channel_values: np.ndarray
) -> np.ndarray:
    translation = np.array(joint.offset, dtype=np.float64)
    rotation = np.eye(3, dtype=np.float64)
    for channel, value in zip(joint.channels, channel_values, strict=True):
        if channel in _POSITION_CHANNELS:
            translation[{"Xposition": 0, "Yposition": 1, "Zposition": 2}[
                channel
            ]] += float(value)
        else:
            rotation = rotation @ _axis_rotation(channel[0], float(value))
    transform = np.eye(4, dtype=np.float64)
    transform[:3, :3] = rotation
    transform[:3, 3] = translation
    return transform


def evaluate_frame(
    document: BVHDocument, frame: int
) -> np.ndarray:
    if not 0 <= frame < document.frame_count:
        raise BVHInvalidError(
            "frame index out of range",
            clip_id=document.clip_id,
            frame=frame,
            details={"frame_count": document.frame_count},
        )
    return evaluate_channel_values(document, document.frame_values[frame])


def evaluate_channel_values(
    document: BVHDocument, row: np.ndarray
) -> np.ndarray:
    """Evaluate a complete motion row without mutating the document."""

    values = np.asarray(row, dtype=np.float64)
    if values.ndim != 1 or values.shape[0] != document.channel_count:
        raise BVHInvalidError(
            "channel row does not match hierarchy",
            clip_id=document.clip_id,
            details={
                "expected_channels": document.channel_count,
                "actual_shape": list(values.shape),
            },
        )
    result = np.empty((len(document.joints), 4, 4), dtype=np.float64)
    for joint in document.joints:
        local = _local_transform(
            joint, values[document.channel_slice(joint.index)]
        )
        if joint.parent is None:
            result[joint.index] = local
        else:
            result[joint.index] = result[joint.parent] @ local
    return result


def evaluate_fk(
    document: BVHDocument,
    frames: int | Iterable[int] | None = None,
) -> np.ndarray:
    """Return float64 world transforms shaped ``(frames, joints, 4, 4)``.

    For a single integer frame the result has shape ``(joints, 4, 4)``.  The
    evaluator is serial and uses column-vector convention:
    ``world_child = world_parent @ T(offset) @ R(channel order)``.
    """

    if isinstance(frames, int):
        return evaluate_frame(document, frames)
    if frames is None:
        indices = range(document.frame_count)
    elif isinstance(frames, range):
        indices = frames
    else:
        indices = list(frames)
    result = np.empty(
        (len(list(indices)) if not isinstance(indices, range) else len(indices),
         len(document.joints), 4, 4),
        dtype=np.float64,
    )
    for output_index, frame in enumerate(indices):
        if not 0 <= frame < document.frame_count:
            raise BVHInvalidError(
                "frame index out of range",
                clip_id=document.clip_id,
                frame=frame,
                details={"frame_count": document.frame_count},
            )
        result[output_index] = evaluate_frame(document, frame)
    return result


def world_position(transform: np.ndarray) -> np.ndarray:
    return np.asarray(transform[:3, 3], dtype=np.float64)


def joint_world_positions(document: BVHDocument, frame: int) -> np.ndarray:
    return evaluate_frame(document, frame)[:, :3, 3]


def bone_rest_length(joint: Joint) -> float:
    return math.sqrt(sum(float(value) ** 2 for value in joint.offset))
