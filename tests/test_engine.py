import os
import tempfile
import threading
import time
import unittest

from pcwb.engine import EditEngine
from pcwb.errors import E_CANCELLED, PointCloudError
from pcwb.format import content_hash, encode_cloud
from pcwb.samples import generate_cloud
from pcwb.selection import sphere_select
from pcwb.versions import VersionStore


class EngineTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.store = VersionStore(os.path.join(self.tmp.name, "store"))
        xyz, rgb = generate_cloud(5, 1200)
        self.base = self.store.commit(xyz, rgb, record={"operator": "import"})
        self.engine = EditEngine(self.store, self.base)

    def tearDown(self):
        self.engine.shutdown()
        self.tmp.cleanup()

    def test_background_commit_publishes_whole_snapshot(self):
        cloud = self.engine.current()
        sel = sphere_select(cloud, (0, 0, 0), 2.5)
        self.engine.submit("color_cover", sel, {"rgb": [1, 2, 3]})
        self.assertTrue(self.engine.wait(timeout=20))
        outcome = self.engine.last_outcome
        self.assertTrue(outcome.ok)
        new_cloud = self.engine.current()
        self.assertEqual(new_cloud.version, cloud.version + 1)
        self.assertEqual(new_cloud.sha256,
                         self.store.head().sha256)
        self.assertNotEqual(new_cloud.sha256, cloud.sha256)

    def test_render_thread_never_sees_partial_update(self):
        """Concurrent readers always observe a complete committed cloud."""
        seen_versions: list[int] = []
        stop = threading.Event()
        errors: list[Exception] = []

        def reader():
            while not stop.is_set():
                try:
                    snap = self.engine.current()
                    # snapshot integrity: arrays are consistent and frozen
                    self.assertEqual(snap.xyz.shape, snap.rgb.shape)
                    self.assertFalse(snap.xyz.flags.writeable)
                    expected = content_hash(
                        encode_cloud(snap.xyz, snap.rgb, snap.bbox))
                    self.assertEqual(expected, snap.sha256)
                    seen_versions.append(snap.version)
                except Exception as exc:  # pragma: no cover
                    errors.append(exc)

        thread = threading.Thread(target=reader)
        thread.start()
        for _ in range(3):
            cur = self.engine.current()
            job_sel = sphere_select(cur, (-2, -2, -2), 1.5)
            self.engine.submit("downsample", job_sel, {"voxel_size": 0.8})
            self.assertTrue(self.engine.wait(timeout=30))
        stop.set()
        thread.join(timeout=5)
        self.assertEqual(errors, [])
        self.assertIn(1, seen_versions)
        self.assertEqual(self.engine.current().version, 4)

    def test_cooperative_cancel_preserves_committed_version(self):
        cloud = self.engine.current()
        sel = sphere_select(cloud, (0, 0, 0), 2.0)
        before = self.engine.current().version
        self.engine.submit(
            "smooth", sel, {"radius": 0.3, "iterations": 50})
        self.engine.cancel()
        self.assertTrue(self.engine.wait(timeout=20))
        outcome = self.engine.last_outcome
        self.assertFalse(outcome.ok)
        self.assertEqual(outcome.error_code, E_CANCELLED)
        self.assertEqual(self.engine.current().version, before)
        self.assertEqual(self.store.head_version, before)
        # all staging buffers released
        self.assertEqual(os.listdir(self.store.tmp_dir), [])

    def test_progress_reported_while_running(self):
        cloud = self.engine.current()
        sel = sphere_select(cloud, (0, 0, 0), 2.5)
        self.engine.submit(
            "smooth", sel, {"radius": 0.4, "iterations": 30})
        states = []
        deadline = time.time() + 20
        while time.time() < deadline:
            state = self.engine.progress()
            if state.get("running"):
                states.append(state["progress"])
            if self.engine.wait(timeout=0.05):
                break
        self.assertTrue(self.engine.last_outcome.ok)
        self.assertTrue(all(0.0 <= p <= 1.0 for p in states))

    def test_stale_selection_rejected_before_background_run(self):
        sel = sphere_select(self.base, (0, 0, 0), 2.0)
        # mutate the engine's head to a different cloud version
        other_xyz, other_rgb = generate_cloud(99, 300)
        other = self.store.commit(other_xyz, other_rgb,
                                  record={"operator": "other"})
        self.engine.publish(other)
        with self.assertRaises(PointCloudError):
            self.engine.submit("crop", sel, {"mode": "delete"})


if __name__ == "__main__":
    unittest.main()
