from __future__ import annotations

import threading
import time
import unittest
from unittest.mock import patch

from tactical.changes import SetObstacle
from tactical.errors import E_BATCH_FAILED, E_BUSY, ServiceError
from tactical.service import TacticalService

from .helpers import open_grid


class BatchAndTaskTests(unittest.TestCase):
    def test_mid_batch_failure_rolls_back(self) -> None:
        service = TacticalService(
            open_grid(24, 24),
            {0: (20, 22), 1: (21, 22), 2: (22, 22)},
        )
        try:
            old_version = service.version
            old_hash = service.version_hash
            with (
                patch(
                    "tactical.service.incremental_update",
                    side_effect=RuntimeError("injected"),
                ),
                self.assertRaises(ServiceError) as raised,
            ):
                service.commit_batch(
                    [SetObstacle(10, 10, True), SetObstacle(11, 11, True)]
                )
            self.assertEqual(raised.exception.code, E_BATCH_FAILED)
            self.assertEqual(service.version, old_version)
            self.assertEqual(service.version_hash, old_hash)
            self.assertEqual(
                service.get_path(
                    0,
                    (10, 0),
                    expected_version=old_version,
                    expected_hash=old_hash,
                ).version,
                old_version,
            )
        finally:
            service.close()

    def test_duplicate_path_task_returns_busy_then_releases(self) -> None:
        service = TacticalService(
            open_grid(24, 24),
            {0: (20, 22), 1: (21, 22), 2: (22, 22)},
        )
        release = threading.Event()
        entered = threading.Event()
        original = service._run_path_task

        def blocked_run(*args: object, **kwargs: object):
            entered.set()
            release.wait(timeout=5)
            return original(*args, **kwargs)

        try:
            with patch.object(service, "_run_path_task", side_effect=blocked_run):
                task = service.submit_path_task(0, (10, 0))
                self.assertTrue(entered.wait(timeout=5))
                with self.assertRaises(ServiceError) as raised:
                    service.submit_path_task(0, (12, 0))
                self.assertEqual(raised.exception.code, E_BUSY)
                release.set()
                result = task.future.result(timeout=10)
                self.assertEqual(result.unit_id, 0)
                deadline = time.time() + 5
                while service.active_task_keys() and time.time() < deadline:
                    time.sleep(0.01)
                self.assertEqual(service.active_task_keys(), ())
                second = service.submit_path_task(0, (12, 0))
                self.assertEqual(second.future.result(timeout=10).unit_id, 0)
        finally:
            release.set()
            service.close()


if __name__ == "__main__":
    unittest.main()
