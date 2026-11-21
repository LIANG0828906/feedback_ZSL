from __future__ import annotations

import math
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable, Iterator, Sequence

from .errors import E_BVH_INVALID, WorkbenchError
from .hashing import content_hash, sha256_bytes
from .math3d import (
    Mat3,
    Quat,
    Vec3,
    mat_identity,
    mat_mul,
    mat_vec,
    quat_axis_angle,
    quat_mul,
    quat_normalize,
    quat_to_mat3,
    v_add,
)


@dataclass(frozen=True)
class Joint:
    name: str
    parent_index: int
    offset: Vec3
    channels: tuple[str, ...]
    channel_start: int
    end_sites: tuple[Vec3, ...] = ()

    @property
    def rotation_channels(self) -> tuple[str, ...]:
        return tuple(
            channel[0].upper()
            for channel in self.channels
            if channel.endswith("rotation")
        )

    @property
    def translation_channel_names(self) -> tuple[str, ...]:
        return tuple(
            channel for channel in self.channels if channel.endswith("position")
        )

    @property
    def rotation_order(self) -> str:
        return "".join(self.rotation_channels)


@dataclass(frozen=True)
class FrameTransform:
    frame: int
    world_positions: tuple[Vec3, ...]
    world_rotations: tuple[Mat3, ...]
    end_site_positions: tuple[Vec3, ...]

    def to_dict(self) -> dict:
        return {
            "frame": self.frame,
            "world_positions": [list(position) for position in self.world_positions],
            "world_rotations": [list(matrix) for matrix in self.world_rotations],
            "end_site_positions": [list(position) for position in self.end_site_positions],
        }


@dataclass(frozen=True)
class BVHData:
    joints: tuple[Joint, ...]
    frames: tuple[tuple[float, ...], ...]
    frame_time: float
    source_hash: str
    content_digest: str
    joint_by_name: dict[str, int]

    @property
    def frame_count(self) -> int:
        return len(self.frames)

    @property
    def channel_count(self) -> int:
        return sum(len(joint.channels) for joint in self.joints)

    def base_local_rotation(self, joint_index: int, frame: int) -> Quat:
        joint = self.joints[joint_index]
        values = self.frames[frame]
        result = (1.0, 0.0, 0.0, 0.0)
        cursor = joint.channel_start
        for channel in joint.channels:
            value = values[cursor]
            cursor += 1
            if channel.endswith("rotation"):
                axis = channel[0].upper()
                result = quat_mul(
                    result, quat_axis_angle(axis, math.radians(value))
                )
        return quat_normalize(result)

    def base_local_translation(self, joint_index: int, frame: int) -> Vec3:
        joint = self.joints[joint_index]
        values = self.frames[frame]
        translation = list(joint.offset)
        cursor = joint.channel_start
        for channel in joint.channels:
            value = values[cursor]
            cursor += 1
            if channel.endswith("position"):
                axis = channel[0].upper()
                index = {"X": 0, "Y": 1, "Z": 2}[axis]
                translation[index] += value
        return (translation[0], translation[1], translation[2])

    def base_pose_arrays(
        self, frame: int
    ) -> tuple[tuple[Quat, ...], tuple[Vec3, ...]]:
        rotations = tuple(
            self.base_local_rotation(index, frame)
            for index in range(len(self.joints))
        )
        translations = tuple(
            self.base_local_translation(index, frame)
            for index in range(len(self.joints))
        )
        return rotations, translations

    def evaluate(
        self,
        frame: int,
        rotations: Sequence[Quat] | None = None,
        scales: Sequence[float] | None = None,
        translations: Sequence[Vec3] | None = None,
    ) -> FrameTransform:
        if frame < 0 or frame >= len(self.frames):
            raise WorkbenchError(
                E_BVH_INVALID,
                "Frame index out of range",
                {"joint": None, "frame": frame},
            )
        joint_count = len(self.joints)
        if rotations is None or translations is None:
            base_rotations, base_translations = self.base_pose_arrays(frame)
            rotations = base_rotations if rotations is None else rotations
            translations = (
                base_translations if translations is None else translations
            )
        if len(rotations) != joint_count or len(translations) != joint_count:
            raise ValueError("Pose arrays do not match skeleton")
        if scales is None:
            scales = (1.0,) * joint_count
        if len(scales) != joint_count:
            raise ValueError("Scale array does not match skeleton")

        positions: list[Vec3] = [(0.0, 0.0, 0.0)] * joint_count
        world_rotations: list[Mat3] = [mat_identity()] * joint_count
        for index, joint in enumerate(self.joints):
            local_rotation = quat_to_mat3(quat_normalize(rotations[index]))
            local_translation = translations[index]
            scale = scales[index]
            parent = joint.parent_index
            if parent < 0:
                parent_rotation = mat_identity()
                parent_position = (0.0, 0.0, 0.0)
            else:
                parent_rotation = world_rotations[parent]
                parent_position = positions[parent]
            positions[index] = v_add(
                parent_position,
                mat_vec(parent_rotation, local_translation) if scale == 1.0 else
                mat_vec(
                    parent_rotation,
                    (
                        local_translation[0] * scale,
                        local_translation[1] * scale,
                        local_translation[2] * scale,
                    ),
                ),
            )
            world_rotations[index] = mat_mul(parent_rotation, local_rotation)

        end_positions: list[Vec3] = []
        for index, joint in enumerate(self.joints):
            for end_offset in joint.end_sites:
                end_positions.append(
                    v_add(positions[index], mat_vec(world_rotations[index], end_offset))
                )

        return FrameTransform(
            frame=frame,
            world_positions=tuple(positions),
            world_rotations=tuple(world_rotations),
            end_site_positions=tuple(end_positions),
        )

    def endpoint_position(
        self,
        frame: int,
        joint_index: int,
        rotations: Sequence[Quat],
        scales: Sequence[float],
        translations: Sequence[Vec3],
    ) -> Vec3:
        positions = [None] * len(self.joints)
        world_rotations: list[Mat3] = [mat_identity()] * len(self.joints)
        for index, joint in enumerate(self.joints):
            local_rotation = quat_to_mat3(quat_normalize(rotations[index]))
            local_translation = translations[index]
            scale = scales[index]
            parent = joint.parent_index
            if parent < 0:
                parent_rotation = mat_identity()
                parent_position = (0.0, 0.0, 0.0)
            else:
                parent_rotation = world_rotations[parent]
                parent_position = positions[parent]
                if parent_position is None:
                    raise AssertionError("Parent transform missing")
            positions[index] = v_add(
                parent_position,
                mat_vec(parent_rotation, (
                    local_translation[0] * scale,
                    local_translation[1] * scale,
                    local_translation[2] * scale,
                )),
            )
            world_rotations[index] = mat_mul(parent_rotation, local_rotation)
        result = positions[joint_index]
        if result is None:
            raise AssertionError("Joint transform missing")
        return result

    def structural_dict(self) -> dict:
        return {
            "joints": [
                {
                    "name": joint.name,
                    "parent_index": joint.parent_index,
                    "offset": list(joint.offset),
                    "channels": list(joint.channels),
                    "end_sites": [list(end) for end in joint.end_sites],
                }
                for joint in self.joints
            ],
            "frames": [list(frame) for frame in self.frames],
            "frame_time": self.frame_time,
        }


def _bvh_error(
    message: str,
    *,
    joint: str | None = None,
    joint_index: int | None = None,
    frame: int | None = None,
    **extra,
) -> WorkbenchError:
    details = {
        "joint": joint,
        "joint_index": joint_index,
        "frame": frame,
    }
    details.update(extra)
    return WorkbenchError(E_BVH_INVALID, message, details)


def _tokenize_hierarchy(lines: Sequence[str]) -> list[str]:
    tokens: list[str] = []
    for line in lines:
        if "#" in line:
            line = line.split("#", 1)[0]
        tokens.extend(line.replace("{", " { ").replace("}", " } ").split())
    return tokens


def _parse_float(token: str, *, joint: str, kind: str) -> float:
    try:
        value = float(token)
    except ValueError as exc:
        raise _bvh_error(
            f"Invalid numeric value for {joint} {kind}",
            joint=joint,
            frame=None,
            value=token,
        ) from exc
    if not math.isfinite(value):
        raise _bvh_error(
            f"Non-finite numeric value for {joint} {kind}",
            joint=joint,
            frame=None,
            value=token,
        )
    return value


def parse_bvh_text(
    text: str, *, source_hash: str | None = None
) -> BVHData:
    raw_bytes = text.encode("utf-8")
    if source_hash is None:
        source_hash = sha256_bytes(raw_bytes)
    lines = text.splitlines()
    motion_line = None
    for index, line in enumerate(lines):
        if line.strip().upper().startswith("MOTION"):
            motion_line = index
            break
    if motion_line is None:
        raise _bvh_error(
            "MOTION section missing", joint="MOTION", frame=None
        )

    hierarchy_tokens = _tokenize_hierarchy(lines[:motion_line])
    iterator = iter(hierarchy_tokens)
    joints: list[Joint] = []
    joint_by_name: dict[str, int] = {}
    stack: list[int] = []
    pending_explicit_parents: list[tuple[int, int, str]] = []
    current_joint: int | None = None
    seen_root = False
    joints_with_offset: set[int] = set()

    def next_token(context: str) -> str:
        try:
            return next(iterator)
        except StopIteration as exc:
            raise _bvh_error(
                f"Unexpected end of hierarchy near {context}",
                joint=context,
                frame=None,
            ) from exc

    while True:
        try:
            token = next(iterator)
        except StopIteration:
            break
        upper = token.upper()
        if upper == "HIERARCHY":
            continue
        if upper == "ROOT":
            if seen_root:
                raise _bvh_error(
                    "Multiple ROOT declarations", joint="HIERARCHY", frame=None
                )
            seen_root = True
        if upper in {"ROOT", "JOINT"}:
            name = next_token(upper)
            if name in joint_by_name:
                raise _bvh_error(
                    "Duplicate joint name",
                    joint=name,
                    joint_index=len(joints),
                    frame=None,
                )
            parent_index = stack[-1] if stack else -1
            if upper == "ROOT" and stack:
                raise _bvh_error(
                    "ROOT cannot be nested inside another joint",
                    joint=name,
                    frame=None,
                )
            if upper == "JOINT" and not stack:
                raise _bvh_error(
                    "JOINT declared without a ROOT", joint=name, frame=None
                )
            explicit_parent: int | None = None
            lookahead = next_token(name)
            if lookahead.upper() == "PARENT":
                parent_token = next_token(name)
                try:
                    explicit_parent = int(parent_token)
                except ValueError as exc:
                    raise _bvh_error(
                        "Explicit parent reference is not an integer",
                        joint=name,
                        frame=None,
                        parent_reference=parent_token,
                    ) from exc
                lookahead = next_token(name)
            if lookahead != "{":
                raise _bvh_error(
                    "Expected '{' after joint declaration",
                    joint=name,
                    frame=None,
                    token=lookahead,
                )
            joint_index = len(joints)
            if explicit_parent is not None:
                pending_explicit_parents.append(
                    (joint_index, explicit_parent, name)
                )
            joints.append(
                Joint(
                    name=name,
                    parent_index=parent_index,
                    offset=(0.0, 0.0, 0.0),
                    channels=(),
                    channel_start=0,
                )
            )
            joint_by_name[name] = joint_index
            stack.append(joint_index)
            current_joint = joint_index
            continue

        if upper == "END":
            site = next_token("End Site")
            if site.upper() != "SITE":
                raise _bvh_error(
                    "Expected SITE after END", joint="End Site", frame=None
                )
            if next_token("End Site") != "{":
                raise _bvh_error(
                    "Expected '{' after End Site", joint="End Site", frame=None
                )
            if not stack:
                raise _bvh_error(
                    "End Site outside a joint", joint="End Site", frame=None
                )
            offset = (0.0, 0.0, 0.0)
            closed = False
            end_site_has_offset = False
            while True:
                item = next_token("End Site")
                if item == "}":
                    closed = True
                    break
                if item.upper() == "OFFSET":
                    end_site_has_offset = True
                    offset = (
                        _parse_float(next_token("End Site"), joint="End Site", kind="offset"),
                        _parse_float(next_token("End Site"), joint="End Site", kind="offset"),
                        _parse_float(next_token("End Site"), joint="End Site", kind="offset"),
                    )
                else:
                    raise _bvh_error(
                        "Unexpected token in End Site",
                        joint="End Site",
                        frame=None,
                        token=item,
                    )
            if not closed:
                raise _bvh_error(
                    "Unterminated End Site", joint="End Site", frame=None
                )
            if not end_site_has_offset:
                raise _bvh_error(
                    "End Site OFFSET is missing", joint="End Site", frame=None
                )
            target = joints[stack[-1]]
            joints[stack[-1]] = Joint(
                name=target.name,
                parent_index=target.parent_index,
                offset=target.offset,
                channels=target.channels,
                channel_start=target.channel_start,
                end_sites=target.end_sites + (offset,),
            )
            continue

        if upper == "OFFSET":
            if current_joint is None or not stack or current_joint != stack[-1]:
                raise _bvh_error(
                    "OFFSET outside a joint", joint="OFFSET", frame=None
                )
            joint = joints[current_joint]
            offset = (
                _parse_float(next_token(joint.name), joint=joint.name, kind="offset"),
                _parse_float(next_token(joint.name), joint=joint.name, kind="offset"),
                _parse_float(next_token(joint.name), joint=joint.name, kind="offset"),
            )
            joints[current_joint] = Joint(
                name=joint.name,
                parent_index=joint.parent_index,
                offset=offset,
                channels=joint.channels,
                channel_start=joint.channel_start,
                end_sites=joint.end_sites,
            )
            joints_with_offset.add(current_joint)
            continue

        if upper == "CHANNELS":
            if current_joint is None or not stack or current_joint != stack[-1]:
                raise _bvh_error(
                    "CHANNELS outside a joint", joint="CHANNELS", frame=None
                )
            count_token = next_token(joints[current_joint].name)
            try:
                count = int(count_token)
            except ValueError as exc:
                raise _bvh_error(
                    "Channel count is not an integer",
                    joint=joints[current_joint].name,
                    frame=None,
                ) from exc
            if count < 0:
                raise _bvh_error(
                    "Negative channel count",
                    joint=joints[current_joint].name,
                    frame=None,
                )
            channels: list[str] = []
            for _ in range(count):
                channel = next_token(joints[current_joint].name)
                normalized = channel[0].upper() + channel[1:].lower()
                if normalized not in {
                    "Xrotation",
                    "Yrotation",
                    "Zrotation",
                    "Xposition",
                    "Yposition",
                    "Zposition",
                }:
                    raise _bvh_error(
                        "Unsupported channel",
                        joint=joints[current_joint].name,
                        frame=None,
                        channel=channel,
                    )
                channels.append(normalized)
            joint = joints[current_joint]
            joints[current_joint] = Joint(
                name=joint.name,
                parent_index=joint.parent_index,
                offset=joint.offset,
                channels=tuple(channels),
                channel_start=joint.channel_start,
                end_sites=joint.end_sites,
            )
            continue

        if token == "{":
            continue
        if token == "}":
            if not stack:
                raise _bvh_error(
                    "Unmatched closing brace", joint="HIERARCHY", frame=None
                )
            stack.pop()
            current_joint = stack[-1] if stack else None
            continue
        raise _bvh_error(
            "Unexpected hierarchy token",
            joint="HIERARCHY",
            frame=None,
            token=token,
        )

    if not seen_root or not joints:
        raise _bvh_error(
            "No ROOT joint found", joint="HIERARCHY", frame=None
        )
    if stack:
        raise _bvh_error(
            "Unclosed joint block",
            joint=joints[stack[-1]].name,
            joint_index=stack[-1],
            frame=None,
        )
    for index, joint in enumerate(joints):
        if index not in joints_with_offset:
            raise _bvh_error(
                "Joint OFFSET is missing",
                joint=joint.name,
                joint_index=index,
                frame=None,
            )

    for joint_index, explicit_parent, name in pending_explicit_parents:
        if explicit_parent < 0 or explicit_parent >= len(joints):
            raise _bvh_error(
                "Parent reference out of range",
                joint=name,
                joint_index=joint_index,
                frame=None,
                parent_reference=explicit_parent,
                joint_count=len(joints),
            )
        if explicit_parent != joints[joint_index].parent_index:
            raise _bvh_error(
                "Explicit parent reference disagrees with hierarchy",
                joint=name,
                joint_index=joint_index,
                frame=None,
                parent_reference=explicit_parent,
                structural_parent=joints[joint_index].parent_index,
            )

    channel_start = 0
    normalized_joints: list[Joint] = []
    for index, joint in enumerate(joints):
        parent = joint.parent_index
        if parent < -1 or parent >= len(joints):
            raise _bvh_error(
                "Joint parent reference out of range",
                joint=joint.name,
                joint_index=index,
                frame=None,
                parent_reference=parent,
            )
        if index > 0 and parent >= index:
            raise _bvh_error(
                "Joint parent must precede child",
                joint=joint.name,
                joint_index=index,
                frame=None,
                parent_reference=parent,
            )
        normalized_joints.append(
            Joint(
                name=joint.name,
                parent_index=parent,
                offset=joint.offset,
                channels=joint.channels,
                channel_start=channel_start,
                end_sites=joint.end_sites,
            )
        )
        channel_start += len(joint.channels)
    joints = normalized_joints
    total_channels = channel_start

    motion_lines = [line.strip() for line in lines[motion_line + 1 :]]
    frame_count: int | None = None
    frame_time: float | None = None
    frame_values: list[tuple[float, ...]] = []
    for line in motion_lines:
        if not line:
            continue
        if line.lower().startswith("frames:"):
            if frame_count is not None:
                raise _bvh_error(
                    "Duplicate Frames declaration",
                    joint="MOTION",
                    frame=None,
                )
            try:
                frame_count = int(line.split(":", 1)[1].strip())
            except ValueError as exc:
                raise _bvh_error(
                    "Invalid Frames declaration",
                    joint="MOTION",
                    frame=None,
                ) from exc
            continue
        if line.lower().startswith("frame time:"):
            try:
                frame_time = float(line.split(":", 1)[1].strip())
            except ValueError as exc:
                raise _bvh_error(
                    "Invalid Frame Time declaration",
                    joint="MOTION",
                    frame=None,
                ) from exc
            continue
        if frame_count is None or frame_time is None:
            raise _bvh_error(
                "Motion data appears before Frames and Frame Time",
                joint="MOTION",
                frame=len(frame_values),
            )
        tokens = line.split()
        frame_index = len(frame_values)
        if len(tokens) != total_channels:
            failing_joint = "MOTION"
            failing_channel = min(len(tokens), total_channels)
            if len(tokens) < total_channels:
                for joint in joints:
                    end = joint.channel_start + len(joint.channels)
                    if len(tokens) < end:
                        failing_joint = joint.name
                        failing_channel = len(tokens) - joint.channel_start
                        break
            else:
                if joints:
                    failing_joint = joints[-1].name
                    failing_channel = total_channels - joints[-1].channel_start
            raise _bvh_error(
                "Channel value count does not match channel declaration",
                joint=failing_joint,
                frame=frame_index,
                expected_channels=total_channels,
                actual_channels=len(tokens),
                channel_index=max(0, failing_channel),
            )
        try:
            values = tuple(float(token) for token in tokens)
        except ValueError as exc:
            for channel_index, token in enumerate(tokens):
                try:
                    float(token)
                except ValueError:
                    owner = joints[-1]
                    for joint in joints:
                        if channel_index < joint.channel_start + len(joint.channels):
                            owner = joint
                            break
                    raise _bvh_error(
                        "Non-numeric channel value",
                        joint=owner.name,
                        frame=frame_index,
                        channel_index=channel_index - owner.channel_start,
                    ) from exc
            raise
        if not all(math.isfinite(value) for value in values):
            raise _bvh_error(
                "Non-finite channel value",
                joint="MOTION",
                frame=frame_index,
            )
        frame_values.append(values)

    if frame_count is None:
        raise _bvh_error(
            "Frames declaration missing", joint="MOTION", frame=None
        )
    if frame_time is None:
        raise _bvh_error(
            "Frame Time declaration missing", joint="MOTION", frame=None
        )
    if frame_count < 0:
        raise _bvh_error(
            "Negative frame count", joint="MOTION", frame=None
        )
    if frame_time <= 0.0 or not math.isfinite(frame_time):
        raise _bvh_error(
            "Frame Time must be positive and finite",
            joint="MOTION",
            frame=None,
        )
    if len(frame_values) < frame_count:
        raise _bvh_error(
            "Declared frame count exceeds available motion frames",
            joint="MOTION",
            frame=len(frame_values),
            expected_frames=frame_count,
            actual_frames=len(frame_values),
        )
    if len(frame_values) > frame_count:
        raise _bvh_error(
            "Motion contains more frames than declared",
            joint="MOTION",
            frame=frame_count,
            expected_frames=frame_count,
            actual_frames=len(frame_values),
        )

    data = BVHData(
        joints=tuple(joints),
        frames=tuple(frame_values),
        frame_time=frame_time,
        source_hash=source_hash,
        content_digest="",
        joint_by_name=dict(joint_by_name),
    )
    digest = content_hash(data.structural_dict())
    return BVHData(
        joints=data.joints,
        frames=data.frames,
        frame_time=data.frame_time,
        source_hash=source_hash,
        content_digest=digest,
        joint_by_name=data.joint_by_name,
    )


def load_bvh(path: str | Path) -> BVHData:
    path = Path(path)
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise _bvh_error(
            "Unable to read BVH file",
            joint=None,
            frame=None,
            path=str(path),
            reason=str(exc),
        ) from exc
    try:
        text = raw.decode("utf-8-sig")
    except UnicodeDecodeError as exc:
        raise _bvh_error(
            "BVH file is not UTF-8",
            joint=None,
            frame=None,
            path=str(path),
        ) from exc
    return parse_bvh_text(text, source_hash=sha256_bytes(raw))
