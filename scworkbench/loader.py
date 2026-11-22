from __future__ import annotations

import json
import math
import re
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from .errors import DecodeError, SchemaError
from .models import (
    CapabilitySet,
    DeviceSpec,
    FrameSpec,
    InputEvent,
    PhaseActionSpec,
    SampleSession,
)
from .profiles import CODEC_PROFILES, PROTOCOL_VERSION, SCHEMA_VERSION

_HEX_COLOR = re.compile(r"^#[0-9a-fA-F]{6}$")
_INPUT_TYPES = {"click", "drag", "key", "scroll"}
_PHASE_ACTIONS = {
    "pause",
    "resume",
    "reconnect",
    "reconnect_complete",
    "disconnect",
}


def _required(data: dict[str, Any], key: str, where: str) -> Any:
    if key not in data:
        raise SchemaError(f"{where} 缺少必填字段 {key!r}", field=key, path=where)
    return data[key]


def _as_int(value: Any, where: str, *, minimum: int | None = None) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise SchemaError(f"{where} 必须是整数", value=value, path=where)
    if minimum is not None and value < minimum:
        raise SchemaError(
            f"{where} 必须大于等于 {minimum}", value=value, path=where
        )
    return value


def _as_str(value: Any, where: str) -> str:
    if not isinstance(value, str) or not value:
        raise SchemaError(f"{where} 必须是非空字符串", value=value, path=where)
    return value


def _parse_time(value: Any, where: str) -> datetime:
    text = _as_str(value, where)
    try:
        parsed = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError as exc:
        raise SchemaError(f"{where} 不是合法 ISO-8601 时间", value=text) from exc
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def _validate_capabilities(data: Any, where: str) -> CapabilitySet:
    if not isinstance(data, dict):
        raise SchemaError(f"{where} 必须是对象", value=data)
    codecs_raw = _required(data, "codecs", where)
    if not isinstance(codecs_raw, list):
        raise SchemaError(f"{where}.codecs 必须是数组")
    codecs: list[str] = []
    for index, codec in enumerate(codecs_raw):
        codec_name = _as_str(codec, f"{where}.codecs[{index}]")
        if codec_name not in CODEC_PROFILES:
            raise SchemaError(
                f"{where}.codecs 包含未知编码档位 {codec_name!r}",
                value=codec_name,
                allowed=sorted(CODEC_PROFILES),
            )
        if codec_name in codecs:
            raise SchemaError(f"{where}.codecs 包含重复档位 {codec_name!r}")
        codecs.append(codec_name)
    max_width = _as_int(
        _required(data, "max_width", where), f"{where}.max_width", minimum=16
    )
    max_height = _as_int(
        _required(data, "max_height", where), f"{where}.max_height", minimum=16
    )
    max_fps = _as_int(
        _required(data, "max_fps", where), f"{where}.max_fps", minimum=1
    )
    supports_input = _required(data, "supports_input", where)
    if not isinstance(supports_input, bool):
        raise SchemaError(f"{where}.supports_input 必须是布尔值")
    if max_width > 16384 or max_height > 16384 or max_fps > 240:
        raise SchemaError(f"{where} 能力值超出协议上限", path=where)
    return CapabilitySet(
        codecs=tuple(codecs),
        max_width=max_width,
        max_height=max_height,
        max_fps=max_fps,
        supports_input=supports_input,
    )


def _validate_device(data: Any, index: int) -> DeviceSpec:
    where = f"devices[{index}]"
    if not isinstance(data, dict):
        raise SchemaError(f"{where} 必须是对象")
    device_id = _as_str(_required(data, "id", where), f"{where}.id")
    role = _as_str(_required(data, "role", where), f"{where}.role")
    if role not in {"sender", "receiver"}:
        raise SchemaError(
            f"{where}.role 必须是 sender 或 receiver", value=role
        )
    label = _as_str(_required(data, "label", where), f"{where}.label")
    capabilities = _validate_capabilities(
        _required(data, "capabilities", where), f"{where}.capabilities"
    )
    return DeviceSpec(
        id=device_id, label=label, role=role, capabilities=capabilities
    )


def _validate_color(value: Any, where: str) -> None:
    if not isinstance(value, str) or not _HEX_COLOR.match(value):
        raise DecodeError(
            f"{where} 颜色内容损坏", value=value, path=where
        )


def _validate_point(value: Any, where: str) -> None:
    if (
        not isinstance(value, list)
        or len(value) != 2
        or any(isinstance(v, bool) or not isinstance(v, int) for v in value)
    ):
        raise DecodeError(f"{where} 必须包含两个整数坐标", value=value)


def _validate_scene(scene: Any, frame_seq: int) -> None:
    where = f"frames[{frame_seq}].scene"
    if not isinstance(scene, dict):
        raise DecodeError(f"{where} 内容损坏", value=type(scene).__name__)
    background = scene.get("background")
    if not isinstance(background, list) or len(background) != 2:
        raise DecodeError(f"{where}.background 内容损坏")
    for index, color in enumerate(background):
        _validate_color(color, f"{where}.background[{index}]")
    windows = scene.get("windows")
    if not isinstance(windows, list) or not windows:
        raise DecodeError(f"{where}.windows 不能为空")
    seen_window_ids: set[int] = set()
    for index, window in enumerate(windows):
        if not isinstance(window, dict):
            raise DecodeError(f"{where}.windows[{index}] 内容损坏")
        try:
            window_id = _as_int(window["id"], f"{where}.windows[{index}].id")
            _as_int(window["x"], f"{where}.windows[{index}].x")
            _as_int(window["y"], f"{where}.windows[{index}].y")
            width = _as_int(window["w"], f"{where}.windows[{index}].w")
            height = _as_int(window["h"], f"{where}.windows[{index}].h")
            _as_str(window["title"], f"{where}.windows[{index}].title")
        except KeyError as exc:
            raise DecodeError(
                f"{where}.windows[{index}] 缺少字段 {exc.args[0]!r}"
            ) from exc
        except SchemaError as exc:
            raise DecodeError(exc.message, **exc.details) from exc
        if window_id in seen_window_ids:
            raise DecodeError(f"{where}.windows 的 id 重复", window_id=window_id)
        seen_window_ids.add(window_id)
        if width <= 0 or height <= 0:
            raise DecodeError(
                f"{where}.windows[{index}] 尺寸损坏",
                width=width,
                height=height,
            )
        _validate_color(window.get("color"), f"{where}.windows[{index}].color")
    _validate_point(scene.get("cursor"), f"{where}.cursor")
    particles = scene.get("particles", [])
    if not isinstance(particles, list):
        raise DecodeError(f"{where}.particles 内容损坏")
    for index, particle in enumerate(particles):
        if not isinstance(particle, dict):
            raise DecodeError(f"{where}.particles[{index}] 内容损坏")
        _validate_point(
            [particle.get("x"), particle.get("y")],
            f"{where}.particles[{index}]",
        )
        radius = particle.get("r")
        if isinstance(radius, bool) or not isinstance(radius, (int, float)):
            raise DecodeError(f"{where}.particles[{index}].r 内容损坏")
        if not math.isfinite(float(radius)) or radius <= 0:
            raise DecodeError(f"{where}.particles[{index}].r 内容损坏")
        _validate_color(
            particle.get("color"), f"{where}.particles[{index}].color"
        )


def _validate_frame(data: Any, index: int) -> FrameSpec:
    where = f"frames[{index}]"
    if not isinstance(data, dict):
        raise SchemaError(f"{where} 必须是对象")
    seq = _as_int(_required(data, "seq", where), f"{where}.seq", minimum=1)
    timestamp_ms = _as_int(
        _required(data, "timestamp_ms", where),
        f"{where}.timestamp_ms",
        minimum=0,
    )
    scene = _required(data, "scene", where)
    _validate_scene(scene, seq)
    return FrameSpec(seq=seq, timestamp_ms=timestamp_ms, scene=scene)


def _validate_input(data: Any, index: int) -> InputEvent:
    where = f"inputs[{index}]"
    if not isinstance(data, dict):
        raise SchemaError(f"{where} 必须是对象")
    seq = _as_int(_required(data, "seq", where), f"{where}.seq", minimum=1)
    timestamp_ms = _as_int(
        _required(data, "timestamp_ms", where),
        f"{where}.timestamp_ms",
        minimum=0,
    )
    kind = _as_str(_required(data, "type", where), f"{where}.type")
    if kind not in _INPUT_TYPES:
        raise DecodeError(f"{where}.type 损坏", value=kind)
    x = _as_int(_required(data, "x", where), f"{where}.x")
    y = _as_int(_required(data, "y", where), f"{where}.y")
    to_x = to_y = key = delta = None
    if kind == "drag":
        to_x = _as_int(_required(data, "to_x", where), f"{where}.to_x")
        to_y = _as_int(_required(data, "to_y", where), f"{where}.to_y")
    elif kind == "key":
        key = _as_str(_required(data, "key", where), f"{where}.key")
        if len(key) > 32:
            raise DecodeError(f"{where}.key 内容损坏", length=len(key))
    elif kind == "scroll":
        delta = _as_int(_required(data, "delta", where), f"{where}.delta")
        if abs(delta) > 100:
            raise DecodeError(f"{where}.delta 内容损坏", value=delta)
    return InputEvent(
        seq=seq,
        timestamp_ms=timestamp_ms,
        kind=kind,
        x=x,
        y=y,
        to_x=to_x,
        to_y=to_y,
        key=key,
        delta=delta,
    )


def parse_session(data: Any) -> SampleSession:
    if not isinstance(data, dict):
        raise SchemaError("会话样本顶层必须是对象", actual=type(data).__name__)
    schema_version = _as_int(
        _required(data, "schema_version", "$"), "$.schema_version", minimum=1
    )
    if schema_version != SCHEMA_VERSION:
        raise SchemaError(
            "不支持的样本 schema_version",
            expected=SCHEMA_VERSION,
            actual=schema_version,
        )
    protocol_version = _as_str(
        _required(data, "protocol_version", "$"), "$.protocol_version"
    )
    if protocol_version != PROTOCOL_VERSION:
        raise SchemaError(
            "不支持的协议版本",
            expected=PROTOCOL_VERSION,
            actual=protocol_version,
        )
    session_id = _as_str(
        _required(data, "session_id", "$"), "$.session_id"
    )
    session_name = _as_str(
        _required(data, "session_name", "$"), "$.session_name"
    )
    sender_device_id = _as_str(
        _required(data, "sender_device_id", "$"), "$.sender_device_id"
    )
    receiver_device_id = _as_str(
        _required(data, "receiver_device_id", "$"), "$.receiver_device_id"
    )
    devices_raw = _required(data, "devices", "$")
    if not isinstance(devices_raw, list) or not devices_raw:
        raise SchemaError("$.devices 必须是非空数组")
    devices = tuple(
        _validate_device(device, index)
        for index, device in enumerate(devices_raw)
    )
    by_id = {device.id: device for device in devices}
    if len(by_id) != len(devices):
        raise SchemaError("设备标识重复", device_ids=[d.id for d in devices])
    if sender_device_id not in by_id:
        raise SchemaError(
            "sender_device_id 引用了不存在的设备",
            device_id=sender_device_id,
        )
    if receiver_device_id not in by_id:
        raise SchemaError(
            "receiver_device_id 引用了不存在的设备",
            device_id=receiver_device_id,
        )
    if sender_device_id == receiver_device_id:
        raise SchemaError("发送端与接收端不能引用同一设备")
    if by_id[sender_device_id].role != "sender":
        raise SchemaError("sender_device_id 指向的设备角色不是 sender")
    if by_id[receiver_device_id].role != "receiver":
        raise SchemaError("receiver_device_id 指向的设备角色不是 receiver")

    declared_raw = _required(data, "declared", "$")
    if not isinstance(declared_raw, dict):
        raise SchemaError("$.declared 必须是对象")
    declared_width = _as_int(
        _required(declared_raw, "width", "$.declared"),
        "$.declared.width",
        minimum=16,
    )
    declared_height = _as_int(
        _required(declared_raw, "height", "$.declared"),
        "$.declared.height",
        minimum=16,
    )
    declared_fps = _as_int(
        _required(declared_raw, "fps", "$.declared"),
        "$.declared.fps",
        minimum=1,
    )
    if declared_width > 16384 or declared_height > 16384:
        raise SchemaError("声明分辨率超出协议上限")
    requires_input = _required(data, "requires_input_backchannel", "$")
    if not isinstance(requires_input, bool):
        raise SchemaError("$.requires_input_backchannel 必须是布尔值")

    establish_time = _as_str(
        _required(data, "establish_time", "$"), "$.establish_time"
    )
    disconnect_time = _as_str(
        _required(data, "disconnect_time", "$"), "$.disconnect_time"
    )
    established = _parse_time(establish_time, "$.establish_time")
    disconnected = _parse_time(disconnect_time, "$.disconnect_time")
    if disconnected <= established:
        raise SchemaError("disconnect_time 必须晚于 establish_time")

    frame_seed = _as_int(
        _required(data, "frame_seed", "$"), "$.frame_seed", minimum=0
    )
    frames_raw = _required(data, "frames", "$")
    if not isinstance(frames_raw, list) or not frames_raw:
        raise DecodeError("帧序列为空或内容损坏")
    frames = tuple(_validate_frame(item, i) for i, item in enumerate(frames_raw))
    previous_seq = 0
    previous_time = -1
    for frame in frames:
        if frame.seq <= previous_seq:
            raise DecodeError(
                "帧序号必须严格单调递增",
                previous=previous_seq,
                actual=frame.seq,
            )
        if frame.timestamp_ms < previous_time:
            raise DecodeError(
                "帧时间戳必须有序", actual=frame.timestamp_ms
            )
        previous_seq = frame.seq
        previous_time = frame.timestamp_ms

    inputs_raw = _required(data, "inputs", "$")
    if not isinstance(inputs_raw, list):
        raise DecodeError("输入事件流为空或内容损坏")
    inputs = tuple(_validate_input(item, i) for i, item in enumerate(inputs_raw))
    previous_seq = 0
    previous_time = -1
    for event in inputs:
        if event.seq <= previous_seq:
            raise DecodeError(
                "输入事件序号必须严格单调递增",
                previous=previous_seq,
                actual=event.seq,
            )
        if event.timestamp_ms < previous_time:
            raise DecodeError(
                "输入事件时间戳必须有序",
                previous=previous_time,
                actual=event.timestamp_ms,
            )
        previous_seq = event.seq
        previous_time = event.timestamp_ms

    actions_raw = _required(data, "phase_script", "$")
    if not isinstance(actions_raw, list):
        raise SchemaError("$.phase_script 必须是数组")
    actions: list[PhaseActionSpec] = []
    previous_time = -1
    for index, action in enumerate(actions_raw):
        where = f"phase_script[{index}]"
        if not isinstance(action, dict):
            raise SchemaError(f"{where} 必须是对象")
        timestamp_ms = _as_int(
            _required(action, "timestamp_ms", where),
            f"{where}.timestamp_ms",
            minimum=0,
        )
        action_name = _as_str(
            _required(action, "action", where), f"{where}.action"
        )
        if action_name not in _PHASE_ACTIONS:
            raise DecodeError(
                f"{where}.action 内容损坏", value=action_name
            )
        if timestamp_ms < previous_time:
            raise DecodeError("phase_script 时间戳必须有序")
        previous_time = timestamp_ms
        actions.append(
            PhaseActionSpec(timestamp_ms=timestamp_ms, action=action_name)
        )

    session = SampleSession(
        schema_version=schema_version,
        protocol_version=protocol_version,
        session_id=session_id,
        session_name=session_name,
        sender_device_id=sender_device_id,
        receiver_device_id=receiver_device_id,
        devices=devices,
        declared_width=declared_width,
        declared_height=declared_height,
        declared_fps=declared_fps,
        requires_input_backchannel=requires_input,
        establish_time=establish_time,
        disconnect_time=disconnect_time,
        frame_seed=frame_seed,
        frames=frames,
        inputs=inputs,
        phase_script=tuple(actions),
        raw=data,
    )
    session.content_hash = session.compute_content_hash()
    return session


def load_session(path: str | Path) -> SampleSession:
    source = Path(path)
    try:
        text = source.read_text(encoding="utf-8")
    except FileNotFoundError as exc:
        raise SchemaError("会话样本文件不存在", path=str(source)) from exc
    except OSError as exc:
        raise SchemaError(
            "会话样本文件不可读", path=str(source), error=str(exc)
        ) from exc
    try:
        data = json.loads(text)
    except json.JSONDecodeError as exc:
        raise DecodeError(
            "会话样本 JSON 损坏",
            line=exc.lineno,
            column=exc.colno,
            error=exc.msg,
        ) from exc
    return parse_session(data)
