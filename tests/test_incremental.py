from __future__ import annotations

import unittest

from haptics.dependencies import DependencyGraph, InvalidationReason
from haptics.incremental import (
    add_event,
    edit_channel_capability,
    edit_event_priority,
    persisted_packet_index,
    remove_event,
)
from haptics.models import HapticEvent
from haptics.sample import sample_capabilities, sample_events
from haptics.scheduler import schedule_serial
from haptics.codec import encode_stream


class IncrementalTests(unittest.TestCase):
    def setUp(self) -> None:
        self.stream = sample_events()
        self.capabilities = sample_capabilities()
        self.scheduled = schedule_serial(self.stream, self.capabilities)
        self.encoded = encode_stream(self.scheduled, self.capabilities)
        self.graph = DependencyGraph.build(self.scheduled, self.capabilities, self.encoded)
        self.persisted = persisted_packet_index([])
        self.persisted = {
            (packet.channel, packet.local_index): packet for packet in self.encoded
        }

    def test_priority_edit_invalidates_only_channel_cone(self) -> None:
        updated = edit_event_priority(
            self.stream,
            self.capabilities,
            "E0011_00",
            9,
            old_graph=self.graph,
            persisted=self.persisted,
        )
        self.assertEqual(updated.reason, InvalidationReason.EVENT_PRIORITY_CHANGED)
        self.assertTrue(updated.invalidations)
        self.assertTrue(all(item.channel == 0 for item in updated.invalidations))
        self.assertTrue(
            all(item.local_index >= 2 for item in updated.invalidations)
        )
        self.assertGreater(updated.reused_count, 0)
        reused_keys = set(updated.reused_keys)
        self.assertTrue(any(channel == 1 for channel, _ in reused_keys))
        self.assertTrue(any(channel == 0 and index < 2 for channel, index in reused_keys))
        self.assertTrue(
            all(
                packet.reused
                for packet in updated.packets
                if packet.cache_key in updated.reused_keys
            )
        )
        for packet in updated.packets:
            if packet.cache_key in updated.reused_keys:
                self.assertEqual(packet.data, self.persisted[packet.cache_key].data)

    def test_capability_edit_invalidates_only_target_channel(self) -> None:
        updated = edit_channel_capability(
            self.stream,
            self.capabilities,
            channel=1,
            min_event_interval_ms=8.0,
            old_graph=self.graph,
            persisted=self.persisted,
        )
        self.assertEqual(updated.reason, InvalidationReason.CAPABILITY_CHANGED)
        self.assertTrue(updated.invalidations)
        self.assertTrue(all(item.channel == 1 for item in updated.invalidations))
        reused_keys = set(updated.reused_keys)
        self.assertTrue(any(channel == 0 for channel, _ in reused_keys))
        self.assertFalse(any(channel == 1 for channel, _ in reused_keys))

    def test_dependency_graph_records_previous_packet_and_capability(self) -> None:
        dependencies = self.graph.dependencies
        self.assertIsNone(dependencies[0].previous_packet_seq)
        for dependency in dependencies[1:]:
            if dependency.channel == dependencies[0].channel:
                self.assertIsNotNone(dependency.previous_packet_seq)
                self.assertIsNotNone(dependency.previous_packet_hash)
                break
        self.assertTrue(all(dep.capability_fingerprint for dep in dependencies))
        self.assertTrue(all(dep.capability_params for dep in dependencies))
        self.assertEqual(dependencies[0].previous_packet_state, "ABSENT")
        self.assertTrue(
            any(dep.previous_packet_state == "PRESENT" for dep in dependencies)
        )

    def test_graph_invalidate_starts_at_changed_event_cone(self) -> None:
        invalidations = self.graph.invalidate(
            reason=InvalidationReason.EVENT_PRIORITY_CHANGED,
            cause_event_id="E0011_00",
            event_id="E0011_00",
        )
        self.assertTrue(invalidations)
        self.assertTrue(all(item.channel == 0 for item in invalidations))
        self.assertEqual(invalidations[0].local_index, 2)
        self.assertEqual(invalidations[0].invalidated_event_ids[0], "E0011_00")

    def test_add_and_remove_event_use_same_channel_cone(self) -> None:
        added = add_event(
            self.stream,
            self.capabilities,
            HapticEvent(
                event_id="X-NEW",
                frame=23,
                frame_index=1,
                event_type="ui",
                channel=1,
                intensity=0.2,
                priority=1,
            ),
            old_graph=self.graph,
            persisted=self.persisted,
        )
        self.assertEqual(added.reason, InvalidationReason.EVENT_ADDED)
        self.assertTrue(all(item.channel == 1 for item in added.invalidations))
        self.assertEqual(added.changed_event_id, "X-NEW")

        removed = remove_event(
            self.stream,
            self.capabilities,
            "E0000_00",
            old_graph=self.graph,
            persisted=self.persisted,
        )
        self.assertEqual(removed.reason, InvalidationReason.EVENT_REMOVED)
        self.assertTrue(all(item.channel == 0 for item in removed.invalidations))
        self.assertEqual(removed.changed_event_id, "E0000_00")


if __name__ == "__main__":
    unittest.main()

