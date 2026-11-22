from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, replace
from typing import Iterable

from .errors import ErrorCode, HapticError, Issue
from .limits import ResourceLimits
from .models import (
    ChannelCapability,
    DecisionType,
    DeviceCapabilities,
    EventStream,
    HapticEvent,
    ScheduleDecision,
    ScheduledPacket,
    ScheduledSlot,
    ScheduledStream,
    canonical_json,
    sha256_hex,
)

_EPSILON = 1e-9


@dataclass
class _SlotBuilder:
    event_type: str
    first_event_id: str
    last_event_id: str
    source_event_ids: list[str]
    time_ms: float
    last_event_time_ms: float
    priority: int
    no_interrupt: bool
    intensity: float
    envelope: tuple[float, ...]
    effective_until_ms: float


@dataclass
class _PacketBuilder:
    local_index: int
    channel: int
    start_ms: float
    slots: list[_SlotBuilder]

    @property
    def key(self) -> tuple[int, int]:
        return (self.channel, self.local_index)


@dataclass
class _DecisionBuilder:
    event: HapticEvent
    decision_type: DecisionType
    basis_event_ids: tuple[str, ...]
    target_key: tuple[int, int] | None
    reason: str
    details: dict[str, object]


@dataclass
class _ChannelResult:
    channel: int
    packets: list[_PacketBuilder]
    decisions: list[_DecisionBuilder]


def _quantize(value: float) -> float:
    return round(float(value), 6)


def _scaled_envelope(channel: ChannelCapability, event: HapticEvent) -> tuple[float, ...]:
    template = channel.envelope_templates[event.event_type]
    return tuple(_quantize(sample * event.intensity) for sample in template.samples)


def _effect_duration(channel: ChannelCapability, event: HapticEvent) -> float:
    return channel.envelope_templates[event.event_type].duration_ms()


class _ChannelScheduler:
    def __init__(self, channel: ChannelCapability, frame_ms: float):
        self.channel = channel
        self.frame_ms = frame_ms
        self.packets: list[_PacketBuilder] = []
        self.decisions: list[_DecisionBuilder] = []
        self._current: _PacketBuilder | None = None
        self._last_packet: _PacketBuilder | None = None
        self._last_slot_packet: _PacketBuilder | None = None

    def _event_time(self, event: HapticEvent) -> float:
        return _quantize(event.frame * self.frame_ms)

    def feed(self, events: Iterable[HapticEvent]) -> None:
        for event in events:
            self._consume(event)

    def finish(self) -> _ChannelResult:
        self._close_current()
        return _ChannelResult(self.channel.channel, self.packets, self.decisions)

    def _new_packet(self, time_ms: float) -> _PacketBuilder:
        packet = _PacketBuilder(
            local_index=len(self.packets),
            channel=self.channel.channel,
            start_ms=time_ms,
            slots=[],
        )
        self.packets.append(packet)
        self._current = packet
        return packet

    def _close_current(self) -> None:
        if self._current is not None:
            self._last_packet = self._current
            self._current = None

    def _last_slot(self) -> _SlotBuilder | None:
        if self._current is not None and self._current.slots:
            return self._current.slots[-1]
        if self._last_packet is not None and self._last_packet.slots:
            return self._last_packet.slots[-1]
        return None

    def _last_slot_owner(self) -> _PacketBuilder | None:
        if self._current is not None and self._current.slots:
            return self._current
        if self._last_packet is not None and self._last_packet.slots:
            return self._last_packet
        return None

    def _add_decision(
        self,
        event: HapticEvent,
        decision_type: DecisionType,
        target: _PacketBuilder | None,
        reason: str,
        basis: tuple[str, ...] = (),
        details: dict[str, object] | None = None,
    ) -> None:
        self.decisions.append(
            _DecisionBuilder(
                event=event,
                decision_type=decision_type,
                basis_event_ids=basis,
                target_key=None if target is None else target.key,
                reason=reason,
                details=details or {},
            )
        )

    def _discard(
        self,
        event: HapticEvent,
        target: _PacketBuilder | None,
        reason: str,
        basis: tuple[str, ...],
        details: dict[str, object] | None = None,
    ) -> None:
        self._add_decision(event, DecisionType.DISCARD, target, reason, basis, details)
        self._remove_empty_current()

    def _remove_empty_current(self) -> None:
        if self._current is not None and not self._current.slots:
            empty = self._current
            self._current = None
            if self.packets and self.packets[-1] is empty:
                self.packets.pop()

    def _make_slot(self, event: HapticEvent, time_ms: float) -> _SlotBuilder:
        intensity = _quantize(event.intensity)
        return _SlotBuilder(
            event_type=event.event_type,
            first_event_id=event.event_id,
            last_event_id=event.event_id,
            source_event_ids=[event.event_id],
            time_ms=time_ms,
            last_event_time_ms=time_ms,
            priority=event.priority,
            no_interrupt=event.no_interrupt,
            intensity=intensity,
            envelope=_scaled_envelope(self.channel, event),
            effective_until_ms=_quantize(time_ms + _effect_duration(self.channel, event)),
        )

    def _merge_into(self, slot: _SlotBuilder, event: HapticEvent, time_ms: float) -> None:
        previous_priority = slot.priority
        incoming = _scaled_envelope(self.channel, event)
        merged = tuple(
            _quantize(max(left, right)) for left, right in zip(slot.envelope, incoming)
        )
        if len(incoming) > len(merged):
            merged = merged + incoming[len(merged) :]
        slot.envelope = merged
        slot.source_event_ids.append(event.event_id)
        slot.last_event_id = event.event_id
        slot.last_event_time_ms = time_ms
        slot.priority = max(slot.priority, event.priority)
        slot.no_interrupt = slot.no_interrupt or event.no_interrupt
        slot.intensity = _quantize(max(slot.intensity, event.intensity))
        slot.effective_until_ms = _quantize(
            max(
                slot.effective_until_ms,
                time_ms + _effect_duration(self.channel, event),
            )
        )
        if event.priority >= previous_priority:
            slot.event_type = event.event_type

    def _consume(self, event: HapticEvent) -> None:
        time_ms = self._event_time(event)
        if self._current is None:
            self._new_packet(time_ms)
        else:
            elapsed = time_ms - self._current.start_ms
            packet_span = (
                self.channel.packet_window_ms[1] - self.channel.packet_window_ms[0]
            )
            if (
                elapsed > packet_span + _EPSILON
                or len(self._current.slots) >= self.channel.max_events_per_packet
            ):
                self._close_current()
                self._new_packet(time_ms)

        current = self._current
        assert current is not None
        last_slot = self._last_slot()
        owner = self._last_slot_owner()

        if last_slot is None:
            self._apply_new_slot(event, time_ms, current)
            return

        in_active_window = time_ms < last_slot.effective_until_ms - _EPSILON
        within_merge_interval = (
            time_ms - last_slot.last_event_time_ms
            <= self.channel.min_event_interval_ms + _EPSILON
        )

        if in_active_window and last_slot.no_interrupt:
            self._discard(
                event,
                owner,
                "NO_INTERRUPT_PROTECTED",
                (last_slot.last_event_id,),
                {
                    "protected_event_id": last_slot.last_event_id,
                    "effective_until_ms": last_slot.effective_until_ms,
                },
            )
            return

        if within_merge_interval and owner is current:
            self._merge_into(last_slot, event, time_ms)
            self._add_decision(
                event,
                DecisionType.MERGE,
                current,
                "WITHIN_MIN_EVENT_INTERVAL",
                (last_slot.first_event_id,),
                {
                    "merged_into_event_id": last_slot.first_event_id,
                    "slot_source_count": len(last_slot.source_event_ids),
                },
            )
            return

        if in_active_window and event.priority <= last_slot.priority:
            self._discard(
                event,
                owner,
                "LOWER_OR_EQUAL_PRIORITY_ACTIVE",
                (last_slot.last_event_id,),
                {
                    "active_priority": last_slot.priority,
                    "incoming_priority": event.priority,
                },
            )
            return

        if in_active_window and event.priority > last_slot.priority:
            if owner is current:
                interrupted = current.slots.pop()
                self._add_decision(
                    event,
                    DecisionType.INTERRUPT,
                    current,
                    "HIGHER_PRIORITY_PREEMPTS_SLOT",
                    (interrupted.last_event_id,),
                    {
                        "interrupted_event_id": interrupted.last_event_id,
                        "interrupted_priority": interrupted.priority,
                        "incoming_priority": event.priority,
                    },
                )
                self._apply_new_slot(event, time_ms, current, record_accept=False)
                return
            self._add_decision(
                event,
                DecisionType.INTERRUPT,
                current,
                "CROSS_PACKET_PREEMPT",
                (last_slot.last_event_id,),
                {
                    "preempted_packet_seq_local": owner.local_index if owner else None,
                    "preempted_event_id": last_slot.last_event_id,
                },
            )
            self._apply_new_slot(event, time_ms, current, record_accept=False)
            return

        self._apply_new_slot(event, time_ms, current)

    def _apply_new_slot(
        self,
        event: HapticEvent,
        time_ms: float,
        packet: _PacketBuilder,
        *,
        record_accept: bool = True,
    ) -> None:
        if len(packet.slots) >= self.channel.max_events_per_packet:
            self._close_current()
            packet = self._new_packet(time_ms)
        packet.slots.append(self._make_slot(event, time_ms))
        if record_accept:
            self._add_decision(
                event,
                DecisionType.ACCEPT,
                packet,
                "NEW_SLOT",
            )


def _finalize(
    channel_results: Iterable[_ChannelResult],
    capabilities: DeviceCapabilities,
    event_count: int,
) -> ScheduledStream:
    results = list(channel_results)
    all_builders: list[_PacketBuilder] = []
    for result in results:
        all_builders.extend(result.packets)
    all_builders.sort(key=lambda packet: (_quantize(packet.start_ms), packet.channel, packet.local_index))

    seq_by_key: dict[tuple[int, int], int] = {}
    packets: list[ScheduledPacket] = []
    for seq, builder in enumerate(all_builders):
        seq_by_key[builder.key] = seq
        slots = tuple(
            ScheduledSlot(
                slot_index=index,
                event_type=slot.event_type,
                first_event_id=slot.first_event_id,
                last_event_id=slot.last_event_id,
                source_event_ids=tuple(slot.source_event_ids),
                time_ms=slot.time_ms,
                last_event_time_ms=slot.last_event_time_ms,
                priority=slot.priority,
                no_interrupt=slot.no_interrupt,
                intensity=slot.intensity,
                envelope=slot.envelope,
                effective_until_ms=slot.effective_until_ms,
            )
            for index, slot in enumerate(builder.slots)
        )
        source_ids: list[str] = []
        for slot in slots:
            source_ids.extend(slot.source_event_ids)
        start_ms = _quantize(builder.start_ms)
        packet_cap = capabilities.channel(builder.channel)
        packets.append(
            ScheduledPacket(
                packet_seq=seq,
                local_index=builder.local_index,
                channel=builder.channel,
                start_ms=start_ms,
                end_ms=max((slot.effective_until_ms for slot in slots), default=start_ms),
                timing_mark_ms=packet_cap.packet_window_ms[0],
                slots=slots,
                source_event_ids=tuple(source_ids),
            )
        )

    decision_builders: list[_DecisionBuilder] = []
    for result in results:
        decision_builders.extend(result.decisions)
    decision_builders.sort(
        key=lambda item: (
            item.event.frame,
            item.event.frame_index,
            item.event.channel,
            item.event.event_id,
        )
    )
    decisions: list[ScheduleDecision] = []
    decision_ids_by_packet: dict[int, list[int]] = {}
    for decision_id, builder in enumerate(decision_builders):
        target_seq = -1
        if builder.target_key is not None and builder.target_key in seq_by_key:
            target_seq = seq_by_key[builder.target_key]
            decision_ids_by_packet.setdefault(target_seq, []).append(decision_id)
        decisions.append(
            ScheduleDecision(
                decision_id=decision_id,
                event_id=builder.event.event_id,
                event_order=(builder.event.frame, builder.event.frame_index),
                channel=builder.event.channel,
                decision_type=builder.decision_type,
                basis_event_ids=builder.basis_event_ids,
                target_packet_seq=target_seq,
                reason=builder.reason,
                intensity=_quantize(builder.event.intensity),
                priority=builder.event.priority,
                details=builder.details,
            )
        )

    packets = [
        replace(packet, decision_ids=tuple(decision_ids_by_packet.get(packet.packet_seq, ())))
        for packet in packets
    ]
    payload = {
        "capability_version": capabilities.capability_version,
        "event_count": event_count,
        "packets": [packet.to_dict() for packet in packets],
        "decisions": [decision.to_dict() for decision in decisions],
    }
    return ScheduledStream(
        packets=tuple(packets),
        decisions=tuple(decisions),
        capability_version=capabilities.capability_version,
        event_count=event_count,
        content_hash=sha256_hex(canonical_json(payload)),
    )


def schedule_serial(
    stream: EventStream,
    capabilities: DeviceCapabilities,
) -> ScheduledStream:
    """Schedule all channels in a single, canonical pass."""

    by_channel: dict[int, list[HapticEvent]] = {cap.channel: [] for cap in capabilities.channels}
    for event in stream.events:
        if event.channel not in by_channel:
            raise HapticError(
                Issue(
                    ErrorCode.E_CHAN,
                    "channel is not declared by the device capability",
                    event_id=event.event_id,
                    channel=event.channel,
                    field="channel",
                )
            )
        by_channel[event.channel].append(event)

    results: list[_ChannelResult] = []
    for capability in capabilities.channels:
        scheduler = _ChannelScheduler(capability, capabilities.frame_ms)
        scheduler.feed(by_channel[capability.channel])
        results.append(scheduler.finish())
    return _finalize(results, capabilities, len(stream.events))


def _chunks(items: list[HapticEvent], size: int) -> Iterable[list[HapticEvent]]:
    for offset in range(0, len(items), size):
        yield items[offset : offset + size]


def schedule_parallel(
    stream: EventStream,
    capabilities: DeviceCapabilities,
    limits: ResourceLimits = ResourceLimits(),
) -> ScheduledStream:
    """Schedule independent channels in a bounded thread pool.

    Each worker owns at most `max_events_per_task` events at a time and feeds
    them into a channel-local state machine.  The final global packet numbering
    is assigned only after all channel results are joined, so completion order
    can never alter decisions or bytes.
    """

    limits.validate()
    by_channel: dict[int, list[HapticEvent]] = {cap.channel: [] for cap in capabilities.channels}
    for event in stream.events:
        if event.channel not in by_channel:
            raise HapticError(
                Issue(
                    ErrorCode.E_CHAN,
                    "channel is not declared by the device capability",
                    event_id=event.event_id,
                    channel=event.channel,
                    field="channel",
                )
            )
        by_channel[event.channel].append(event)

    def run_channel(capability: ChannelCapability) -> _ChannelResult:
        scheduler = _ChannelScheduler(capability, capabilities.frame_ms)
        for chunk in _chunks(by_channel[capability.channel], limits.max_events_per_task):
            scheduler.feed(chunk)
        return scheduler.finish()

    workers = min(limits.max_parallelism, max(1, len(capabilities.channels)))
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="haptic-schedule") as pool:
        results = list(pool.map(run_channel, capabilities.channels))
    return _finalize(results, capabilities, len(stream.events))


def schedule(
    stream: EventStream,
    capabilities: DeviceCapabilities,
    *,
    parallel: bool = False,
    limits: ResourceLimits = ResourceLimits(),
) -> ScheduledStream:
    if parallel:
        return schedule_parallel(stream, capabilities, limits)
    return schedule_serial(stream, capabilities)

