from __future__ import annotations

import unittest
from dataclasses import replace

from haptics.models import (
    ChannelCapability,
    DecisionType,
    DeviceCapabilities,
    EnvelopeTemplate,
    EventStream,
    HapticEvent,
)
from haptics.sample import sample_capabilities, sample_events
from haptics.scheduler import schedule_parallel, schedule_serial


def one_channel_capabilities(
    *,
    min_interval: float = 8.0,
    max_events: int = 3,
    window: tuple[float, float] = (0.0, 64.0),
    sample_ms: float = 32.0,
) -> DeviceCapabilities:
    template = EnvelopeTemplate("pulse", (1.0,), sample_ms)
    channel = ChannelCapability(
        channel=0,
        envelope_templates={"hit": template, "shoot": template},
        min_event_interval_ms=min_interval,
        max_events_per_packet=max_events,
        packet_window_ms=window,
    )
    return DeviceCapabilities(
        device_id="test",
        capability_version=1,
        frame_ms=16.0,
        channels=(channel,),
    )


def event(
    event_id: str,
    frame: int,
    priority: int,
    *,
    type: str = "hit",
    no_interrupt: bool = False,
    frame_index: int = 0,
) -> HapticEvent:
    return HapticEvent(
        event_id=event_id,
        frame=frame,
        frame_index=frame_index,
        event_type=type,
        channel=0,
        intensity=0.8,
        priority=priority,
        no_interrupt=no_interrupt,
    )


class SchedulerTests(unittest.TestCase):
    def test_every_event_has_a_decision_and_all_types_occur(self) -> None:
        scheduled = schedule_serial(sample_events(), sample_capabilities())
        self.assertEqual(len(scheduled.decisions), len(sample_events().events))
        types = {decision.decision_type for decision in scheduled.decisions}
        self.assertEqual(
            types,
            {
                DecisionType.ACCEPT,
                DecisionType.MERGE,
                DecisionType.INTERRUPT,
                DecisionType.DISCARD,
            },
        )

    def test_merge_within_minimum_event_interval(self) -> None:
        caps = one_channel_capabilities(min_interval=32.0)
        stream = EventStream(
            events=(event("A", 0, 2), event("B", 1, 3)),
            capability_version=1,
        )
        scheduled = schedule_serial(stream, caps)
        self.assertEqual(len(scheduled.packets), 1)
        self.assertEqual(scheduled.packets[0].event_count, 1)
        self.assertEqual(scheduled.packets[0].source_event_ids, ("A", "B"))
        self.assertEqual(scheduled.decisions[1].decision_type, DecisionType.MERGE)
        self.assertEqual(scheduled.decisions[1].target_packet_seq, 0)

    def test_no_interrupt_discards_lower_priority(self) -> None:
        caps = one_channel_capabilities(min_interval=4.0)
        stream = EventStream(
            events=(
                event("PROTECTED", 0, 8, no_interrupt=True),
                event("LOW", 1, 2),
            ),
            capability_version=1,
        )
        scheduled = schedule_serial(stream, caps)
        decision = scheduled.decisions[1]
        self.assertEqual(decision.decision_type, DecisionType.DISCARD)
        self.assertEqual(decision.reason, "NO_INTERRUPT_PROTECTED")
        self.assertEqual(decision.target_packet_seq, 0)
        self.assertEqual(scheduled.packets[0].source_event_ids, ("PROTECTED",))

    def test_higher_priority_interrupts_slot(self) -> None:
        caps = one_channel_capabilities(min_interval=4.0, sample_ms=32.0)
        stream = EventStream(
            events=(event("LOW", 0, 2), event("HIGH", 1, 9, type="shoot")),
            capability_version=1,
        )
        scheduled = schedule_serial(stream, caps)
        decision = scheduled.decisions[1]
        self.assertEqual(decision.decision_type, DecisionType.INTERRUPT)
        self.assertEqual(decision.basis_event_ids, ("LOW",))
        self.assertEqual(scheduled.packets[0].source_event_ids, ("HIGH",))

    def test_parallel_and_serial_are_identical(self) -> None:
        stream = sample_events()
        caps = sample_capabilities()
        serial = schedule_serial(stream, caps)
        parallel = schedule_parallel(stream, caps)
        self.assertEqual(serial.content_hash, parallel.content_hash)
        self.assertEqual(
            [packet.to_dict() for packet in serial.packets],
            [packet.to_dict() for packet in parallel.packets],
        )
        self.assertEqual(
            [decision.to_dict() for decision in serial.decisions],
            [decision.to_dict() for decision in parallel.decisions],
        )


if __name__ == "__main__":
    unittest.main()

