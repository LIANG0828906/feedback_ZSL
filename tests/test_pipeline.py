"""Acceptance tests covering all eight business requirements."""

from __future__ import annotations

import copy
import os
import shutil
import tempfile
import unittest
from concurrent.futures import ThreadPoolExecutor

import numpy as np

from terrain_geo import SCHEMA_VERSION, ErrorCode, GeoError
from terrain_geo.factory import build_stages
from terrain_geo.io_utils import load_image
from terrain_geo.persistence import SnapshotStore
from terrain_geo.pipeline import Pipeline
from terrain_geo.resample import ResampleConfig, resample_full, resample_tiled
from terrain_geo.samplegen import generate
from terrain_geo.stages import execute_stage
from terrain_geo.validation import validate_geometry


class TerrainGeoTestBase(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.root = tempfile.mkdtemp(prefix="terrain_geo_")
        cls.bundle = generate(cls.root)
        cls.rgb = load_image(cls.bundle["rgb_path"])
        cls.gray = load_image(cls.bundle["gray_path"])
        cls.specs = cls.bundle["stage_specs"]
        cls.truth = cls.bundle["truth"]

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.root, ignore_errors=True)

    def workdir(self, name):
        path = os.path.join(self.root, name)
        os.makedirs(path, exist_ok=True)
        return path

    def cfg(self, order=2, tile=48, overlap=16):
        return ResampleConfig(tile_size=tile, order=order,
                              max_overlap=overlap)

    def make_pipeline(self, workdir, specs=None, workers=4, cfg=None):
        cfg = cfg or self.cfg()
        specs = specs or self.specs
        return Pipeline(build_stages(specs, cfg), workdir,
                        SCHEMA_VERSION, max_workers=workers)


class TestStageChain(TerrainGeoTestBase):
    def test_channel_preservation_and_sizes(self):
        for image, channels in ((self.gray, 1), (self.rgb, 3)):
            stages = build_stages(self.specs, self.cfg())
            current = image
            for stage, spec in zip(stages, self.specs):
                current = execute_stage(stage, current)
                self.assertEqual(tuple(current.shape[:2]),
                                 tuple(spec["params"]["output_size"]))
                self.assertEqual(current.ndim, image.ndim)
                if channels == 1:
                    self.assertEqual(current.ndim, 2)
                else:
                    self.assertEqual(current.shape[2], 3)

    def test_param_range_validation_names_stage_and_field(self):
        cases = [
            (1, "k1", 5.0),
            (1, "k2", -5.0),
            (2, "scale[0]", [0.0, 1.0]),
            (2, "translate[0]", [1e9, 0.0]),
        ]
        for stage_idx, field_name, bad in cases:
            specs = copy.deepcopy(self.specs)
            base_field = field_name.split("[")[0]
            if isinstance(bad, list):
                specs[stage_idx]["params"][base_field] = bad
            else:
                specs[stage_idx]["params"][field_name] = bad
            with self.assertRaises(GeoError) as ctx:
                self.make_pipeline(
                    self.workdir(f"para_{stage_idx}_{field_name}"),
                    specs).run(self.rgb)
            self.assertEqual(ctx.exception.code, ErrorCode.E_PARA)
            self.assertEqual(ctx.exception.field, field_name)
            self.assertIsNotNone(ctx.exception.stage)

    def test_singular_homography_rejected(self):
        specs = copy.deepcopy(self.specs)
        specs[0]["params"]["H"] = np.zeros((3, 3)).tolist()
        with self.assertRaises(GeoError) as ctx:
            self.make_pipeline(self.workdir("badH"), specs).run(self.rgb)
        self.assertEqual(ctx.exception.code, ErrorCode.E_PARA)
        self.assertEqual(ctx.exception.field, "H")


class TestCachingAndInvalidation(TerrainGeoTestBase):
    def test_committed_cache_is_externally_immutable(self):
        pipeline = self.make_pipeline(self.workdir("immutable"))
        result = pipeline.run(self.rgb)
        key = result.statuses[0].cache_key
        snapshot = pipeline.cache[key]
        snapshot.flat[0] = 255 - snapshot.flat[0]
        np.testing.assert_array_equal(
            pipeline.committed_output(key), result.statuses[0].output)

    def test_only_downstream_stages_recompute(self):
        workdir = self.workdir("incremental")
        pipeline = self.make_pipeline(workdir)
        first = pipeline.run(self.rgb)
        self.assertEqual(first.commit_sequence,
                         ["homography", "radial_correction", "affine"])
        second = pipeline.run(self.rgb)
        self.assertEqual(second.commit_sequence, [])
        self.assertTrue(all(s.state == "reused"
                            for s in second.statuses))
        new_specs = copy.deepcopy(self.specs)
        new_specs[1]["params"]["k1"] = 0.24
        new_specs[1]["param_version"] = "r-1.1.0"
        stages = build_stages(new_specs, self.cfg())
        pipeline.update_stage_params("radial_correction",
                                     stages[1].params, "r-1.1.0")
        third = pipeline.run(self.rgb)
        states = {s.stage_id: s.state for s in third.statuses}
        self.assertEqual(states["homography"], "reused")
        self.assertEqual(states["radial_correction"], "recomputed")
        self.assertEqual(states["affine"], "recomputed")
        listing = third.invalidation_listing()
        self.assertEqual(len(listing), 3)
        self.assertTrue(all(item["reason"] for item in listing))

    def test_cache_key_includes_upstream_and_content(self):
        from terrain_geo.stages import stage_cache_key
        stages = build_stages(self.specs, self.cfg())
        key_a = stage_cache_key(stages[0], "hash-in", None)
        key_b = stage_cache_key(stages[0], "hash-other", None)
        key_c = stage_cache_key(stages[0], "hash-in", "upstream")
        self.assertNotEqual(key_a, key_b)
        self.assertNotEqual(key_a, key_c)


class TestGeometryValidation(TerrainGeoTestBase):
    def test_reprojection_within_tolerance(self):
        report = validate_geometry(build_stages(self.specs, self.cfg()),
                                   self.truth, tolerance=0.35)
        self.assertTrue(report.ok)
        for stage_report in report.stages:
            self.assertLess(stage_report.upper, 0.35)
            self.assertGreaterEqual(stage_report.median, 0.0)

    def test_over_tolerance_raises_e_geom_with_point_ids(self):
        bad_truth = copy.deepcopy(self.truth)
        bad_truth["points_by_stage"]["affine"][0] = [
            bad_truth["points_by_stage"]["affine"][0][0] + 5.0,
            bad_truth["points_by_stage"]["affine"][0][1]]
        with self.assertRaises(GeoError) as ctx:
            validate_geometry(build_stages(self.specs, self.cfg()),
                              bad_truth, tolerance=0.35)
        self.assertEqual(ctx.exception.code, ErrorCode.E_GEOM)
        self.assertTrue(ctx.exception.detail["failures"])
        first = ctx.exception.detail["failures"][0]
        self.assertIn("id", first)
        self.assertEqual(len(first["measured"]), 2)


class TestTiledResampling(TerrainGeoTestBase):
    def test_tiled_matches_single_shot_all_orders(self):
        stages = build_stages(self.specs, self.cfg())
        current = self.rgb
        for stage in stages:
            stage.validate()
            inverse = stage.inverse_map()
            h, w = stage.output_size
            for order in (0, 1, 2):
                cfg = ResampleConfig(tile_size=37, order=order,
                                     max_overlap=20)
                tiled = resample_tiled(current, inverse, h, w, cfg,
                                       stage.stage_id)
                full = resample_full(current, inverse, h, w, order)
                np.testing.assert_array_equal(tiled, full)
            current = execute_stage(stage, current)

    def test_tiled_matches_single_shot_single_channel(self):
        stages = build_stages(self.specs, self.cfg(order=1))
        current = self.gray
        for stage in stages:
            stage.validate()
            h, w = stage.output_size
            cfg = ResampleConfig(tile_size=40, order=1, max_overlap=8)
            tiled = resample_tiled(current, stage.inverse_map(), h, w,
                                   cfg, stage.stage_id)
            full = resample_full(current, stage.inverse_map(), h, w, 1)
            np.testing.assert_array_equal(tiled, full)
            current = execute_stage(stage, current)

    def test_insufficient_band_raises_e_band(self):
        tight = ResampleConfig(tile_size=32, order=2, max_overlap=0)
        with self.assertRaises(GeoError) as ctx:
            self.make_pipeline(self.workdir("band"), cfg=tight).run(self.rgb)
        self.assertEqual(ctx.exception.code, ErrorCode.E_BAND)
        self.assertIsNotNone(ctx.exception.stage)

    def test_buffer_cap_enforced(self):
        tight = ResampleConfig(tile_size=128, order=1,
                               max_overlap=20,
                               max_pixels_per_thread=1024)
        with self.assertRaises(GeoError) as ctx:
            self.make_pipeline(self.workdir("buffer"), cfg=tight).run(
                self.rgb)
        self.assertEqual(ctx.exception.code, ErrorCode.E_BAND)


class TestIncrementalConsistency(TerrainGeoTestBase):
    def test_incremental_equals_cold_full_recompute(self):
        workdir = self.workdir("consistency")
        pipeline = self.make_pipeline(workdir, workers=4)
        pipeline.run(self.rgb)
        new_specs = copy.deepcopy(self.specs)
        new_specs[0]["params"]["H"][0][0] = 0.78
        new_specs[0]["param_version"] = "h-1.1.0"
        stages = build_stages(new_specs, self.cfg())
        pipeline.update_stage_params("homography", stages[0].params,
                                     "h-1.1.0")
        result = pipeline.run(self.rgb)
        ok, _ = pipeline.assert_matches_full(self.rgb, result)
        self.assertTrue(ok)

    def test_parallel_matches_serial_and_commit_order(self):
        serial = self.make_pipeline(self.workdir("serial"),
                                    workers=1).run(self.rgb)
        parallel = self.make_pipeline(self.workdir("parallel"),
                                      workers=4).run(self.rgb)
        np.testing.assert_array_equal(serial.output, parallel.output)
        self.assertEqual(serial.commit_sequence, parallel.commit_sequence)

    def test_diff_detection(self):
        pipeline = self.make_pipeline(self.workdir("diff"))
        result = pipeline.run(self.rgb)
        def mutate(stage_id, output):
            if stage_id == "homography":
                tainted = output.copy()
                tainted.flat[0] = 255 - tainted.flat[0]
                return tainted
            return output

        pipeline.reference_mutator = mutate
        with self.assertRaises(GeoError) as ctx:
            pipeline.assert_matches_full(self.rgb, result)
        self.assertEqual(ctx.exception.code, ErrorCode.E_DIFF)
        self.assertEqual(ctx.exception.stage, "homography")
        detail = ctx.exception.detail["first_pixel"]
        self.assertEqual(detail[:2], [0, 0])
        with self.assertRaises(GeoError) as ctx:
            pipeline.assert_matches_full(self.rgb, result)
        self.assertEqual(ctx.exception.code, ErrorCode.E_DIFF)


class TestSnapshotPersistence(TerrainGeoTestBase):
    def test_restart_restores_verified_snapshot(self):
        workdir = self.workdir("snapshot")
        first_pipeline = self.make_pipeline(workdir)
        first = first_pipeline.run(self.rgb)
        restarted = self.make_pipeline(workdir)
        second = restarted.run(self.rgb)
        np.testing.assert_array_equal(first.output, second.output)
        self.assertTrue(all(s.state == "reused" for s in second.statuses))

    def test_version_mismatch_invalidates_entire_snapshot(self):
        workdir = self.workdir("snapshot_mismatch")
        self.make_pipeline(workdir).run(self.rgb)
        tampered = copy.deepcopy(self.specs)
        tampered[2]["param_version"] = "a-9.9.9"
        stale = self.make_pipeline(workdir, specs=tampered)
        result = stale.run(self.rgb)
        self.assertTrue(all(s.state == "recomputed"
                            for s in result.statuses))

    def test_blob_tampering_invalidates_snapshot(self):
        workdir = self.workdir("snapshot_tamper")
        self.make_pipeline(workdir).run(self.rgb)
        snap_dir = os.path.join(workdir, "snapshots")
        blob = next(name for name in os.listdir(snap_dir)
                    if name.endswith(".npy"))
        path = os.path.join(snap_dir, blob)
        data = np.load(path)
        data.flat[0] = (int(data.flat[0]) + 1) % 256
        np.save(path, data)
        result = self.make_pipeline(workdir).run(self.rgb)
        self.assertTrue(all(s.state == "recomputed"
                            for s in result.statuses))


class TestErrorAndRetryIsolation(TerrainGeoTestBase):
    def test_decode_error_on_corrupt_file(self):
        path = os.path.join(self.workdir("decode"), "bad.png")
        with open(path, "wb") as fh:
            fh.write(b"not a real png")
        with self.assertRaises(GeoError) as ctx:
            load_image(path)
        self.assertEqual(ctx.exception.code, ErrorCode.E_DECODE)

    def test_stage_failure_isolated_and_retryable(self):
        workdir = self.workdir("retry")
        pipeline = self.make_pipeline(workdir)
        pipeline.set_fault("radial_correction", GeoError(
            ErrorCode.E_INTERNAL, "boom", stage="radial_correction"))
        with self.assertRaises(GeoError) as ctx:
            pipeline.run(self.rgb)
        self.assertEqual(ctx.exception.code, ErrorCode.E_INTERNAL)
        # Homography was committed before the fault and stays reusable.
        result = pipeline.run(self.rgb)
        states = {s.stage_id: s.state for s in result.statuses}
        self.assertEqual(states["homography"], "reused")
        self.assertEqual(states["radial_correction"], "recomputed")

    def test_failure_leaves_no_partial_snapshot(self):
        workdir = self.workdir("no_partial")
        pipeline = self.make_pipeline(workdir)
        pipeline.set_fault("homography", GeoError(
            ErrorCode.E_INTERNAL, "boom", stage="homography"))
        with self.assertRaises(GeoError):
            pipeline.run(self.rgb)
        snap_dir = os.path.join(workdir, "snapshots")
        self.assertFalse(os.path.exists(
            os.path.join(snap_dir, "manifest.json")))
        self.assertFalse(any(name.endswith(".tmp")
                             for name in os.listdir(snap_dir)))

    def test_tile_size_zero_single_shot_path(self):
        cfg = ResampleConfig(tile_size=0, order=1)
        result = self.make_pipeline(self.workdir("single_shot"),
                                    cfg=cfg).run(self.rgb)
        self.assertEqual(result.output.shape,
                         tuple(self.specs[-1]["params"]["output_size"]) + (3,))


class TestConcurrencyBounds(TerrainGeoTestBase):
    def test_max_workers_must_be_positive(self):
        with self.assertRaises(GeoError) as ctx:
            Pipeline(build_stages(self.specs, self.cfg()),
                     self.workdir("workers"), SCHEMA_VERSION, max_workers=0)
        self.assertEqual(ctx.exception.code, ErrorCode.E_PARA)

    def test_thread_pool_bounds_and_deterministic_order(self):
        import threading
        seen = set()
        active = {"max": 0}
        lock = threading.Lock()
        original_submit = ThreadPoolExecutor.submit

        def tracked_submit(self_pool, fn, *args, **kwargs):
            def wrapper(*a, **kw):
                with lock:
                    seen.add(threading.get_ident())
                    current = 1 + getattr(wrapper, "_depth", 0)
                    active["max"] = max(active["max"], current)
                return fn(*a, **kw)
            return original_submit(self_pool, wrapper, *args, **kwargs)

        ThreadPoolExecutor.submit = tracked_submit
        try:
            result = self.make_pipeline(
                self.workdir("pool"), workers=3).run(self.rgb)
        finally:
            ThreadPoolExecutor.submit = original_submit
        self.assertLessEqual(len(seen), 3)
        self.assertEqual(result.commit_sequence,
                         ["homography", "radial_correction", "affine"])

    def test_per_thread_pixel_buffer_cap(self):
        cfg = ResampleConfig(tile_size=96, order=1, max_overlap=8,
                             max_pixels_per_thread=100)
        with self.assertRaises(GeoError) as ctx:
            self.make_pipeline(self.workdir("cap"), cfg=cfg).run(self.rgb)
        self.assertEqual(ctx.exception.code, ErrorCode.E_BAND)

    def test_repeated_parallel_runs_are_bit_stable(self):
        pipeline = self.make_pipeline(self.workdir("bitstable"), workers=4)
        first = pipeline.run(self.rgb).output
        for _ in range(2):
            again = self.make_pipeline(
                self.workdir("bitstable"), workers=4).run(self.rgb).output
            np.testing.assert_array_equal(first, again)


if __name__ == "__main__":
    unittest.main(verbosity=2)
