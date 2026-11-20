"""Strict local BVH parser and deterministic forward-kinematics evaluator."""

from __future__ import annotations

import math
from collections.abc import Iterator, Sequence
from dataclasses import dataclass, field
from pathlib import Path

import numpy as np

from .errors import E_BVH_INVALID, E_IO, BusinessError
from .math3d import (
    canonical_json_bytes,
    euler_matrix,
    hash_pose,
    hash_text,
    rotation_vector_to_matrix,
    transform_matrix,
)

ROTATION_AXES = ("x", "y", "z")
CHANNEL_NAMES = {
    "xrotation": "x",
    "yrotation": "y",
    "zrotation": "z",
    "xposition": "x",
    "yposition": "y",
    "zposition": "z",
}


def _fail(message: str, **details: object) -> None:
    raise BusinessError(E_BVH_INVALID, message, details)


@dataclass(frozen=True)
class BvhJoint:
    name: str
    parent: int | None
    offset: np.ndarray
    channels: tuple[str, ...] = ()
    channel_start: int = 0
    is_end_site: bool = False
    rotation_channels: tuple[tuple[str, int], ...] = ()
    position_channels: tuple[tuple[str, int], ...] = ()

    @property
    def child_slot(self) -> int:
        return self.channel_start


@dataclass(frozen=True)
class Pose:
    world: np.ndarray
    local_rotations: np.ndarray
    joint_positions: np.ndarray

    def copy(self) -> Pose:
        return Pose(
            self.world.copy(),
            self.local_rotations.copy(),
            self.joint_positions.copy(),
        )


@dataclass
class BvhDocument:
    path: str
    joints: tuple[BvhJoint, ...]
    frame_time: float
    frame_data: np.ndarray
    source_sha256: str = ""
    _rotation_order: dict[int, tuple[str, ...]] = field(default_factory=dict)
    _translation_base: np.ndarray | None = field(default=None, repr=False)
    _channel_values_cache: dict[int, np.ndarray] = field(default_factory=dict, repr=False)

    @property
    def frame_count(self) -> int:
        return int(self.frame_data.shape[0])

    @property
    def channel_count(self) -> int:
        return int(self.frame_data.shape[1])

    @property
    def joint_count(self) -> int:
        return len(self.joints)

    @property
    def joint_names(self) -> tuple[str, ...]:
        return tuple(j.name for j in self.joints)

    @property
    def root_index(self) -> int:
        for index, joint in enumerate(self.joints):
            if joint.parent is None:
                return index
        raise AssertionError("validated BVH has no root")

    def skeleton_fingerprint(self) -> str:
        payload = {
            "joints": [
                {
                    "name": joint.name,
                    "parent": joint.parent,
                    "offset": [float(x) for x in joint.offset],
                    "channels": list(joint.channels),
                    "end": joint.is_end_site,
                }
                for joint in self.joints
            ],
            "frame_time": self.frame_time,
        }
        return hash_text(canonical_json_bytes(payload).decode("utf-8"))

    def rotation_order(self, joint_index: int) -> tuple[str, ...]:
        order = self._rotation_order.get(joint_index)
        if order is None:
            order = tuple(axis for axis, _ in self.joints[joint_index].rotation_channels)
            self._rotation_order[joint_index] = order
        return order

    def _translation_table(self) -> np.ndarray:
        if self._translation_base is None:
            table = np.empty((len(self.joints), 3), dtype=np.float64)
            for index, joint in enumerate(self.joints):
                table[index] = joint.offset
            self._translation_base = table
        return self._translation_base

    def channel_vector(self, frame_index: int) -> np.ndarray:
        idx = int(frame_index)
        if idx < 0 or idx >= self.frame_count:
            _fail(
                "frame index is outside the motion",
                frame=idx,
                frame_count=self.frame_count,
            )
        cached = self._channel_values_cache.get(idx)
        if cached is None:
            cached = np.array(self.frame_data[idx], copy=True)
            if len(self._channel_values_cache) > 64:
                self._channel_values_cache.clear()
            self._channel_values_cache[idx] = cached
        return cached

    def base_rotations_and_translations(
        self, frame_index: int
    ) -> tuple[np.ndarray, np.ndarray]:
        values = self.channel_vector(frame_index)
        count = len(self.joints)
        rotations = np.empty((count, 3, 3), dtype=np.float64)
        rotations[:] = np.eye(3, dtype=np.float64)
        translations = np.array(self._translation_table(), copy=True)
        for index, joint in enumerate(self.joints):
            if joint.rotation_channels:
                angles = [float(values[joint.channel_start + slot]) for _, slot in joint.rotation_channels]
                rotations[index] = euler_matrix(self.rotation_order(index), angles)
            for axis, slot in joint.position_channels:
                translations[index, ROTATION_AXES.index(axis)] += float(
                    values[joint.channel_start + slot]
                )
        return rotations, translations

    def evaluate(
        self,
        frame_index: int,
        delta_rotation_vectors: np.ndarray | None = None,
    ) -> Pose:
        base_rotations, translations = self.base_rotations_and_translations(frame_index)
        return self.evaluate_with_state(
            base_rotations,
            translations,
            delta_rotation_vectors=delta_rotation_vectors,
        )

    def evaluate_with_state(
        self,
        base_rotations: np.ndarray,
        translations: np.ndarray,
        delta_rotation_vectors: np.ndarray | None = None,
    ) -> Pose:
        count = len(self.joints)
        local_rotations = np.array(base_rotations, dtype=np.float64, copy=True)
        if delta_rotation_vectors is not None:
            deltas = np.asarray(delta_rotation_vectors, dtype=np.float64)
            if deltas.shape != (count, 3):
                _fail(
                    "rotation delta shape does not match the skeleton",
                    expected=[count, 3],
                    actual=list(deltas.shape),
                )
            for index in range(count):
                if np.any(deltas[index]):
                    local_rotations[index] = (
                        base_rotations[index] @ rotation_vector_to_matrix(deltas[index])
                    )
        world = np.empty((count, 4, 4), dtype=np.float64)
        positions = np.empty((count, 3), dtype=np.float64)
        for index, joint in enumerate(self.joints):
            local = transform_matrix(local_rotations[index], translations[index])
            if joint.parent is None:
                world[index] = local
            else:
                world[index] = world[joint.parent] @ local
            positions[index] = world[index, :3, 3]
        return Pose(world=world, local_rotations=local_rotations, joint_positions=positions)

    def pose_hash(self, pose: Pose) -> str:
        return hash_pose(self.joint_names, pose.world)

    def frame_hash(self, frame_index: int) -> str:
        return self.pose_hash(self.evaluate(frame_index))

    def ancestor_chain(self, joint_index: int) -> tuple[int, ...]:
        """Return ancestors from root to the joint (inclusive)."""

        out: list[int] = []
        current: int | None = int(joint_index)
        while current is not None:
            out.append(current)
            current = self.joints[current].parent
        out.reverse()
        return tuple(out)

    def is_descendant(self, joint_index: int, ancestor_index: int) -> bool:
        current: int | None = int(joint_index)
        while current is not None:
            if current == int(ancestor_index):
                return True
            current = self.joints[current].parent
        return False

    def descendents_of(self, joint_index: int) -> Iterator[int]:
        for index in range(len(self.joints)):
            if self.is_descendant(index, joint_index):
                yield index


class _TokenStream:
    def __init__(self, tokens: Sequence[tuple[str, int]]):
        self.tokens = tokens
        self.index = 0

    def peek(self) -> tuple[str, int] | None:
        if self.index >= len(self.tokens):
            return None
        return self.tokens[self.index]

    def take(self, expected: str | None = None) -> tuple[str, int]:
        if self.index >= len(self.tokens):
            _fail("unexpected end of BVH hierarchy")
        token, line = self.tokens[self.index]
        self.index += 1
        if expected is not None and token != expected:
            _fail(
                f"expected {expected!r} in hierarchy",
                line=line,
                token=token,
            )
        return token, line

    def take_float(self, label: str) -> tuple[float, int]:
        token, line = self.take()
        try:
            value = float(token)
        except ValueError:
            _fail(f"expected numeric {label}", line=line, token=token)
        if not math.isfinite(value):
            _fail(f"non-finite {label}", line=line, token=token)
        return value, line


def _strip_line(line: str) -> str:
    # BVH exporters occasionally append comments.  Keep numeric data intact.
    for marker in ("#", "//"):
        pos = line.find(marker)
        if pos >= 0:
            line = line[:pos]
    return line.strip()


def parse_bvh_text(text: str, path: str = "<memory>") -> BvhDocument:
    lines = text.splitlines()
    cleaned: list[tuple[str, int]] = []
    for number, raw in enumerate(lines, start=1):
        line = _strip_line(raw)
        if not line:
            continue
        cleaned.append((line, number))

    hierarchy_tokens: list[tuple[str, int]] = []
    motion_header: list[tuple[str, int]] = []
    motion_rows: list[str] = []
    seen_motion = False
    for line, number in cleaned:
        if line.upper() == "MOTION":
            seen_motion = True
            continue
        if not seen_motion:
            for token in line.split():
                hierarchy_tokens.append((token, number))
        elif line.upper().startswith("FRAMES:") or line.upper().startswith("FRAME TIME:"):
            motion_header.append((line, number))
        else:
            motion_rows.append(line)

    if not seen_motion:
        _fail("BVH is missing MOTION section", path=path)
    if not motion_header:
        _fail("BVH is missing Frames/Frame Time header", path=path)

    declared_frames: int | None = None
    frame_time: float | None = None
    for line, number in motion_header:
        upper = line.upper()
        if upper.startswith("FRAMES:"):
            raw = line.split(":", 1)[1].strip()
            try:
                declared_frames = int(raw)
            except ValueError:
                _fail("invalid Frames header", path=path, line=number, value=raw)
            if declared_frames < 0:
                _fail("frame count must not be negative", path=path, line=number)
        elif upper.startswith("FRAME TIME:"):
            raw = line.split(":", 1)[1].strip()
            try:
                frame_time = float(raw)
            except ValueError:
                _fail("invalid Frame Time header", path=path, line=number, value=raw)
            if not math.isfinite(frame_time) or frame_time <= 0.0:
                _fail("Frame Time must be positive and finite", path=path, line=number)
    if declared_frames is None or frame_time is None:
        _fail("BVH is missing a complete motion header", path=path)

    if not hierarchy_tokens:
        _fail("BVH hierarchy is empty", path=path)
    stream = _TokenStream(hierarchy_tokens)
    token, _ = stream.take()
    if token.upper() != "HIERARCHY":
        _fail("BVH must start with HIERARCHY", path=path)

    names: dict[str, int] = {}
    joints: list[BvhJoint] = []
    total_channels = 0
    end_counter = 0

    def unique_name(candidate: str) -> str:
        if candidate not in names:
            return candidate
        suffix = 2
        while f"{candidate}_{suffix}" in names:
            suffix += 1
        return f"{candidate}_{suffix}"

    def parse_node(parent: int | None) -> int:
        nonlocal total_channels, end_counter
        token, line = stream.take()
        is_end = False
        if token.upper() == "ROOT":
            if parent is not None:
                _fail("ROOT cannot be nested", line=line)
            name_token, name_line = stream.take()
            name = name_token
        elif token.upper() == "JOINT":
            if parent is None:
                _fail("JOINT cannot appear without ROOT", line=line)
            name_token, name_line = stream.take()
            name = name_token
        elif token.upper() == "END":
            site_token, site_line = stream.take()
            if site_token.upper() != "SITE":
                _fail("expected End Site", line=site_line, token=site_token)
            is_end = True
            name = unique_name(f"{joints[parent].name}_EndSite" if parent is not None else f"EndSite_{end_counter}")
            end_counter += 1
            name_line = line
        else:
            _fail("expected ROOT, JOINT or End Site", line=line, token=token)

        if name in names:
            _fail("duplicate joint name", name=name, line=name_line)
        index = len(joints)
        stream.take("{")

        offset: np.ndarray | None = None
        channels: tuple[str, ...] = ()
        channel_start = total_channels
        closed = False
        while not closed:
            next_token = stream.peek()
            if next_token is None:
                _fail("unterminated joint block", joint=name)
            word, word_line = next_token
            upper = word.upper()
            if upper == "}":
                stream.take()
                closed = True
                break
            if upper == "OFFSET":
                stream.take()
                values = [stream.take_float("OFFSET")[0] for _ in range(3)]
                if offset is not None:
                    _fail("duplicate OFFSET", joint=name, line=word_line)
                offset = np.array(values, dtype=np.float64)
                continue
            if upper == "CHANNELS":
                if is_end:
                    _fail("End Site cannot declare CHANNELS", joint=name, line=word_line)
                stream.take()
                count_token, count_line = stream.take()
                try:
                    channel_count = int(count_token)
                except ValueError:
                    _fail("invalid CHANNELS count", line=count_line, token=count_token)
                if channel_count <= 0:
                    _fail("CHANNELS count must be positive", line=count_line)
                raw_channels: list[str] = []
                for _ in range(channel_count):
                    channel_token, channel_line = stream.take()
                    key = channel_token.lower()
                    if key not in CHANNEL_NAMES:
                        _fail(
                            "unknown motion channel",
                            line=channel_line,
                            token=channel_token,
                        )
                    raw_channels.append(channel_token)
                channels = tuple(raw_channels)
                total_channels += channel_count
                continue
            if upper in ("ROOT", "JOINT", "END"):
                break
            _fail("unexpected token in joint block", joint=name, line=word_line, token=word)

        if offset is None:
            _fail("joint is missing OFFSET", joint=name)
        if not is_end and not channels:
            _fail("joint is missing CHANNELS", joint=name)

        rot_channels: list[tuple[str, int]] = []
        pos_channels: list[tuple[str, int]] = []
        seen_rot: set[str] = set()
        seen_pos: set[str] = set()
        for slot, raw in enumerate(channels):
            key = CHANNEL_NAMES[raw.lower()]
            if "rotation" in raw.lower():
                if key in seen_rot:
                    _fail("duplicate rotation channel", joint=name, axis=key)
                seen_rot.add(key)
                rot_channels.append((key, slot))
            else:
                if key in seen_pos:
                    _fail("duplicate position channel", joint=name, axis=key)
                seen_pos.add(key)
                pos_channels.append((key, slot))
        if not is_end and seen_rot != {"x", "y", "z"}:
            _fail(
                "joint must declare one rotation channel per axis",
                joint=name,
                rotations=sorted(seen_rot),
            )
        if is_end and channels:
            _fail("End Site channels are not supported", joint=name)

        joint = BvhJoint(
            name=name,
            parent=parent,
            offset=offset,
            channels=channels,
            channel_start=channel_start,
            is_end_site=is_end,
            rotation_channels=tuple(rot_channels),
            position_channels=tuple(pos_channels),
        )
        joints.append(joint)
        names[name] = index
        while not closed:
            next_token = stream.peek()
            if next_token is None:
                _fail("unterminated hierarchy", joint=name)
            word = next_token[0].upper()
            if word in ("ROOT", "JOINT", "END"):
                if is_end:
                    _fail("End Site cannot contain child nodes", joint=name)
                parse_node(index)
                continue
            if word == "}":
                stream.take()
                closed = True
                break
            _fail("unexpected token after joint block", joint=name, token=next_token[0])
        return index

    root_count = 0
    while stream.peek() is not None:
        word = stream.peek()[0].upper()
        if word == "ROOT":
            root_count += 1
            if root_count > 1:
                _fail("BVH must contain exactly one ROOT", path=path)
            parse_node(None)
        else:
            _fail("unexpected token in hierarchy", token=stream.peek()[0], line=stream.peek()[1])
    if root_count != 1:
        _fail("BVH must contain exactly one ROOT", path=path)

    if declared_frames == 0:
        columns = total_channels
        data = np.empty((0, columns), dtype=np.float64)
    else:
        tokens: list[str] = []
        for row in motion_rows:
            tokens.extend(row.split())
        expected = declared_frames * total_channels
        if len(tokens) != expected:
            _fail(
                "motion data length does not match Frames * channel count",
                expected=expected,
                actual=len(tokens),
                declared_frames=declared_frames,
                channels=total_channels,
            )
        try:
            flat = np.array([float(token) for token in tokens], dtype=np.float64)
        except ValueError as exc:
            _fail("motion data contains a non-numeric value", error=str(exc))
        if not np.all(np.isfinite(flat)):
            _fail("motion data contains a non-finite value")
        data = flat.reshape(declared_frames, total_channels)

    # Parent-before-child is required by the FK loop.
    for index, joint in enumerate(joints):
        if joint.parent is not None and joint.parent >= index:
            _fail("joint hierarchy is not topologically ordered", joint=joint.name)

    from .math3d import hash_file

    source_hash = hash_text(text) if path.startswith("<") and path.endswith(">") else hash_file(path)
    return BvhDocument(
        path=path,
        joints=tuple(joints),
        frame_time=frame_time,
        frame_data=data,
        source_sha256=source_hash,
    )


def parse_bvh(path: str | Path) -> BvhDocument:
    target = Path(path)
    try:
        text = target.read_text(encoding="utf-8-sig")
    except UnicodeDecodeError:
        try:
            text = target.read_text(encoding="latin-1")
        except OSError as exc:
            raise BusinessError(E_IO, "unable to read BVH", {"path": str(target), "error": str(exc)}) from exc
    except OSError as exc:
        raise BusinessError(E_IO, "unable to read BVH", {"path": str(target), "error": str(exc)}) from exc
    return parse_bvh_text(text, str(target))


def enumerate_frame_hashes(document: BvhDocument) -> list[str]:
    return [document.frame_hash(index) for index in range(document.frame_count)]

