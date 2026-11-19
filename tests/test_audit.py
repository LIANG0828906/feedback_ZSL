import tempfile
import unittest
from pathlib import Path

from dualwrite.audit import AuditStore


class AuditStoreTests(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.audit = AuditStore(str(Path(self.tempdir.name) / "audit.sqlite3"))

    def tearDown(self):
        self.tempdir.cleanup()

    def test_cursor_does_not_skip_gap(self):
        self.assertEqual(self.audit.ack_change("east", "south", 3), 0)
        self.assertEqual(self.audit.ack_change("east", "south", 2), 0)
        self.assertEqual(self.audit.ack_change("east", "south", 1), 3)
        self.assertEqual(self.audit.cursor_position("east", "south"), 3)

    def test_duplicate_apply_is_audited_but_not_counted_twice(self):
        delivery = {
            "delivery_id": "d1",
            "source_site": "east",
            "target_site": "south",
            "source_change_id": 1,
            "duplicate_of": None,
        }
        event = {
            "change_key": "east:1",
            "origin_site": "east",
            "source_change_id": 1,
            "table_name": "members",
            "row_id": 1,
            "row_version": 2,
            "outcome": "applied",
            "stale_fields": [],
            "field_decisions": {},
            "old_row": {"points": 10},
            "new_row": {"points": 15},
            "points_delta": 5,
            "points_merged": True,
            "status_overridden": False,
        }
        self.audit.record_delivery(
            delivery_id="d1",
            change_key="east:1",
            source_site="east",
            target_site="south",
            source_change_id=1,
            table_name="members",
            row_id=1,
            row_version=2,
        )
        self.audit.record_apply_bundle(
            delivery=delivery,
            event=event,
            conflicts=[],
            field_updates=[],
            duplicate=False,
        )
        delivery2 = dict(delivery, delivery_id="d2", duplicate_of="d1")
        self.audit.record_delivery(
            delivery_id="d2",
            change_key="east:1",
            source_site="east",
            target_site="south",
            source_change_id=1,
            table_name="members",
            row_id=1,
            row_version=2,
            duplicate_of="d1",
        )
        self.audit.record_apply_bundle(
            delivery=delivery2,
            event=event,
            conflicts=[],
            field_updates=[],
            duplicate=True,
        )
        self.assertEqual(self.audit.counters()["applied"], 1)
        self.assertEqual(self.audit.counters()["point_merges"], 1)
        deliveries = self.audit.deliveries_for_row("members", 1)
        self.assertEqual(
            sorted(item["outcome"] for item in deliveries),
            ["applied", "duplicate"],
        )

    def test_completed_batch_is_immutable(self):
        self.audit.create_validation_batch("b1", "all", {"cursor": 1})
        summary = {"mismatch_count": 0}
        self.assertTrue(self.audit.complete_validation("b1", summary))
        self.assertFalse(
            self.audit.complete_validation("b1", {"mismatch_count": 99})
        )
        batch = self.audit.validation_batch("b1")
        self.assertEqual(batch["summary"], summary)


if __name__ == "__main__":
    unittest.main()

