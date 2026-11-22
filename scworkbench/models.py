from __future__ import annotations

import hashlib
import json
from collections.abc import Iterable
from dataclasses import asdict, dataclass, field
from enum import Enum
from typing import Any

from .profiles import PROTOCOL_VERSION


class Phase(str, Enum):
    NEGOTIATING = "negotiating"
    MIRRORING = "mirroring"
    PAUSED = "paused"
    RECONNECTING = "reconnecting"
    DISCONNECTED = "disconnected"

    @property
    def label(self) -> str:
        return {
            Phase.NEGOTIATING: "协商中",
            Phase.MIRRORING: "镜像中",
            Phase.PAUSED: "暂停",
            Phase.RECONNECTING: "重连中",
            Phase.DISCONNECTED: "已断开",
        }[self]


LEGAL_TRANSITIONS: dict[Phase, frozenset[Phase]] = {
    Phase.NEGOTIATING: frozenset({Phase.MIRRORING, Phase.DISCONNECTED}),
    Phase.MIRRORING: frozenset(
        {Phase.PAUSED, Phase.RECONNECTING, Phase.DISCONNECTED}
    ),
    Phase.PAUSED: frozenset(
        {Phase.MIRRORING, Phase.RECONNECTING, Phase.DISCONNECTED}
    ),
    Phase.RECONNECTING: frozenset({Phase.MIRRORING, Phase.DISCONNECTED}),
    Phase.DISCONNECTED: frozenset(),
}

PHASE_SEQUENCE = (
    Phase.NEGOTIATING,
    Phase.MIRRORING,
    Phase.PAUSED,
    Phase.RECONNECTING,
    Phase.DISCONNECTED,
)


@dataclass(frozen=True)
class CapabilitySet:
    codecs: tuple[str, ...]
    max_width: int
    max_height: int
    max_fps: int
    supports_input: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "codecs": list(self.codecs),
            "max_width": self.max_width,
            "max_height": self.max_height,
            "max_fps": self.max_fps,
            "supports_input": self.supports_input,
        }

    def canonical(self) -> dict[str, Any]:
        return {
            "codecs": sorted(self.codecs),
            "max_width": self.max_width,
            "max_height": self.max_height,
            "max_fps": self.max_fps,
            "supports_input": self.supports_input,
        }


@dataclass(frozen=True)
class DeviceSpec:
    id: str
    label: str
    role: str
    capabilities: CapabilitySet

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "label": self.label,
            "role": self.role,
            "capabilities": self.capabilities.to_dict(),
        }


@dataclass(frozen=True)
class Resolution:
    width: int
    height: int

    def to_dict(self) -> dict[str, int]:
        return {"width": self.width, "height": self.height}


@dataclass
class FrameSpec:
    seq: int
    timestamp_ms: int
    scene: dict[str, Any]

    def to_dict(self) -> dict[str, Any]:
        return {
            "seq": self.seq,
            "timestamp_ms": self.timestamp_ms,
            "scene": self.scene,
        }


@dataclass(frozen=True)
class InputEvent:
    seq: int
    timestamp_ms: int
    kind: str
    x: int
    y: int
    to_x: int | None = None
    to_y: int | None = None
    key: str | None = None
    delta: int | None = None

    def to_dict(self) -> dict[str, Any]:
        data: dict[str, Any] = {
            "seq": self.seq,
            "timestamp_ms": self.timestamp_ms,
            "type": self.kind,
            "x": self.x,
            "y": self.y,
        }
        if self.to_x is not None:
            data["to_x"] = self.to_x
        if self.to_y is not None:
            data["to_y"] = self.to_y
        if self.key is not None:
            data["key"] = self.key
        if self.delta is not None:
            data["delta"] = self.delta
        return data


@dataclass(frozen=True)
class PhaseActionSpec:
    timestamp_ms: int
    action: str

    def to_dict(self) -> dict[str, Any]:
        return {"timestamp_ms": self.timestamp_ms, "action": self.action}


@dataclass
class SampleSession:
    schema_version: int
    protocol_version: str
    session_id: str
    session_name: str
    sender_device_id: str
    receiver_device_id: str
    devices: tuple[DeviceSpec, ...]
    declared_width: int
    declared_height: int
    declared_fps: int
    requires_input_backchannel: bool
    establish_time: str
    disconnect_time: str
    frame_seed: int
    frames: tuple[FrameSpec, ...]
    inputs: tuple[InputEvent, ...]
    phase_script: tuple[PhaseActionSpec, ...]
    raw: dict[str, Any]
    content_hash: str = ""

    @property
    def sender(self) -> DeviceSpec:
        return next(d for d in self.devices if d.id == self.sender_device_id)

    @property
    def receiver(self) -> DeviceSpec:
        return next(d for d in self.devices if d.id == self.receiver_device_id)

    def canonical_content(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "protocol_version": self.protocol_version,
            "session_id": self.session_id,
            "session_name": self.session_name,
            "sender_device_id": self.sender_device_id,
            "receiver_device_id": self.receiver_device_id,
            "devices": [device.to_dict() for device in self.devices],
            "declared": {
                "width": self.declared_width,
                "height": self.declared_height,
                "fps": self.declared_fps,
            },
            "requires_input_backchannel": self.requires_input_backchannel,
            "establish_time": self.establish_time,
            "disconnect_time": self.disconnect_time,
            "frame_seed": self.frame_seed,
            "frames": [frame.to_dict() for frame in self.frames],
            "inputs": [event.to_dict() for event in self.inputs],
            "phase_script": [action.to_dict() for action in self.phase_script],
        }

    def compute_content_hash(self) -> str:
        payload = json.dumps(
            self.canonical_content(),
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        return hashlib.sha256(payload).hexdigest()


@dataclass(frozen=True)
class NegotiationResult:
    codec: str
    width: int
    height: int
    fps: int
    supports_input: bool
    sender_device_id: str
    receiver_device_id: str
    capabilities_hash: str

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "NegotiationResult":
        return cls(
            codec=str(data["codec"]),
            width=int(data["width"]),
            height=int(data["height"]),
            fps=int(data["fps"]),
            supports_input=bool(data["supports_input"]),
            sender_device_id=str(data["sender_device_id"]),
            receiver_device_id=str(data["receiver_device_id"]),
            capabilities_hash=str(data["capabilities_hash"]),
        )


@dataclass
class PhaseEvent:
    index: int
    timestamp_ms: int
    action: str
    from_phase: str
    to_phase: str
    ok: bool
    code: str | None = None
    message: str = ""
    frames_committed: int = 0
    inputs_replayed: int = 0

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "PhaseEvent":
        return cls(**data)


@dataclass
class FrameResult:
    frame_seq: int
    timestamp_ms: int
    phase: str
    status: str
    committed: bool
    codec: str
    width: int
    height: int
    frame_sha256: str
    wire_sha256: str
    packets_sent: int
    packets_received: int
    fragments_total: int
    fragments_received: int
    retransmissions: int
    missing_fragments: list[int] = field(default_factory=list)
    codes: list[str] = field(default_factory=list)
    message: str = ""
    fragment_order: list[int] = field(default_factory=list)
    fragment_status: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "FrameResult":
        return cls(**data)


@dataclass
class InputResult:
    input_seq: int
    timestamp_ms: int
    phase: str
    event_type: str
    status: str
    code: str | None
    codes: list[str]
    source_coords: list[int]
    mapped_coords: list[int]
    clipped: bool
    clip_trace: list[dict[str, Any]]
    desktop_digest: str
    desktop_state: dict[str, Any]
    message: str = ""

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "InputResult":
        return cls(**data)


@dataclass
class SessionStats:
    frames_sent: int = 0
    frames_reassembled: int = 0
    frames_committed: int = 0
    frames_missing: int = 0
    frames_rejected: int = 0
    fragments_sent: int = 0
    fragments_received: int = 0
    fragments_lost: int = 0
    packets_sent: int = 0
    packets_received: int = 0
    packets_corrupt: int = 0
    packets_session_rejected: int = 0
    retransmission_requests: int = 0
    retransmission_recovered: int = 0
    order_events: int = 0
    inputs_total: int = 0
    inputs_replayed: int = 0
    inputs_duplicate: int = 0
    inputs_rejected: int = 0
    phase_rejections: int = 0
    capability_changes_rejected: int = 0
    frames_failed: int = 0
    frame_retries: int = 0

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "SessionStats":
        return cls(**data)


@dataclass
class RunResult:
    mode: str
    session_id: str
    content_hash: str
    capabilities_hash: str
    negotiation: NegotiationResult
    final_phase: str
    frames: list[FrameResult]
    inputs: list[InputResult]
    phase_events: list[PhaseEvent]
    stats: SessionStats
    desktop_state: dict[str, Any]
    events: list[dict[str, Any]]
    checkpoint_count: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "mode": self.mode,
            "session_id": self.session_id,
            "content_hash": self.content_hash,
            "capabilities_hash": self.capabilities_hash,
            "negotiation": self.negotiation.to_dict(),
            "final_phase": self.final_phase,
            "frames": [item.to_dict() for item in self.frames],
            "inputs": [item.to_dict() for item in self.inputs],
            "phase_events": [item.to_dict() for item in self.phase_events],
            "stats": self.stats.to_dict(),
            "desktop_state": self.desktop_state,
            "events": self.events,
            "checkpoint_count": self.checkpoint_count,
        }


@dataclass(frozen=True)
class TimelineItem:
    index: int
    timestamp_ms: int
    kind: str
    payload_index: int


def stable_digest(payload: Any) -> str:
    encoded = json.dumps(
        payload, ensure_ascii=False, sort_keys=True, separators=(",", ":")
    ).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def capabilities_digest(
    sender: CapabilitySet,
    receiver: CapabilitySet,
    protocol_version: str = PROTOCOL_VERSION,
) -> str:
    payload = {
        "protocol_version": protocol_version,
        "sender": sender.canonical(),
        "receiver": receiver.canonical(),
    }
    return stable_digest(payload)


def phase_event_summary(events: Iterable[PhaseEvent]) -> list[list[str]]:
    return [[event.from_phase, event.to_phase] for event in events]
