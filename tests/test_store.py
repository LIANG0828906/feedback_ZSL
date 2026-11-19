from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from settlement_rollout.config import load_configuration
from settlement_rollout.errors import StateError
from settlement_rollout.orchestrator import RolloutOrchestrator
from settlement_rollout.store import AuditStore

from .helpers import QUICK_PLAN, QUICK_SLO


class StoreInvariantTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.directory = Path(self.temporary.name)
        self.config = load_configuration(QUICK_PLAN, QUICK_SLO)
        self.store = AuditStore(self.directory / "state.sqlite3")
        self.run_id = self.store.start_run(self.config)

    def tearDown(self) -> None:
        self.store.close()
        self.temporary.cleanup()

    def test_passed_batch_cannot_regress(self) -> None:
        batch_id = self.config.batches[0].batch_id
        self.store.transition_batch(
            self.run_id, batch_id, "PRECHECKING", "TEST", {}
        )
        self.store.transition_batch(
            self.run_id, batch_id, "SWITCHING", "TEST", {}
        )
        self.store.mark_observation_started(
            self.run_id, batch_id, 1.0, 2.0
        )
        self.store.record_decision(
            self.run_id,
            batch_id,
            "PASS",
            "slo_met",
            {"sample_count": 8},
            {},
            [],
            "PASSED",
        )
        with self.assertRaises(StateError) as caught:
            self.store.transition_batch(
                self.run_id, batch_id, "PENDING", "REGRESS", {}
            )
        self.assertEqual(caught.exception.code, "STATE_TERMINAL_BATCH")

    def test_terminal_run_is_idempotent_without_instance_calls(self) -> None:
        self.store.finish_run(self.run_id, "RELEASED", "test")
        controller = RolloutOrchestrator(
            self.config,
            self.store,
            {
                instance.instance_id: "http://127.0.0.1:1"
                for instance in self.config.instances
            },
        )
        summary = controller.run()
        self.assertEqual(summary["status"], "RELEASED")

    def test_manual_abort_event_and_snapshot_are_committed(self) -> None:
        controller = RolloutOrchestrator(self.config, self.store)
        controller.run_id = self.run_id
        controller.abort("operator requested stop")
        summary = controller.run()
        self.assertEqual(summary["status"], "ABORTED")
        self.assertIsNone(summary["current_batch"])
        batch_id = self.config.batches[0].batch_id
        audit = self.store.audit_batch(self.run_id, batch_id)
        self.assertEqual(audit["manual_aborts"][0]["reason"], "operator requested stop")
        self.assertEqual(
            audit["manual_aborts"][0]["metrics"],
            {},
        )
        manual_events = [
            event
            for event in audit["events"]
            if event["event_type"] == "MANUAL_ABORT"
        ]
        self.assertEqual(len(manual_events), 1)
        self.assertIn("metrics", manual_events[0]["payload"])


if __name__ == "__main__":
    unittest.main()
