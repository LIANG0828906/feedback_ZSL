from __future__ import annotations

from dataclasses import dataclass, replace
from enum import Enum
from typing import Any, Iterable, Mapping

from .models import (
    DeviceCapabilities,
    EncodedPacket,
    ScheduledStream,
    canonical_json,
    sha256_hex,
)


class InvalidationReason(str, Enum):
    EVENT_PRIORITY_CHANGED = "EVENT_PRIORITY_CHANGED"
    EVENT_ADDED = "EVENT_ADDED"
    EVENT_REMOVED = "EVENT_REMOVED"
    CAPABILITY_CHANGED = "CAPABILITY_CHANGED"
    PRIOR_PACKET_CHANGED = "PRIOR_PACKET_CHANGED"


@dataclass(frozen=True)
class PacketDependency:
    packet_seq: int
    channel: int
    local_index: int
    event_ids: tuple[str, ...]
    capability_fingerprint: str
    capability_params: Mapping[str, Any]
    start_ms: float
    end_ms: float
    previous_packet_seq: int | None
    previous_packet_hash: str | None
    previous_packet_state: str
    packet_hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "packet_seq": self.packet_seq,
            "channel": self.channel,
            "local_index": self.local_index,
            "event_ids": list(self.event_ids),
            "capability_fingerprint": self.capability_fingerprint,
            "capability_params": dict(self.capability_params),
            "start_ms": self.start_ms,
            "end_ms": self.end_ms,
            "previous_packet_seq": self.previous_packet_seq,
            "previous_packet_hash": self.previous_packet_hash,
            "previous_packet_state": self.previous_packet_state,
            "packet_hash": self.packet_hash,
        }


@dataclass(frozen=True)
class Invalidation:
    packet_seq: int
    channel: int
    local_index: int
    invalidated_event_ids: tuple[str, ...]
    cause_event_id: str | None
    reason: InvalidationReason

    def to_dict(self) -> dict[str, Any]:
        return {
            "packet_seq": self.packet_seq,
            "channel": self.channel,
            "local_index": self.local_index,
            "invalidated_event_ids": list(self.invalidated_event_ids),
            "cause_event_id": self.cause_event_id,
            "reason": self.reason.value,
        }


@dataclass(frozen=True)
class DependencyGraph:
    dependencies: tuple[PacketDependency, ...]
    event_to_packet: Mapping[str, int]

    @classmethod
    def build(
        cls,
        scheduled: ScheduledStream,
        capabilities: DeviceCapabilities,
        encoded: Iterable[EncodedPacket],
    ) -> "DependencyGraph":
        hashes = {packet.packet_seq: packet.digest for packet in encoded}
        dependencies: list[PacketDependency] = []
        event_to_packet: dict[str, int] = {}
        previous_by_channel: dict[int, PacketDependency] = {}
        for packet in sorted(scheduled.packets, key=lambda item: item.packet_seq):
            capability = capabilities.channel(packet.channel)
            previous = previous_by_channel.get(packet.channel)
            dependency = PacketDependency(
                packet_seq=packet.packet_seq,
                channel=packet.channel,
                local_index=packet.local_index,
                event_ids=packet.source_event_ids,
                capability_fingerprint=capability.capacity_fingerprint,
                capability_params={
                    "min_event_interval_ms": capability.min_event_interval_ms,
                    "max_events_per_packet": capability.max_events_per_packet,
                    "packet_window_ms": list(capability.packet_window_ms),
                },
                start_ms=packet.start_ms,
                end_ms=packet.end_ms,
                previous_packet_seq=None if previous is None else previous.packet_seq,
                previous_packet_hash=None if previous is None else previous.packet_hash,
                previous_packet_state="ABSENT" if previous is None else "PRESENT",
                packet_hash=hashes.get(packet.packet_seq, ""),
            )
            dependencies.append(dependency)
            previous_by_channel[packet.channel] = dependency
            for event_id in packet.source_event_ids:
                event_to_packet[event_id] = packet.packet_seq
        return cls(tuple(dependencies), event_to_packet)

    def first_affected_packet(self, event_id: str, channel: int, time_ms: float) -> int | None:
        if event_id in self.event_to_packet:
            return self.event_to_packet[event_id]
        candidates = [dependency for dependency in self.dependencies if dependency.channel == channel]
        if not candidates:
            return None
        containing = [
            dependency
            for dependency in candidates
            if dependency.start_ms - 1e-9 <= time_ms <= dependency.end_ms + 1e-9
        ]
        if containing:
            return min(containing, key=lambda dependency: dependency.local_index).packet_seq
        later = [dependency for dependency in candidates if dependency.start_ms >= time_ms]
        if later:
            return min(later, key=lambda dependency: dependency.local_index).packet_seq
        return max(candidates, key=lambda dependency: dependency.local_index).packet_seq

    def invalidate(
        self,
        *,
        reason: InvalidationReason,
        cause_event_id: str | None = None,
        event_id: str | None = None,
        channel: int | None = None,
        first_packet_seq: int | None = None,
        capability_fingerprints: Mapping[int, str] | None = None,
    ) -> tuple[Invalidation, ...]:
        starts: dict[int, int] = {}
        if capability_fingerprints:
            for dep in self.dependencies:
                if (
                    capability_fingerprints.get(dep.channel, dep.capability_fingerprint)
                    != dep.capability_fingerprint
                ):
                    starts[dep.channel] = 0
        if first_packet_seq is not None:
            anchor = next(
                (dep for dep in self.dependencies if dep.packet_seq == first_packet_seq),
                None,
            )
            if anchor is not None:
                starts[anchor.channel] = min(
                    starts.get(anchor.channel, anchor.local_index), anchor.local_index
                )
        elif event_id is not None and event_id in self.event_to_packet:
            anchor_seq = self.event_to_packet[event_id]
            anchor = next(
                dep for dep in self.dependencies if dep.packet_seq == anchor_seq
            )
            starts[anchor.channel] = min(
                starts.get(anchor.channel, anchor.local_index), anchor.local_index
            )
        elif channel is not None:
            anchor = next(
                (
                    dep
                    for dep in self.dependencies
                    if dep.channel == channel
                ),
                None,
            )
            if anchor is not None:
                starts[channel] = min(starts.get(channel, anchor.local_index), anchor.local_index)

        affected = [
            Invalidation(
                packet_seq=dep.packet_seq,
                channel=dep.channel,
                local_index=dep.local_index,
                invalidated_event_ids=dep.event_ids,
                cause_event_id=cause_event_id,
                reason=reason,
            )
            for dep in sorted(self.dependencies, key=lambda item: item.packet_seq)
            if dep.channel in starts and dep.local_index >= starts[dep.channel]
        ]
        return tuple(affected)

    def to_dict(self) -> dict[str, Any]:
        return {
            "dependencies": [dep.to_dict() for dep in self.dependencies],
            "event_to_packet": dict(sorted(self.event_to_packet.items())),
            "content_hash": sha256_hex(
                canonical_json([dep.to_dict() for dep in self.dependencies])
            ),
        }

