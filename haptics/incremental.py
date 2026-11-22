from __future__ import annotations

from dataclasses import dataclass, replace
from typing import Iterable, Mapping

from .codec import encode_stream, encode_stream_parallel
from .dependencies import DependencyGraph, Invalidation, InvalidationReason
from .limits import ResourceLimits
from .models import (
    ChannelCapability,
    DeviceCapabilities,
    EncodedPacket,
    EventStream,
    HapticEvent,
    ScheduleDecision,
    ScheduledStream,
)
from .persistence import StoredCheckpoint
from .scheduler import schedule
from .validation import validate_capabilities, validate_events
from .errors import ValidationException


@dataclass(frozen=True)
class IncrementalResult:
    stream: EventStream
    capabilities: DeviceCapabilities
    scheduled: ScheduledStream
    packets: tuple[EncodedPacket, ...]
    decisions: tuple[ScheduleDecision, ...]
    dependencies: DependencyGraph
    invalidations: tuple[Invalidation, ...]
    reused_keys: tuple[tuple[int, int], ...]
    changed_event_id: str | None
    reason: InvalidationReason

    @property
    def reused_count(self) -> int:
        return len(self.reused_keys)

    def to_dict(self) -> dict:
        return {
            "changed_event_id": self.changed_event_id,
            "reason": self.reason.value,
            "reused_count": self.reused_count,
            "reused_keys": [list(item) for item in self.reused_keys],
            "invalidations": [item.to_dict() for item in self.invalidations],
            "packets": [item.to_dict() for item in self.packets],
            "decisions": [item.to_dict() for item in self.decisions],
        }


def persisted_packet_index(
    checkpoints: Iterable[StoredCheckpoint],
) -> dict[tuple[int, int], EncodedPacket]:
    return {
        (checkpoint.packet.channel, checkpoint.packet.local_index): checkpoint.packet
        for checkpoint in checkpoints
    }


def _changed_channel(
    old_graph: DependencyGraph,
    *,
    event_id: str | None,
    channel: int | None,
    event_time_ms: float | None,
    capability_changed: bool,
) -> tuple[int | None, int | None]:
    if capability_changed:
        if channel is None:
            return None, None
        candidates = [dep for dep in old_graph.dependencies if dep.channel == channel]
        if not candidates:
            return channel, None
        anchor = min(candidates, key=lambda dep: dep.local_index)
        return channel, anchor.local_index

    if event_id is not None and event_id in old_graph.event_to_packet:
        seq = old_graph.event_to_packet[event_id]
        anchor = next(dep for dep in old_graph.dependencies if dep.packet_seq == seq)
        return anchor.channel, anchor.local_index

    if channel is not None:
        candidates = [dep for dep in old_graph.dependencies if dep.channel == channel]
        if not candidates:
            return channel, 0
        if event_time_ms is None:
            anchor = min(candidates, key=lambda dep: dep.local_index)
        else:
            containing = [
                dep
                for dep in candidates
                if dep.start_ms - 1e-9 <= event_time_ms <= dep.end_ms + 1e-9
            ]
            if containing:
                anchor = min(containing, key=lambda dep: dep.local_index)
            else:
                later = [dep for dep in candidates if dep.start_ms >= event_time_ms]
                anchor = (
                    min(later, key=lambda dep: dep.local_index)
                    if later
                    else max(candidates, key=lambda dep: dep.local_index)
                )
        return anchor.channel, anchor.local_index
    return None, None


def incremental_rebuild(
    stream: EventStream,
    capabilities: DeviceCapabilities,
    *,
    old_graph: DependencyGraph,
    persisted: Mapping[tuple[int, int], EncodedPacket] | None = None,
    changed_event_id: str | None = None,
    changed_channel: int | None = None,
    changed_event_time_ms: float | None = None,
    reason: InvalidationReason = InvalidationReason.EVENT_PRIORITY_CHANGED,
    capability_changed: bool = False,
    parallel: bool = False,
    limits: ResourceLimits = ResourceLimits(),
) -> IncrementalResult:
    """Recompute a stream while reusing persisted bytes outside the cone.

    Only the anchor packet and subsequent packets on the same channel are
    invalidated.  A changed event can move between channels only through
    explicitly expressed add/remove operations; editing priority keeps its
    channel.
    """

    issues = list(validate_capabilities(capabilities).issues) + list(
        validate_events(stream, capabilities).issues
    )
    if issues:
        raise ValidationException(issues)
    scheduled = schedule(stream, capabilities, parallel=parallel, limits=limits)
    encoded = (
        encode_stream_parallel(scheduled, capabilities, limits)
        if parallel
        else encode_stream(scheduled, capabilities)
    )
    dependencies = DependencyGraph.build(scheduled, capabilities, encoded)
    affected_channel, anchor_local = _changed_channel(
        old_graph,
        event_id=changed_event_id,
        channel=changed_channel,
        event_time_ms=changed_event_time_ms,
        capability_changed=capability_changed,
    )

    persisted = persisted or {}
    invalidations: list[Invalidation] = []
    reused_keys: list[tuple[int, int]] = []
    final_packets: list[EncodedPacket] = []
    for packet in encoded:
        if affected_channel is None:
            is_invalid = True
        else:
            is_invalid = packet.channel == affected_channel and (
                anchor_local is None or packet.local_index >= anchor_local
            )
        if is_invalid:
            event_ids = next(
                (
                    dep.event_ids
                    for dep in dependencies.dependencies
                    if dep.packet_seq == packet.packet_seq
                ),
                (),
            )
            invalidations.append(
                Invalidation(
                    packet_seq=packet.packet_seq,
                    channel=packet.channel,
                    local_index=packet.local_index,
                    invalidated_event_ids=event_ids,
                    cause_event_id=changed_event_id
                    if not capability_changed
                    else f"<capability:{affected_channel}>",
                    reason=reason,
                )
            )
            final_packets.append(replace(packet, invalidated=True, reused=False))
            continue
        cached = persisted.get(packet.cache_key)
        if cached is not None:
            reused_keys.append(packet.cache_key)
            final_packets.append(
                replace(
                    cached,
                    packet_seq=packet.packet_seq,
                    local_index=packet.local_index,
                    reused=True,
                    invalidated=False,
                )
            )
        else:
            final_packets.append(replace(packet, invalidated=False, reused=False))

    final_encoded = tuple(final_packets)
    dependencies = DependencyGraph.build(scheduled, capabilities, final_encoded)
    return IncrementalResult(
        stream=stream,
        capabilities=capabilities,
        scheduled=scheduled,
        packets=final_encoded,
        decisions=scheduled.decisions,
        dependencies=dependencies,
        invalidations=tuple(invalidations),
        reused_keys=tuple(reused_keys),
        changed_event_id=changed_event_id,
        reason=reason,
    )


def edit_event_priority(
    stream: EventStream,
    capabilities: DeviceCapabilities,
    event_id: str,
    new_priority: int,
    *,
    old_graph: DependencyGraph,
    persisted: Mapping[tuple[int, int], EncodedPacket] | None = None,
    parallel: bool = False,
    limits: ResourceLimits = ResourceLimits(),
) -> IncrementalResult:
    found = False
    events: list[HapticEvent] = []
    for event in stream.events:
        if event.event_id == event_id:
            events.append(event.with_priority(new_priority))
            found = True
        else:
            events.append(event)
    if not found:
        raise KeyError(f"event {event_id} does not exist")
    updated = replace(stream, events=tuple(events))
    return incremental_rebuild(
        updated,
        capabilities,
        old_graph=old_graph,
        persisted=persisted,
        changed_event_id=event_id,
        reason=InvalidationReason.EVENT_PRIORITY_CHANGED,
        parallel=parallel,
        limits=limits,
    )


def add_event(
    stream: EventStream,
    capabilities: DeviceCapabilities,
    event: HapticEvent,
    *,
    old_graph: DependencyGraph,
    persisted: Mapping[tuple[int, int], EncodedPacket] | None = None,
    parallel: bool = False,
    limits: ResourceLimits = ResourceLimits(),
) -> IncrementalResult:
    events = tuple(sorted(stream.events + (event,), key=lambda item: item.order_key))
    updated = replace(stream, events=events)
    return incremental_rebuild(
        updated,
        capabilities,
        old_graph=old_graph,
        persisted=persisted,
        changed_event_id=event.event_id,
        changed_channel=event.channel,
        changed_event_time_ms=event.frame * capabilities.frame_ms,
        reason=InvalidationReason.EVENT_ADDED,
        parallel=parallel,
        limits=limits,
    )


def remove_event(
    stream: EventStream,
    capabilities: DeviceCapabilities,
    event_id: str,
    *,
    old_graph: DependencyGraph,
    persisted: Mapping[tuple[int, int], EncodedPacket] | None = None,
    parallel: bool = False,
    limits: ResourceLimits = ResourceLimits(),
) -> IncrementalResult:
    removed = next((item for item in stream.events if item.event_id == event_id), None)
    if removed is None:
        raise KeyError(f"event {event_id} does not exist")
    updated = replace(
        stream,
        events=tuple(item for item in stream.events if item.event_id != event_id),
    )
    return incremental_rebuild(
        updated,
        capabilities,
        old_graph=old_graph,
        persisted=persisted,
        changed_event_id=event_id,
        changed_channel=removed.channel,
        changed_event_time_ms=removed.frame * capabilities.frame_ms,
        reason=InvalidationReason.EVENT_REMOVED,
        parallel=parallel,
        limits=limits,
    )


def edit_channel_capability(
    stream: EventStream,
    capabilities: DeviceCapabilities,
    channel: int,
    *,
    min_event_interval_ms: float | None = None,
    max_events_per_packet: int | None = None,
    packet_window_ms: tuple[float, float] | None = None,
    old_graph: DependencyGraph,
    persisted: Mapping[tuple[int, int], EncodedPacket] | None = None,
    parallel: bool = False,
    limits: ResourceLimits = ResourceLimits(),
) -> IncrementalResult:
    updated_channels: list[ChannelCapability] = []
    found = False
    for cap in capabilities.channels:
        if cap.channel != channel:
            updated_channels.append(cap)
            continue
        found = True
        updated_channels.append(
            replace(
                cap,
                min_event_interval_ms=cap.min_event_interval_ms
                if min_event_interval_ms is None
                else min_event_interval_ms,
                max_events_per_packet=cap.max_events_per_packet
                if max_events_per_packet is None
                else max_events_per_packet,
                packet_window_ms=cap.packet_window_ms
                if packet_window_ms is None
                else packet_window_ms,
            )
        )
    if not found:
        raise KeyError(f"channel {channel} does not exist")
    updated_caps = replace(capabilities, channels=tuple(updated_channels))
    return incremental_rebuild(
        stream,
        updated_caps,
        old_graph=old_graph,
        persisted=persisted,
        changed_channel=channel,
        reason=InvalidationReason.CAPABILITY_CHANGED,
        capability_changed=True,
        parallel=parallel,
        limits=limits,
    )

