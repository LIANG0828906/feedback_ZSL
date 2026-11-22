from __future__ import annotations

import unittest
from dataclasses import replace

from haptics.codec import decode_header, encode_packet, encode_stream
from haptics.errors import ErrorCode, HapticError
from haptics.models import PacketState
from haptics.sample import sample_capabilities, sample_events
from haptics.scheduler import schedule_serial
from haptics.state import PacketStateMachine


class CodecAndStateTests(unittest.TestCase):
    def setUp(self) -> None:
        self.capabilities = sample_capabilities()
        self.scheduled = schedule_serial(sample_events(), self.capabilities)

    def test_encoder_replays_identical_bytes(self) -> None:
        first = encode_stream(self.scheduled, self.capabilities)
        second = encode_stream(self.scheduled, self.capabilities)
        self.assertEqual([packet.data for packet in first], [packet.data for packet in second])
        for packet in first:
            header = decode_header(packet.data)
            self.assertEqual(header.magic, b"HTP1")
            self.assertTrue(header.crc_valid)
            self.assertEqual(header.packet_seq, packet.packet_seq)
            self.assertEqual(header.channel, packet.channel)

    def test_event_count_over_capability_is_e_pack(self) -> None:
        original = next(packet for packet in self.scheduled.packets if packet.event_count > 1)
        channel = self.capabilities.channels[0]
        constrained_channel = replace(channel, max_events_per_packet=1)
        caps = replace(
            self.capabilities,
            channels=(constrained_channel, self.capabilities.channels[1]),
        )
        with self.assertRaises(HapticError) as context:
            encode_packet(original, caps)
        issue = context.exception.issue
        self.assertEqual(issue.code, ErrorCode.E_PACK)
        self.assertEqual(issue.packet_seq, original.packet_seq)
        self.assertEqual(issue.field, "event_count")

    def test_timing_mark_outside_window_is_e_pack(self) -> None:
        packet = replace(self.scheduled.packets[0], timing_mark_ms=999.0)
        with self.assertRaises(HapticError) as context:
            encode_packet(packet, self.capabilities)
        issue = context.exception.issue
        self.assertEqual(issue.code, ErrorCode.E_PACK)
        self.assertEqual(issue.packet_seq, packet.packet_seq)
        self.assertEqual(issue.field, "timing_mark_ms")

    def test_illegal_state_transition_preserves_state(self) -> None:
        machine = PacketStateMachine()
        machine.register(0, 0)
        machine.transition(0, PacketState.ENCODED)
        machine.transition(0, PacketState.DISPATCHED)
        machine.transition(0, PacketState.CONFIRMED)
        with self.assertRaises(HapticError) as context:
            machine.transition(0, PacketState.ENCODED)
        self.assertEqual(context.exception.issue.code, ErrorCode.E_STATE)
        self.assertEqual(context.exception.issue.packet_seq, 0)
        self.assertEqual(context.exception.issue.field, "state")
        self.assertEqual(machine.state(0), PacketState.CONFIRMED)

    def test_failed_state_is_retryable(self) -> None:
        machine = PacketStateMachine()
        machine.register(1, 1)
        machine.transition(1, PacketState.ENCODED)
        machine.transition(1, PacketState.DISPATCHED)
        machine.transition(1, PacketState.FAILED)
        machine.transition(1, PacketState.CONFIRMED)
        self.assertEqual(machine.state(1), PacketState.CONFIRMED)

    def test_nonzero_window_lower_bound_is_encoded_inside_window(self) -> None:
        from dataclasses import replace as dc_replace

        from haptics.models import EventStream, HapticEvent
        from haptics.scheduler import schedule_serial

        channel = self.capabilities.channels[0]
        shifted = dc_replace(channel, packet_window_ms=(10.0, 30.0))
        caps = dc_replace(
            self.capabilities,
            channels=(shifted, self.capabilities.channels[1]),
        )
        stream = EventStream(
            events=(
                HapticEvent("A", 0, 0, "hit", 0, 0.5, 1),
                HapticEvent("B", 1, 0, "shoot", 0, 0.5, 2),
            ),
            capability_version=caps.capability_version,
        )
        encoded = encode_stream(schedule_serial(stream, caps), caps)
        header = decode_header(encoded[0].data)
        self.assertEqual(header.timing_mark_ms, 10)
        self.assertTrue(header.crc_valid)


if __name__ == "__main__":
    unittest.main()

