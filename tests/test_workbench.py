from __future__ import annotations

import os
import tempfile
import threading
import unittest
import wave
from pathlib import Path

import numpy as np

from venue_crosstalk.config import load_config
from venue_crosstalk.controller import RecomputeController
from venue_crosstalk.errors import (
    E_BUSY,
    E_CALIB_INFEASIBLE,
    E_CANCELLED,
    E_GEOMETRY_INVALID,
    E_POLARITY_UNKNOWN,
    VenueError,
)
from venue_crosstalk.models import load_workspace
from venue_crosstalk.pipeline import AnalysisPipeline
from venue_crosstalk.samplegen import generate_sample_workspace
from venue_crosstalk.utils import atomic_write_json, read_json
from venue_crosstalk.wavio import inspect_wav


def _band_plan_with_changed_boundary(
    result: dict,
    delta_hz: float = 25.0,
) -> list[tuple[str, float, float]]:
    bands = result["effective_config"]["bands"]
    low = bands[1]
    mid = bands[2]
    return [
        (str(band["name"]), float(band["low_hz"]), float(band["high_hz"]))
        for band in bands
        if band["name"] not in {"low", "mid"}
    ] + [
        ("low", float(low["low_hz"]), float(low["high_hz"]) - delta_hz),
        ("mid", float(low["high_hz"]) - delta_hz, float(mid["high_hz"])),
    ]


def _sorted_band_plan(
    plan: list[tuple[str, float, float]]
) -> list[tuple[str, float, float]]:
    return sorted(plan, key=lambda item: item[1])


class ReadOnlyAnalysisTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory(prefix="venue-crosstalk-read-")
        cls.root = Path(cls.temp.name) / "workspace"
        cls.truth = generate_sample_workspace(
            cls.root,
            seed=424242,
            duration_s=0.35,
            overwrite=True,
        )
        cls.pipeline = AnalysisPipeline(cls.root, reset_trace=True)
        cls.result = cls.pipeline.run(
            operation="test-baseline",
            reset_trace=True,
        )
        cls.workspace = load_workspace(cls.root)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def test_geometry_alignment_and_wav_layouts(self) -> None:
        layouts = {item["layout"] for item in self.truth["pairs"]}
        self.assertEqual(layouts, {"mono", "5.1"})
        for entry in self.result["measurements"]:
            pair = self.workspace.pair_map()[entry["pair"]]
            info = inspect_wav(
                self.root / pair.wav_path,
                expected_rate=self.workspace.config.sample_rate,
            )
            self.assertEqual(info.channels, 1 if pair.layout == "mono" else 6)
        self.assertEqual(len(self.result["alignments"]), 30)
        for alignment in self.result["alignments"]:
            self.assertLessEqual(abs(alignment["residual_offset_samples"]), 2)
            basis = alignment["distance_basis"]
            self.assertGreaterEqual(basis["distance_m"], 0.0)
            self.assertIn(
                basis["kind"],
                {"euclidean_speaker_to_pickup", "pair_distance_override"},
            )

    def test_band_coupling_matches_injected_truth(self) -> None:
        truth = {item["pair"]: item for item in self.truth["pairs"]}
        count = 0
        relative_errors: list[float] = []
        phase_errors: list[float] = []
        for section in self.result["bands"]:
            band_name = section["spec"]["name"]
            for estimate in section["coupling"]:
                injected = truth[estimate["pair"]]
                expected = injected["band_gains"][band_name]
                relative_errors.append(
                    abs(estimate["coupling_magnitude"] - expected) / expected
                )
                expected_phase = injected["band_phases_deg"][band_name]
                phase_errors.append(
                    abs(
                        (estimate["phase_deg"] - expected_phase + 180.0) % 360.0
                        - 180.0
                    )
                )
                self.assertGreater(estimate["magnitude_uncertainty_db"], 0.0)
                self.assertGreater(estimate["phase_uncertainty_deg"], 0.0)
                self.assertLessEqual(
                    estimate["magnitude_interval"][0],
                    estimate["coupling_magnitude"],
                )
                self.assertGreaterEqual(
                    estimate["magnitude_interval"][1],
                    estimate["coupling_magnitude"],
                )
                count += 1
        self.assertEqual(count, 150)
        self.assertLess(max(relative_errors), 0.16)
        self.assertLess(max(phase_errors), 12.0)

    def test_phase_and_polarity_discrimination(self) -> None:
        phases = {item["pair"]: item for item in self.result["phase_continuity"]}
        self.assertEqual(phases["FL>FR"]["verdict"], "inverted")
        self.assertEqual(len(phases["FL>FR"]["inversion_bands"]), 5)
        self.assertEqual(phases["FL>FC"]["verdict"], "normal")
        self.assertTrue(phases["FL>FC"]["boundary_wraps"])
        self.assertFalse(phases["FL>FC"]["inversion_bands"])
        self.assertEqual(phases["FR>FL"]["code"], E_POLARITY_UNKNOWN)
        self.assertTrue(phases["FR>FL"]["phase_jumps"])
        self.assertEqual(phases["LFE>SL"]["code"], E_POLARITY_UNKNOWN)
        self.assertTrue(phases["LFE>SL"]["group_delay_discontinuities"])
        self.assertEqual(
            self.result["summary"]["inverted_pairs"],
            ["FL>FR"],
        )

    def test_routing_matrix_and_per_band_budget(self) -> None:
        self.assertEqual(len(self.result["bands"]), 5)
        for section in self.result["bands"]:
            layout = section["matrix"]["layout"]
            self.assertEqual(layout["sources"], ["FC", "FL", "FR", "LFE", "SL", "SR"])
            self.assertEqual(len(layout["magnitude"]), 6)
            self.assertEqual(len(layout["magnitude"][0]), 6)
            budget = section["residual_budget"]
            self.assertEqual(len(budget["pairs"]), 30)
            self.assertEqual(len(budget["pickups"]), 6)
            for row in budget["pairs"]:
                self.assertEqual(
                    row["met"], row["achieved_suppression_db"] + 1e-9 >= row["target_suppression_db"]
                )
                self.assertGreaterEqual(row["gap_db"], 0.0)
            for row in budget["pickups"]:
                self.assertIn("gap_db", row)
                self.assertIn("relative_suppression_db", row)
        shortfall_bands = self.result["summary"]["shortfall_bands"]
        self.assertTrue(shortfall_bands)
        self.assertTrue(
            all(
                section["residual_budget"]["shortfall_pairs"]
                or section["residual_budget"]["shortfall_pickups"]
                for section in self.result["bands"]
                if section["spec"]["name"] in shortfall_bands
            )
        )

    def test_calibration_constraints_and_infeasibility(self) -> None:
        calibrations = self.result["calibration"]
        for item in calibrations:
            config = load_config(self.root / "crossover.json")
            self.assertLessEqual(item["cancel_gain_linear"], config.max_cancel_gain + 1e-12)
            self.assertLessEqual(
                item["primary_suppression_db"],
                config.max_primary_suppression_db + 1e-9,
            )
            self.assertEqual(
                item["constraint_order"],
                [
                    "phase_polarity",
                    "cancel_gain_limit",
                    "primary_suppression_limit",
                    "target_suppression",
                ],
            )
        feasible_true_inversion = [
            item
            for item in calibrations
            if item["pair"] == "FL>FR"
        ]
        self.assertTrue(all(item["feasible"] for item in feasible_true_inversion))
        infeasible_orthogonal = [
            item for item in calibrations if item["pair"] == "SL>SR"
        ]
        self.assertTrue(all(not item["feasible"] for item in infeasible_orthogonal))
        self.assertTrue(
            all(item["error_code"] == E_CALIB_INFEASIBLE for item in infeasible_orthogonal)
        )


class IncrementalTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="venue-crosstalk-inc-")
        self.root = Path(self.temp.name) / "workspace"
        generate_sample_workspace(
            self.root,
            seed=20261122,
            duration_s=0.30,
            overwrite=True,
        )
        self.pipeline = AnalysisPipeline(self.root, reset_trace=True)
        self.baseline = self.pipeline.run(
            operation="baseline",
            reset_trace=True,
        )

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_unchanged_rerun_is_full_cache_hit(self) -> None:
        result = self.pipeline.run(operation="same-input")
        self.assertEqual(result["incremental"]["cache_misses"], 0)
        self.assertGreaterEqual(result["incremental"]["cache_hits"], 400)
        self.assertEqual(result["run_id"], self.baseline["run_id"])

    def test_target_change_only_recalibrates_and_rebudgets(self) -> None:
        target = float(self.baseline["effective_config"]["target_suppression_db"]) + 1.0
        result = self.pipeline.run(
            operation="target-change",
            target_suppression_db=target,
        )
        self.assertEqual(result["incremental"]["changed_bands"], [])
        recomputed = result["incremental"]["recomputed_nodes"]
        self.assertFalse(any(item["node"] == "band_coupling" for item in recomputed))
        self.assertFalse(any(item["node"] == "geometry_alignment" for item in recomputed))
        self.assertTrue(any(item["node"] == "calibration" for item in recomputed))
        self.assertEqual(
            sum(1 for item in recomputed if item["node"] == "calibration"),
            150,
        )
        self.assertEqual(
            sum(1 for item in recomputed if item["node"] == "residual_budget"),
            5,
        )
        self.assertFalse(result["estimation_changes"])

    def test_band_boundary_change_recomputes_only_affected_bands(self) -> None:
        target = float(self.baseline["effective_config"]["target_suppression_db"])
        after_target = self.pipeline.run(
            operation="target-before-band",
            target_suppression_db=target,
        )
        plan = _sorted_band_plan(_band_plan_with_changed_boundary(after_target, 25.0))
        result = self.pipeline.run(
            operation="band-change",
            target_suppression_db=target,
            bands=plan,
        )
        self.assertEqual(result["incremental"]["changed_bands"], ["low", "mid"])
        recomputed = result["incremental"]["recomputed_nodes"]
        coupling = [
            item for item in recomputed if item["node"] == "band_coupling"
        ]
        self.assertEqual(len(coupling), 60)
        self.assertTrue(
            all(
                item["scope"].split("|", 1)[1] in {"low", "mid"}
                for item in coupling
            )
        )
        matrix_scopes = {
            item["scope"]
            for item in recomputed
            if item["node"] == "routing_matrix"
        }
        budget_scopes = {
            item["scope"]
            for item in recomputed
            if item["node"] == "residual_budget"
        }
        self.assertEqual(matrix_scopes, {"low", "mid"})
        self.assertEqual(budget_scopes, {"low", "mid"})
        self.assertEqual(len(result["estimation_changes"]), 60)
        self.assertEqual(
            {item["band"] for item in result["estimation_changes"]},
            {"low", "mid"},
        )
        for change in result["estimation_changes"]:
            self.assertIn("magnitude_delta_db", change)
            self.assertIn("phase_delta_deg", change)
            self.assertIn("magnitude_uncertainty_change_db", change)
            self.assertIn("phase_uncertainty_change_deg", change)

    def test_measurement_content_change_reports_new_old_estimates(self) -> None:
        workspace = load_workspace(self.root)
        pair = workspace.pair_map()["FC>FL"]
        path = self.root / pair.wav_path
        with wave.open(str(path), "rb") as wav:
            params = wav.getparams()
            raw = wav.readframes(wav.getnframes())
        samples = np.frombuffer(raw, dtype="<i2").reshape(-1, params.nchannels).copy()
        column = pair.pickup_channel_index
        altered = samples[:, column].astype(np.int32) + 1
        samples[:, column] = np.clip(altered, -32768, 32767).astype("<i2")
        with wave.open(str(path), "wb") as wav:
            wav.setparams(params)
            wav.writeframes(samples.tobytes(order="C"))
        result = self.pipeline.run(operation="measurement-change")
        self.assertIn("FC>FL", result["incremental"]["changed_pairs"])
        self.assertIn(
            "measurement-content-hash-changed",
            [item["reason"] for item in result["incremental"]["reasons"]],
        )
        changed = [
            item for item in result["estimation_changes"] if item["pair"] == "FC>FL"
        ]
        self.assertEqual(len(changed), 5)
        for item in changed:
            self.assertIn("old_coupling_magnitude", item)
            self.assertIn("new_coupling_magnitude", item)
            self.assertIn("magnitude_uncertainty_change_db", item)


class ErrorTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="venue-crosstalk-err-")
        self.root = Path(self.temp.name) / "workspace"
        generate_sample_workspace(
            self.root,
            seed=9090,
            duration_s=0.20,
            overwrite=True,
        )

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_missing_geometry_entry_aborts_with_location(self) -> None:
        geometry = read_json(self.root / "geometry.json")
        del geometry["pickups"]["FR"]
        atomic_write_json(self.root / "geometry.json", geometry)
        pipeline = AnalysisPipeline(self.root, reset_trace=True)
        with self.assertRaises(VenueError) as raised:
            pipeline.run(operation="bad-geometry")
        self.assertEqual(raised.exception.code, E_GEOMETRY_INVALID)
        missing = raised.exception.details["missing"]
        self.assertEqual(missing, [{"kind": "pickup", "id": "FR"}])

    def test_negative_distance_override_is_rejected(self) -> None:
        manifest = read_json(self.root / "manifest.json")
        manifest["pairs"][0]["distance_m"] = -0.25
        atomic_write_json(self.root / "manifest.json", manifest)
        with self.assertRaises(VenueError) as raised:
            load_workspace(self.root)
        self.assertEqual(raised.exception.code, E_GEOMETRY_INVALID)
        self.assertIn("must not be negative", raised.exception.message)

    def test_preset_cancel_does_not_replace_last_publish(self) -> None:
        pipeline = AnalysisPipeline(self.root, reset_trace=True)
        baseline = pipeline.run(operation="before-cancel", reset_trace=True)
        snapshot_path = self.root / "publish" / "snapshot.json"
        before = snapshot_path.read_bytes()
        cancel_event = threading.Event()
        cancel_event.set()
        with self.assertRaises(VenueError) as raised:
            pipeline.run(
                operation="cancelled-run",
                cancel_event=cancel_event,
            )
        self.assertEqual(raised.exception.code, E_CANCELLED)
        self.assertEqual(snapshot_path.read_bytes(), before)
        self.assertEqual(read_json(snapshot_path)["run_id"], baseline["run_id"])

    def test_controller_returns_busy_for_duplicate_submit(self) -> None:
        controller = RecomputeController(self.root)
        first = controller.submit(operation="controller-first")
        self.assertTrue(first["ok"])
        duplicate = controller.submit(operation="controller-duplicate")
        self.assertFalse(duplicate["ok"])
        self.assertEqual(duplicate["code"], E_BUSY)
        events = controller.wait(timeout=30)
        self.assertIn("completed", [event.event for event in events])
        self.assertIsNotNone(controller.last_result)


class DeterminismTests(unittest.TestCase):
    def test_seed_and_operation_sequence_are_byte_identical(self) -> None:
        temp = tempfile.TemporaryDirectory(prefix="venue-crosstalk-det-")
        roots = [Path(temp.name) / "a", Path(temp.name) / "b"]
        try:
            for root in roots:
                generate_sample_workspace(
                    root,
                    seed=777,
                    duration_s=0.20,
                    overwrite=True,
                )
                pipeline = AnalysisPipeline(root, reset_trace=True)
                baseline = pipeline.run(
                    operation="baseline",
                    reset_trace=True,
                )
                target = float(
                    baseline["effective_config"]["target_suppression_db"]
                ) - 1.0
                pipeline.run(
                    operation="target-change",
                    target_suppression_db=target,
                )
                plan = _sorted_band_plan(
                    _band_plan_with_changed_boundary(baseline, 20.0)
                )
                pipeline.run(
                    operation="band-change",
                    target_suppression_db=target,
                    bands=plan,
                )
            trace_a = (roots[0] / "logs" / "trace.jsonl").read_bytes()
            trace_b = (roots[1] / "logs" / "trace.jsonl").read_bytes()
            snapshot_a = (roots[0] / "publish" / "snapshot.json").read_bytes()
            snapshot_b = (roots[1] / "publish" / "snapshot.json").read_bytes()
            self.assertEqual(trace_a, trace_b)
            self.assertEqual(snapshot_a, snapshot_b)
        finally:
            temp.cleanup()


class GuiSmokeTests(unittest.TestCase):
    def test_gui_renders_published_tables(self) -> None:
        try:
            os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
            from PyQt5.QtWidgets import QApplication

            from venue_crosstalk.gui import VenueWorkbench
        except ImportError:
            self.skipTest("PyQt5 is not installed")
        temp = tempfile.TemporaryDirectory(prefix="venue-crosstalk-gui-")
        root = Path(temp.name) / "workspace"
        try:
            generate_sample_workspace(
                root,
                seed=6060,
                duration_s=0.20,
                overwrite=True,
            )
            AnalysisPipeline(root, reset_trace=True).run(
                operation="gui-fixture",
                reset_trace=True,
            )
            app = QApplication.instance() or QApplication([])
            window = VenueWorkbench(root)
            self.assertEqual(window.band_selector.count(), 5)
            self.assertEqual(window.matrix_table.rowCount(), 6)
            self.assertEqual(window.phase_table.rowCount(), 30)
            self.assertGreater(window.budget_table.rowCount(), 0)
            window.close()
            self.assertIsNotNone(app)
        finally:
            temp.cleanup()


if __name__ == "__main__":
    unittest.main()
