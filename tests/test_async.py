from __future__ import annotations

import tempfile
import time
import unittest
from pathlib import Path

from pcwb.async_edit import AsyncEditController
from pcwb.cache import OperationCache
from pcwb.errors import OperationCancelled
from pcwb.metrics import InteractionMetrics
from pcwb.operators import run_operator
from pcwb.probe import SnapshotProbeLoop
from pcwb.repository import PointCloudRepository
from tests.helpers import all_selection, sample_cloud


def slow_runner(
    base,
    selection,
    operator,
    params,
    *,
    cancel_event=None,
    progress=None,
):
    steps = 80
    for index in range(steps):
        if cancel_event is not None and cancel_event.is_set():
            raise OperationCancelled("cancelled by test")
        if progress is not None:
            progress("slow", (index + 1) / steps)
        time.sleep(0.004)
    return run_operator(base, selection, "color_overwrite", {"rgb": [12, 34, 56]})


class AsyncEditTests(unittest.TestCase):
    def setUp(self) -> None:
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name) / "repo"
        cloud = sample_cloud(25)
        self.repo = PointCloudRepository.create(self.root, cloud)
        self.base = self.repo.current_snapshot
        self.selection = all_selection(cloud, version=1)
        self.metrics = InteractionMetrics()

    def tearDown(self) -> None:
        self.directory.cleanup()

    def test_renderer_sees_only_old_snapshot_until_single_swap(self) -> None:
        controller = AsyncEditController(
            self.repo,
            progress_threshold_ms=10.0,
            operator_runner=slow_runner,
        )
        probe = SnapshotProbeLoop(controller, self.metrics, sleep_seconds=0.001)
        probe.start()
        try:
            controller.submit("color_overwrite", {}, self.selection)
            time.sleep(0.06)
            during = controller.display_snapshot()
            status = controller.status()
            self.assertEqual(during.version, 1)
            self.assertEqual(during.content_hash, self.base.content_hash)
            self.assertIn(status.state, {"running", "committing"})
            self.assertTrue(status.show_progress)
            self.assertGreater(self.metrics.frames.stats().count, 0)
            self.assertTrue(controller.wait(5.0))
            after = controller.display_snapshot()
            self.assertEqual(after.version, 2)
            self.assertEqual(after.content_hash, controller.last_commit.output_hash)
            self.assertEqual(self.repo.current_snapshot.version, 2)
        finally:
            report = probe.stop()
            controller.close()
        self.assertTrue(report.old_snapshot_observed_while_running)
        self.assertGreaterEqual(report.frames, 2)

    def test_cancel_retains_published_version_and_discards_work(self) -> None:
        controller = AsyncEditController(
            self.repo,
            progress_threshold_ms=5.0,
            operator_runner=slow_runner,
        )
        controller.submit("color_overwrite", {}, self.selection)
        time.sleep(0.03)
        self.assertTrue(controller.cancel())
        self.assertTrue(controller.wait(5.0))
        self.assertEqual(controller.status().state, "cancelled")
        self.assertEqual(controller.display_snapshot().version, 1)
        self.assertEqual(self.repo.current_snapshot.version, 1)
        self.assertFalse((self.root / "objects" / "v00000002.pcwb").exists())
        self.assertFalse(list((self.root / "objects").glob("*.tmp")))
        self.assertIsNone(controller.last_commit)
        controller.close()

    def test_commit_failure_keeps_renderer_on_old_snapshot(self) -> None:
        controller = AsyncEditController(self.repo, progress_threshold_ms=5.0)

        def fail_manifest(_manifest):
            raise OSError("injected failure")

        self.repo._write_manifest_atomic = fail_manifest  # type: ignore[method-assign]
        controller.submit("color_overwrite", {"rgb": [1, 1, 1]}, self.selection)
        self.assertTrue(controller.wait(5.0))
        status = controller.status()
        self.assertEqual(status.state, "failed")
        self.assertEqual(status.error["code"], "E_COMMIT_FAILED")
        self.assertEqual(controller.display_snapshot().version, 1)
        self.assertEqual(self.repo.current_snapshot.version, 1)
        self.assertFalse((self.root / "objects" / "v00000002.pcwb").exists())
        controller.close()

    def test_cache_rejects_future_lineage_and_corrupt_payload(self) -> None:
        cache = OperationCache()
        result = run_operator(
            self.base, self.selection, "color_overwrite", {"rgb": [2, 4, 6]}
        )
        cache.put(self.base, "color_overwrite", {"rgb": [2, 4, 6]}, result.cloud)
        hit = cache.get(self.base, "color_overwrite", {"rgb": [2, 4, 6]})
        self.assertIsNotNone(hit)
        self.assertEqual(hit.content_hash, result.cloud.content_hash)

        past = type(self.base)(
            version=0,
            content_hash=self.base.content_hash,
            cloud=self.base.cloud,
        )
        self.assertIsNone(cache.get(past, "color_overwrite", {"rgb": [2, 4, 6]}))
        self.assertGreaterEqual(cache.stats().misses, 1)

        cache.put(self.base, "color_overwrite", {"rgb": [2, 4, 6]}, result.cloud)
        key = cache.make_key(self.base, "color_overwrite", {"rgb": [2, 4, 6]})
        entry = cache._entries[key]
        cache._entries[key] = type(entry)(
            input_hash=entry.input_hash,
            source_version=entry.source_version,
            operator=entry.operator,
            params=entry.params,
            output_hash=entry.output_hash,
            payload=b"corrupt",
        )
        self.assertIsNone(cache.get(self.base, "color_overwrite", {"rgb": [2, 4, 6]}))
        self.assertGreaterEqual(cache.stats().invalid, 1)


if __name__ == "__main__":
    unittest.main()
