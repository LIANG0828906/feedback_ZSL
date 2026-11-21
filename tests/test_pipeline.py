import os

from syncbench.errors import E_BUSY, E_CANCELLED, SyncError
from syncbench.pipeline import (
    STRATEGY_SEGMENTED,
    STRATEGY_WHOLE,
    SyncConfig,
    SyncSession,
)

from .base import SampleSuiteTestCase


class TestPipeline(SampleSuiteTestCase):
    def _run(self, dataset, out_name, config=None, session=None):
        session = session or SyncSession()
        config = config or SyncConfig()
        trace = os.path.join(self.out_dir(out_name), "trace.log")
        return session.run(
            self.dataset_dir(dataset), config, self.out_dir(out_name), trace
        ), session

    def test_strategy_selection_whole_for_short(self):
        result, _ = self._run("skew4", "p_whole",
                              SyncConfig(strategy_threshold_s=60.0))
        self.assertEqual(result["strategy"], STRATEGY_WHOLE)
        self.assertGreater(result["n_segments"], 0)

    def test_strategy_selection_segmented_for_long(self):
        result, _ = self._run("skew4", "p_seg")
        self.assertEqual(result["strategy"], STRATEGY_SEGMENTED)
        self.assertEqual(result["n_segments"], 12)

    def test_strategy_comparison_present(self):
        result, _ = self._run("skew4", "p_cmp")
        comp = result["strategy_comparison"]
        self.assertIsNotNone(comp["segmented"]["residual_rms_mean"])
        self.assertIsNotNone(comp["whole"]["residual_rms_mean"])
        self.assertLessEqual(
            comp["segmented"]["residual_rms_mean"],
            comp["whole"]["residual_rms_mean"] + 1e-9,
        )

    def test_mode_comparison_and_tolerance(self):
        result, _ = self._run("skew4", "p_modes")
        self.assertIn("linear", result["mode_comparison"])
        self.assertIn("sinc", result["mode_comparison"])
        for mode, stats in result["mode_stats"].items():
            self.assertTrue(stats["within_tolerance"], mode)

    def test_stitch_aligned_and_atomic(self):
        result, _ = self._run("skew4", "p_stitch")
        shape = result["stitch_shape"]
        self.assertEqual(shape[0], 4)
        self.assertGreater(shape[1], 0)
        self.assertTrue(os.path.exists(result["stitch_path"]))
        self.assertTrue(os.path.exists(result["report_path"]))

    def test_incremental_recompute(self):
        session = SyncSession()
        config = SyncConfig()
        r1, _ = self._run("skew4", "p_inc1", config, session)
        self.assertGreater(r1["cache_stats"]["entries"], 0)
        r2, _ = self._run("skew4", "p_inc2", config, session)
        stats = r2["cache_stats"]
        self.assertGreater(stats["hits"], 0)
        hits_before = stats["hits"]
        config2 = SyncConfig(ref_channel=1)
        r3, _ = self._run("skew4", "p_inc3", config2, session)
        self.assertGreater(r3["cache_stats"]["misses"], stats["misses"])
        self.assertGreaterEqual(r3["cache_stats"]["hits"], hits_before)

    def test_fit_diff_on_segment_length_change(self):
        session = SyncSession()
        self._run("skew4", "p_fd1", SyncConfig(seg_len_s=2.0), session)
        result, _ = self._run("skew4", "p_fd2", SyncConfig(seg_len_s=4.0), session)
        diffs = [c["fit_diff"] for c in result["channels"] if c["fit_diff"]]
        self.assertTrue(diffs)
        for diff in diffs:
            self.assertIn("delta_ppm", diff)
            self.assertLess(abs(diff["delta_ppm"]), 5.0)

    def test_busy_and_cancel(self):
        session = SyncSession()
        self.assertTrue(session.try_acquire())
        with self.assertRaises(SyncError) as ctx:
            session.run(self.dataset_dir("skew4"), SyncConfig(),
                        self.out_dir("p_busy"), None)
        self.assertEqual(ctx.exception.code, E_BUSY)
        session.release()
        session.cancel()
        with self.assertRaises(SyncError) as ctx:
            session.run(self.dataset_dir("skew4"), SyncConfig(),
                        self.out_dir("p_cancel"), None)
        self.assertEqual(ctx.exception.code, E_CANCELLED)
        self.assertTrue(session.try_acquire())
        session.release()

    def test_deterministic_report_and_trace(self):
        r1, _ = self._run("skew4", "p_det1")
        r2, _ = self._run("skew4", "p_det2")
        with open(r1["report_path"], "rb") as f1, open(r2["report_path"], "rb") as f2:
            self.assertEqual(f1.read(), f2.read())
        with open(r1["trace_path"], "rb") as f1, open(r2["trace_path"], "rb") as f2:
            self.assertEqual(f1.read(), f2.read())

    def test_diagnosis_in_report(self):
        result, _ = self._run("jump4", "p_jump")
        diag = result["channels"][2]["diagnosis"]
        self.assertEqual(diag["conclusion"], "EVENT_JUMP")
        result, _ = self._run("unknown4", "p_unknown")
        diag = result["channels"][1]["diagnosis"]
        self.assertEqual(diag["conclusion"], "E_DRIFT_UNKNOWN")
