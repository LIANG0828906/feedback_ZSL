from __future__ import annotations

import tempfile
import threading
import unittest
from pathlib import Path

from a11y_rhythm.errors import E_BUSY, E_CANCELED, E_STALE_HANDLE, RhythmError
from a11y_rhythm.service import PublishService
from a11y_rhythm.store import DeliverySession

from tests.helpers import conflict_fixture, load_fixture


class ServiceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.fixture = conflict_fixture(self.root)
        self.bundle, self.frames, self.coverage, _ = load_fixture(self.fixture)
        self.session = DeliverySession.open(
            self.bundle,
            self.frames,
            self.coverage,
            self.root / "service.session.json",
        )
        self.service = PublishService(self.session)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_busy_old_new_and_stale_handle(self) -> None:
        entered = threading.Event()
        release = threading.Event()

        def block_progress(_checkpoint):
            entered.set()
            self.assertTrue(release.wait(5))

        old_handle = self.service.handle
        ticket = self.service.start_update(
            "ds001",
            {"text": "A longer description while the preview remains on the old version."},
            progress=block_progress,
        )
        self.assertTrue(entered.wait(5))
        with self.assertRaises(RhythmError) as busy:
            self.service.start_update("sp001", {"text": "Concurrent update."})
        self.assertEqual(busy.exception.code, E_BUSY)

        old_view = self.service.query(old_handle)
        self.assertEqual(old_view["payload"]["version"], 1)
        release.set()
        self.assertTrue(ticket.wait(10))
        ticket_done_error = ticket.error
        self.assertIsNone(ticket_done_error)
        new_view = self.service.query()
        self.assertEqual(new_view["payload"]["version"], 2)
        with self.assertRaises(RhythmError) as stale:
            self.service.query(old_handle)
        self.assertEqual(stale.exception.code, E_STALE_HANDLE)

    def test_cooperative_cancel_keeps_old_version_and_recovery_finishes(self) -> None:
        entered = threading.Event()
        release = threading.Event()

        def block_progress(_checkpoint):
            entered.set()
            release.set()
            # Hold the worker long enough for cancel() to win the race.
            threading.Event().wait(0.02)

        old_handle = self.service.handle
        ticket = self.service.start_update(
            "ds001",
            {"text": "A cancelled description keeps the old published snapshot intact."},
            progress=block_progress,
        )
        self.assertTrue(entered.wait(5))
        self.service.cancel(ticket)
        self.assertTrue(ticket.wait(10))
        self.assertIsNotNone(ticket.error)
        self.assertEqual(ticket.error.code, E_CANCELED)
        self.assertEqual(self.service.query(old_handle)["payload"]["version"], 1)
        status = self.service.recover()
        self.assertTrue(status.recovered)
        self.assertEqual(self.service.query()["payload"]["version"], 2)

    def test_material_level_busy_across_service_instances(self) -> None:
        entered = threading.Event()
        release = threading.Event()

        def block_progress(_checkpoint):
            entered.set()
            self.assertTrue(release.wait(5))

        second_session = DeliverySession.open(
            self.bundle,
            self.frames,
            self.coverage,
            self.root / "service.session.json",
        )
        second_service = PublishService(second_session)
        ticket = self.service.start_update(
            "ds001",
            {"text": "A longer description held for the cross-service busy check."},
            progress=block_progress,
        )
        self.assertTrue(entered.wait(5))
        try:
            with self.assertRaises(RhythmError) as busy:
                second_service.start_update("sp001", {"text": "Blocked by material registry."})
            self.assertEqual(busy.exception.code, E_BUSY)
        finally:
            release.set()
            self.assertTrue(ticket.wait(10))


if __name__ == "__main__":
    unittest.main()
