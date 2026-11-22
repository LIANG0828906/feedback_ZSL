from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field, replace
from enum import Enum
from typing import Any, Mapping

from .errors import ErrorCode, HapticError, Issue


EVENT_TYPES = ("hit", "shoot", "explosion", "footstep", "ui")


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def sha256_hex(data: bytes | str) -> str:
    if isinstance(data, str):
        data = data.encode("utf-8")
    return hashlib.sha256(data).hexdigest()


@dataclass(frozen=True)
class EnvelopeTemplate:
    name: str
    samples: tuple[float, ...]
    sample_ms: float

    def duration_ms(self) -> float:
        return len(self.samples) * self.sample_ms


@dataclass(frozen=True)
class ChannelCapability:
    channel: int
    envelope_templates: Mapping[str, EnvelopeTemplate]
    min_event_interval_ms: float
    max_events_per_packet: int
    packet_window_ms: tuple[float, float]

    @property
    def capacity_fingerprint(self) -> str:
        payload = {
            "channel": self.channel,
            "min_event_interval_ms": self.min_event_interval_ms,
            "max_events_per_packet": self.max_events_per_packet,
            "packet_window_ms": list(self.packet_window_ms),
            "templates": {
                name: {
                    "samples": list(template.samples),
                    "sample_ms": template.sample_ms,
                }
                for name, template in sorted(self.envelope_templates.items())
            },
        }
        return sha256_hex(canonical_json(payload))


@dataclass(frozen=True)
class DeviceCapabilities:
    device_id: str
    capability_version: int
    frame_ms: float
    channels: tuple[ChannelCapability, ...]
    schema_version: int = 1

    def channel(self, channel: int) -> ChannelCapability:
        for capability in self.channels:
            if capability.channel == channel:
                return capability
        raise HapticError(
            Issue(
                ErrorCode.E_CHAN,
                "channel is not declared by the device capability",
                channel=channel,
                field="channel",
            )
        )

    @property
    def channel_count(self) -> int:
        return len(self.channels)

    @property
    def manifest_fingerprint(self) -> str:
        return sha256_hex(canonical_json(self.to_dict()))

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "device_id": self.device_id,
            "capability_version": self.capability_version,
            "frame_ms": self.frame_ms,
            "channels": [
                {
                    "channel": channel.channel,
                    "min_event_interval_ms": channel.min_event_interval_ms,
                    "max_events_per_packet": channel.max_events_per_packet,
                    "packet_window_ms": list(channel.packet_window_ms),
                    "envelope_templates": {
                        name: {
                            "samples": list(template.samples),
                            "sample_ms": template.sample_ms,
                        }
                        for name, template in sorted(channel.envelope_templates.items())
                    },
                }
                for channel in self.channels
            ],
        }


@dataclass(frozen=True)
class HapticEvent:
    event_id: str
    frame: int
    frame_index: int
    event_type: str
    channel: int
    intensity: float
    priority: int
    no_interrupt: bool = False

    @property
    def time_ms(self) -> float:
        return float(self.frame)

    @property
    def order_key(self) -> tuple[int, int]:
        return (self.frame, self.frame_index)

    def to_dict(self) -> dict[str, Any]:
        return {
            "event_id": self.event_id,
            "frame": self.frame,
            "frame_index": self.frame_index,
            "type": self.event_type,
            "channel": self.channel,
            "intensity": self.intensity,
            "priority": self.priority,
            "no_interrupt": self.no_interrupt,
        }

    def with_priority(self, priority: int) -> "HapticEvent":
        return replace(self, priority=priority)


@dataclass(frozen=True)
class EventStream:
    events: tuple[HapticEvent, ...]
    capability_version: int
    schema_version: int = 1
    source: str = "memory"

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "capability_version": self.capability_version,
            "events": [event.to_dict() for event in self.events],
        }

    def content_hash(self) -> str:
        return sha256_hex(canonical_json(self.to_dict()))


class DecisionType(str, Enum):
    ACCEPT = "ACCEPT"
    MERGE = "MERGE"
    INTERRUPT = "INTERRUPT"
    DISCARD = "DISCARD"


@dataclass(frozen=True)
class ScheduleDecision:
    decision_id: int
    event_id: str
    event_order: tuple[int, int]
    channel: int
    decision_type: DecisionType
    basis_event_ids: tuple[str, ...]
    target_packet_seq: int
    reason: str
    intensity: float
    priority: int
    details: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "decision_id": self.decision_id,
            "event_id": self.event_id,
            "event_order": list(self.event_order),
            "channel": self.channel,
            "decision_type": self.decision_type.value,
            "basis_event_ids": list(self.basis_event_ids),
            "target_packet_seq": self.target_packet_seq,
            "reason": self.reason,
            "intensity": self.intensity,
            "priority": self.priority,
            "details": dict(self.details),
        }


@dataclass(frozen=True)
class ScheduledSlot:
    slot_index: int
    event_type: str
    first_event_id: str
    last_event_id: str
    source_event_ids: tuple[str, ...]
    time_ms: float
    last_event_time_ms: float
    priority: int
    no_interrupt: bool
    intensity: float
    envelope: tuple[float, ...]
    effective_until_ms: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "slot_index": self.slot_index,
            "event_type": self.event_type,
            "first_event_id": self.first_event_id,
            "last_event_id": self.last_event_id,
            "source_event_ids": list(self.source_event_ids),
            "time_ms": self.time_ms,
            "last_event_time_ms": self.last_event_time_ms,
            "priority": self.priority,
            "no_interrupt": self.no_interrupt,
            "intensity": self.intensity,
            "envelope": list(self.envelope),
            "effective_until_ms": self.effective_until_ms,
        }


@dataclass(frozen=True)
class ScheduledPacket:
    packet_seq: int
    local_index: int
    channel: int
    start_ms: float
    end_ms: float
    timing_mark_ms: float
    slots: tuple[ScheduledSlot, ...]
    source_event_ids: tuple[str, ...]
    decision_ids: tuple[int, ...] = ()
    reused: bool = False
    invalidated: bool = False

    @property
    def event_count(self) -> int:
        return len(self.slots)

    @property
    def cache_key(self) -> tuple[int, int]:
        return (self.channel, self.local_index)

    def with_reuse(self, reused: bool, invalidated: bool) -> "ScheduledPacket":
        return replace(self, reused=reused, invalidated=invalidated)

    def to_dict(self) -> dict[str, Any]:
        return {
            "packet_seq": self.packet_seq,
            "local_index": self.local_index,
            "channel": self.channel,
            "start_ms": self.start_ms,
            "end_ms": self.end_ms,
            "timing_mark_ms": self.timing_mark_ms,
            "event_count": self.event_count,
            "source_event_ids": list(self.source_event_ids),
            "decision_ids": list(self.decision_ids),
            "reused": self.reused,
            "invalidated": self.invalidated,
            "slots": [slot.to_dict() for slot in self.slots],
        }


@dataclass(frozen=True)
class ScheduledStream:
    packets: tuple[ScheduledPacket, ...]
    decisions: tuple[ScheduleDecision, ...]
    capability_version: int
    event_count: int
    content_hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "capability_version": self.capability_version,
            "event_count": self.event_count,
            "packets": [packet.to_dict() for packet in self.packets],
            "decisions": [decision.to_dict() for decision in self.decisions],
            "content_hash": self.content_hash,
        }


class PacketState(str, Enum):
    PENDING = "PENDING"
    ENCODED = "ENCODED"
    DISPATCHED = "DISPATCHED"
    CONFIRMED = "CONFIRMED"
    FAILED = "FAILED"


@dataclass(frozen=True)
class EncodedPacket:
    packet_seq: int
    channel: int
    data: bytes
    timing_mark_ms: float
    event_count: int
    local_index: int
    state: PacketState = PacketState.PENDING
    reused: bool = False
    invalidated: bool = False
    sent_at_ms: float | None = None

    @property
    def digest(self) -> str:
        return sha256_hex(self.data)

    @property
    def cache_key(self) -> tuple[int, int]:
        return (self.channel, self.local_index)

    def with_state(self, state: PacketState, sent_at_ms: float | None = None) -> "EncodedPacket":
        return replace(
            self,
            state=state,
            sent_at_ms=self.sent_at_ms if sent_at_ms is None else sent_at_ms,
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "packet_seq": self.packet_seq,
            "channel": self.channel,
            "local_index": self.local_index,
            "data_hex": self.data.hex(),
            "digest": self.digest,
            "timing_mark_ms": self.timing_mark_ms,
            "event_count": self.event_count,
            "state": self.state.value,
            "reused": self.reused,
            "invalidated": self.invalidated,
            "sent_at_ms": self.sent_at_ms,
        }


@dataclass(frozen=True)
class Echo:
    received_seq: int
    channel: int
    service_rx_index: int
    delivered_index: int
    processed_at_ms: float
    attempt: int
    batch_id: str
    wall_time_ns: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "received_seq": self.received_seq,
            "channel": self.channel,
            "service_rx_index": self.service_rx_index,
            "delivered_index": self.delivered_index,
            "processed_at_ms": self.processed_at_ms,
            "attempt": self.attempt,
            "batch_id": self.batch_id,
            "wall_time_ns": self.wall_time_ns,
        }


@dataclass(frozen=True)
class KernelVersions:
    kernel_version: str
    capability_version: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "kernel_version": self.kernel_version,
            "capability_version": self.capability_version,
        }

