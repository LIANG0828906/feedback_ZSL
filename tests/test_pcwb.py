"""点云工作台验收测试：格式、分块、LOD、边界一致性、算子、版本、线程隔离。"""

import os
import shutil
import struct
import tempfile
import threading
import time
import unittest

import numpy as np

from pcwb import pcformat
from pcwb.blocks import build_blocks, rebuild
from pcwb.errors import (BlockSeam, CommitFailed, LevelBudget, OpInvalid,
                         PointcloudInvalid, StaleHandle)
from pcwb.lod import (sampling_sequence, select_display,
                      verify_seam_consistency)
from pcwb.ops import (apply_operator, select_box, select_sphere)
from pcwb.runtime import Workbench
from pcwb.samplegen import generate_cloud
from pcwb.store import VersionStore


def small_cloud(count=2000, seed=42):
    return generate_cloud(count, seed)


class TempDirCase(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp(prefix="pcwb-test-")

    def tearDown(self):
        shutil.rmtree(self.dir, ignore_errors=True)


class TestFormat(TempDirCase):
    def test_roundtrip_and_hash(self):
        cloud = small_cloud()
        path = os.path.join(self.dir, "a.pcwb")
        hash1 = pcformat.write(path, cloud)
        loaded = pcformat.read(path)
        self.assertEqual(loaded.count, cloud.count)
        np.testing.assert_array_equal(loaded.points, cloud.points)
        np.testing.assert_array_equal(loaded.colors, cloud.colors)
        self.assertEqual(hash1, loaded.content_hash())
        self.assertEqual(hash1, pcformat.write(path, loaded))

    def test_bad_magic(self):
        path = os.path.join(self.dir, "bad.pcwb")
        with open(path, "wb") as fh:
            fh.write(b"XXXX" + b"\x00" * 100)
        with self.assertRaises(PointcloudInvalid) as ctx:
            pcformat.read(path)
        self.assertEqual(ctx.exception.detail.get("field"), "magic")

    def test_length_mismatch_locates_point_count(self):
        cloud = small_cloud(100)
        path = os.path.join(self.dir, "b.pcwb")
        pcformat.write(path, cloud)
        with open(path, "r+b") as fh:
            fh.seek(12)
            fh.write(struct.pack("<Q", 999))
        with self.assertRaises(PointcloudInvalid) as ctx:
            pcformat.read(path)
        self.assertEqual(ctx.exception.detail.get("field"), "point_count")

    def test_out_of_bbox_locates_point_index(self):
        cloud = small_cloud(100)
        path = os.path.join(self.dir, "c.pcwb")
        pcformat.write(path, cloud)
        payload = bytearray(pcformat.encode(cloud))
        off = pcformat.HEADER_SIZE + 7 * pcformat.POINT_SIZE
        payload[off:off + 4] = struct.pack("<f", 1.0e30)
        with open(path, "wb") as fh:
            fh.write(payload)
        with self.assertRaises(PointcloudInvalid) as ctx:
            pcformat.read(path)
        self.assertEqual(ctx.exception.detail.get("point_index"), 7)


class TestBlocks(TempDirCase):
    def test_deterministic_rebuild(self):
        cloud = small_cloud(5000)
        idx1 = build_blocks(cloud, 10.0)
        idx2 = build_blocks(cloud, 10.0)
        self.assertEqual(sorted(idx1.blocks), sorted(idx2.blocks))
        for cid in idx1.blocks:
            b1, b2 = idx1.blocks[cid], idx2.blocks[cid]
            self.assertEqual(b1.content_hash, b2.content_hash)
            np.testing.assert_array_equal(b1.indices, b2.indices)

    def test_block_union_covers_all_points_in_order(self):
        cloud = small_cloud(5000)
        idx = build_blocks(cloud, 10.0)
        all_idx = np.sort(np.concatenate([b.indices for b in idx.blocks.values()]))
        np.testing.assert_array_equal(all_idx, np.arange(cloud.count))

    def test_rebuild_report(self):
        cloud = small_cloud(5000)
        idx0 = build_blocks(cloud, 10.0)
        sel = select_box(cloud.points, [40, 40, 40], [60, 60, 60])
        pts, cols = apply_operator("crop", cloud.points, cloud.colors,
                                   sel, {"mode": "remove"})
        new_cloud = pcformat.PointCloud(pts, cols)
        idx1, report = rebuild(new_cloud, 10.0, idx0)
        self.assertEqual(report.old_total_points, cloud.count)
        self.assertEqual(report.new_total_points, new_cloud.count)
        self.assertEqual(report.point_diff, new_cloud.count - cloud.count)
        self.assertGreater(len(report.replaced_blocks), 0)
        self.assertEqual(report.new_block_count, len(idx1.blocks))


class TestLod(TempDirCase):
    def test_budget_error_reports_usage(self):
        cloud = small_cloud(3000)
        idx = build_blocks(cloud, 5.0)
        with self.assertRaises(LevelBudget) as ctx:
            select_display(idx, [50, 50, 50], budget=1, seed=0)
        self.assertIn("usage", ctx.exception.detail)

    def test_sampling_sequence_recorded(self):
        cloud = small_cloud(3000)
        idx = build_blocks(cloud, 10.0)
        sel = select_display(idx, [200, 200, 200], budget=500, seed=0)
        self.assertGreater(sel.visible_block_count, 0)
        for block_display in sel.blocks:
            self.assertEqual(block_display.displayed,
                             block_display.kept_indices.shape[0])

    def test_seam_consistency_multiblock_vs_single(self):
        cloud = small_cloud(8000)
        idx = build_blocks(cloud, 10.0)
        for level in (0, 1, 2, 3):
            truth = verify_seam_consistency(idx, level, seed=0)
            np.testing.assert_array_equal(
                truth, sampling_sequence(cloud.count, level, 0))

    def test_seam_error_locates_block_pair(self):
        cloud = small_cloud(4000)
        idx = build_blocks(cloud, 10.0)
        victim = sorted(idx.blocks)[0]
        idx.blocks[victim].indices = idx.blocks[victim].indices[:-1]
        with self.assertRaises(BlockSeam) as ctx:
            verify_seam_consistency(idx, 0, seed=0)
        self.assertIn("block_pair", ctx.exception.detail)


class TestOps(TempDirCase):
    def setUp(self):
        super().setUp()
        self.cloud = small_cloud(4000)
        self.sel = select_box(self.cloud.points, [30, 30, 30], [70, 70, 70])
        self.assertGreater(self.sel.shape[0], 0)

    def test_determinism_byte_identical(self):
        for op, params in [
            ("crop", {"mode": "keep"}),
            ("crop", {"mode": "remove"}),
            ("smooth", {"iterations": 2, "lambda": 0.4, "radius": 3.0}),
            ("downsample", {"ratio": 0.3, "seed": 5}),
            ("color_override", {"rgb": [1, 2, 3]}),
        ]:
            p1, c1 = apply_operator(op, self.cloud.points, self.cloud.colors,
                                    self.sel, params)
            p2, c2 = apply_operator(op, self.cloud.points, self.cloud.colors,
                                    self.sel, params)
            self.assertEqual(p1.tobytes(), p2.tobytes(), op)
            self.assertEqual(c1.tobytes(), c2.tobytes(), op)

    def test_inputs_not_mutated(self):
        p0 = self.cloud.points.copy()
        c0 = self.cloud.colors.copy()
        apply_operator("smooth", self.cloud.points, self.cloud.colors,
                       self.sel, {"iterations": 1, "lambda": 1.0, "radius": 5.0})
        apply_operator("color_override", self.cloud.points, self.cloud.colors,
                       self.sel, {"rgb": [9, 9, 9]})
        np.testing.assert_array_equal(self.cloud.points, p0)
        np.testing.assert_array_equal(self.cloud.colors, c0)

    def test_empty_selection_rejected(self):
        empty = np.empty(0, dtype=np.int64)
        for op, params in [("crop", {"mode": "keep"}),
                           ("smooth", {}),
                           ("downsample", {"ratio": 0.5}),
                           ("color_override", {"rgb": [0, 0, 0]})]:
            with self.assertRaises(OpInvalid, msg=op):
                apply_operator(op, self.cloud.points, self.cloud.colors,
                               empty, params)

    def test_invalid_params_rejected(self):
        cases = [
            ("crop", {"mode": "bogus"}),
            ("smooth", {"iterations": 0}),
            ("smooth", {"lambda": 1.5}),
            ("smooth", {"radius": -1.0}),
            ("downsample", {"ratio": 0.0}),
            ("downsample", {"target_count": 10 ** 9}),
            ("downsample", {}),
            ("color_override", {"rgb": [256, 0, 0]}),
            ("color_override", {"rgb": [0, 0]}),
        ]
        for op, params in cases:
            with self.assertRaises(OpInvalid, msg=f"{op} {params}"):
                apply_operator(op, self.cloud.points, self.cloud.colors,
                               self.sel, params)

    def test_output_limit_rejected(self):
        with self.assertRaises(OpInvalid) as ctx:
            apply_operator("crop", self.cloud.points, self.cloud.colors,
                           self.sel, {"mode": "keep", "max_output_points": 1})
        self.assertEqual(ctx.exception.detail.get("reason"), "output_limit")

    def test_sphere_selection(self):
        sel = select_sphere(self.cloud.points, [20, 30, 40], 8.0)
        self.assertGreater(sel.shape[0], 0)
        c = np.array([20, 30, 40], dtype=np.float32)
        d = np.linalg.norm(self.cloud.points[sel] - c, axis=1)
        self.assertTrue(np.all(d <= 8.0 + 1e-3))


class TestStore(TempDirCase):
    def _make_store(self, count=3000):
        cloud = small_cloud(count)
        src = os.path.join(self.dir, "src.pcwb")
        pcformat.write(src, cloud)
        store = VersionStore(os.path.join(self.dir, "work"), edge_length=10.0)
        store.load_initial(src)
        return store, cloud

    def test_monotonic_versions_and_records(self):
        store, cloud = self._make_store()
        sel = select_box(cloud.points, [40, 40, 40], [60, 60, 60])
        p1, c1 = apply_operator("color_override", cloud.points, cloud.colors,
                                sel, {"rgb": [7, 7, 7]})
        r1 = store.commit("color_override", {"rgb": [7, 7, 7]}, p1, c1)
        self.assertEqual(r1.version, 1)
        self.assertEqual(r1.input_hash, cloud.content_hash())
        self.assertEqual(r1.output_hash, pcformat.content_hash(p1, c1))
        self.assertGreater(len(r1.replaced_blocks), 0)
        p2, c2 = apply_operator("downsample", p1, c1, sel,
                                {"ratio": 0.5, "seed": 1})
        r2 = store.commit("downsample", {"ratio": 0.5, "seed": 1}, p2, c2)
        self.assertEqual(r2.version, 2)
        self.assertEqual(r2.parent, 1)
        self.assertTrue(os.path.exists(store._version_path(0)))
        self.assertTrue(os.path.exists(store._version_path(1)))
        self.assertTrue(os.path.exists(store._version_path(2)))

    def test_atomic_failure_keeps_previous(self):
        store, cloud = self._make_store()
        sel = select_box(cloud.points, [40, 40, 40], [60, 60, 60])
        p1, c1 = apply_operator("crop", cloud.points, cloud.colors,
                                sel, {"mode": "keep"})
        original_replace = os.replace
        hash_before = store.current_cloud.content_hash()
        version_before = store.current_version
        try:
            def boom(*a, **k):
                raise OSError("simulated io failure")
            os.replace = boom
            with self.assertRaises(CommitFailed):
                store.commit("crop", {"mode": "keep"}, p1, c1)
        finally:
            os.replace = original_replace
        self.assertEqual(store.current_version, version_before)
        self.assertEqual(store.current_cloud.content_hash(), hash_before)
        self.assertFalse(os.path.exists(store._version_path(1)))

    def test_rollback_appends_new_version(self):
        store, cloud = self._make_store()
        sel = select_box(cloud.points, [40, 40, 40], [60, 60, 60])
        p1, c1 = apply_operator("crop", cloud.points, cloud.colors,
                                sel, {"mode": "remove"})
        store.commit("crop", {"mode": "remove"}, p1, c1)
        r2 = store.rollback(0)
        self.assertEqual(r2.version, 2)
        self.assertEqual(r2.op, "rollback")
        self.assertEqual(r2.output_hash, cloud.content_hash())
        self.assertEqual(store.current_cloud.content_hash(),
                         cloud.content_hash())
        v0 = pcformat.read(store._version_path(0))
        self.assertEqual(v0.content_hash(), cloud.content_hash())


class TestRuntime(TempDirCase):
    def test_background_edit_and_snapshot_isolation(self):
        cloud = small_cloud(6000)
        src = os.path.join(self.dir, "src.pcwb")
        pcformat.write(src, cloud)
        store = VersionStore(os.path.join(self.dir, "work"), edge_length=10.0)
        store.load_initial(src)
        wb = Workbench(store)
        try:
            sel = select_sphere(cloud.points, [50, 50, 50], 15.0)
            stale_handle = wb.acquire()
            wb.submit_edit("color_override", {"rgb": [200, 0, 0]}, sel)
            observed = []
            stop = threading.Event()

            def render_loop():
                while not stop.is_set():
                    snap = wb.render_frame()
                    expected = pcformat.content_hash(snap.points, snap.colors)
                    observed.append(snap.content_hash == expected)

            thread = threading.Thread(target=render_loop)
            thread.start()
            result = wb.wait_result(timeout=60)
            stop.set()
            thread.join()
            self.assertTrue(result.ok)
            self.assertTrue(all(observed))
            self.assertGreater(len(observed), 0)
            with self.assertRaises(StaleHandle):
                stale_handle.read()
            snap = wb.render_frame()
            self.assertEqual(snap.version, 1)
        finally:
            wb.shutdown()

    def test_edit_does_not_block_render(self):
        cloud = small_cloud(8000)
        src = os.path.join(self.dir, "src.pcwb")
        pcformat.write(src, cloud)
        store = VersionStore(os.path.join(self.dir, "work"), edge_length=10.0)
        store.load_initial(src)
        wb = Workbench(store)
        try:
            sel = select_box(cloud.points, [30, 30, 30], [70, 70, 70])
            wb.submit_edit("smooth", {"iterations": 2, "lambda": 0.5,
                                      "radius": 4.0}, sel)
            started = time.monotonic()
            for _ in range(50):
                wb.render_frame()
            elapsed = time.monotonic() - started
            self.assertLess(elapsed, 5.0)
            result = wb.wait_result(timeout=120)
            self.assertTrue(result.ok, result.error)
        finally:
            wb.shutdown()


if __name__ == "__main__":
    unittest.main()
