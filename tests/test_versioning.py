from __future__ import annotations

import threading
import unittest
from unittest.mock import patch

from tactical.changes import SetObstacle
from tactical.errors import E_VERSION_MISMATCH, ServiceError
from tactical.service import TacticalService

from .helpers import open_grid


class VersioningTests(unittest.TestCase):
    def setUp(self) -> None:
        self.service = TacticalService(
            open_grid(20, 20),
            {0: (10, 19), 1: (11, 19), 2: (12, 19)},
            cache_queries=[(0, (10, 0)), (1, (11, 0)), (2, (12, 0))],
        )

    def tearDown(self) -> None:
        self.service.close()

    def test_prepared_candidate_does_not_change_published_view(self) -> None:
        old = self.service.snapshot
        prepared = self.service.prepare_batch([SetObstacle(10, 10, True)])
        during = self.service.get_path(
            0,
            (10, 0),
            expected_version=old.version,
            expected_hash=old.version_hash,
        )
        self.assertEqual(during.version, old.version)
        self.assertEqual(during.version_hash, old.version_hash)

        stats = self.service.publish_prepared(
            prepared,
            expected_base_version=old.version,
            expected_base_hash=old.version_hash,
        )
        self.assertEqual(stats.version, old.version + 1)
        current = self.service.snapshot
        self.assertEqual(current.version, old.version + 1)
        self.assertNotEqual(current.version_hash, old.version_hash)
        with self.assertRaises(ServiceError) as raised:
            self.service.get_path(
                0,
                (10, 0),
                expected_version=old.version,
                expected_hash=old.version_hash,
            )
        self.assertEqual(raised.exception.code, E_VERSION_MISMATCH)

    def test_stale_prepared_publish_is_rejected(self) -> None:
        prepared = self.service.prepare_batch([SetObstacle(5, 5, True)])
        self.service.commit_batch([SetObstacle(6, 6, True)])
        with self.assertRaises(ServiceError) as raised:
            self.service.publish_prepared(prepared)
        self.assertEqual(raised.exception.code, E_VERSION_MISMATCH)

    def test_concurrent_queries_observe_only_complete_versions(self) -> None:
        entered = threading.Event()
        release = threading.Event()
        old = self.service.snapshot
        original = self.service._build_batch

        def blocked_build(*args: object, **kwargs: object):
            entered.set()
            release.wait(timeout=5)
            return original(*args, **kwargs)

        errors: list[ServiceError] = []

        def commit() -> None:
            try:
                self.service.commit_batch([SetObstacle(10, 10, True)])
            except ServiceError as exc:
                errors.append(exc)

        with patch.object(self.service, "_build_batch", side_effect=blocked_build):
            thread = threading.Thread(target=commit)
            thread.start()
            self.assertTrue(entered.wait(timeout=5))
            observed = self.service.get_path(
                0,
                (10, 0),
                expected_version=old.version,
                expected_hash=old.version_hash,
            )
            self.assertEqual(observed.version, old.version)
            self.assertEqual(observed.version_hash, old.version_hash)
            release.set()
            thread.join(timeout=10)
        self.assertFalse(errors)
        self.assertEqual(self.service.version, 1)


if __name__ == "__main__":
    unittest.main()
