from __future__ import annotations

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import numpy as np

from acoustic_workbench.codes import AcousticError
from acoustic_workbench.dsp import convolve_same
from acoustic_workbench.engine import Workbench
from acoustic_workbench.samplegen import generate_samples, mutate_scene
from acoustic_workbench.util import read_json, sha256_file
from acoustic_workbench.wavio import read_wav_16


class WorkbenchTestCase(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        generate_samples(
            self.root,
            seed=777,
            venue_names=("small",),
            build_baselines=False,
        )
        self.scene_dir = self.root / "scenes" / "small"
        self.scene_path = self.scene_dir / "scene.json"
        self.workbench = Workbench(self.scene_dir, scene_path="scene.json")

    def tearDown(self) -> None:
        self.temp.cleanup()

    def _rebuild(self, listener: str = "L1", **kwargs):
        return self.workbench.rebuild(listener, **kwargs)

    def test_reference_change_recomputes_alignment_and_reports_delta(self) -> None:
        first = self._rebuild()
        mutate_scene(self.scene_path, {"listeners.0.reference_channel": "FL"})
        second = self._rebuild()
        trajectory = second["trajectory"]
        self.assertEqual(trajectory["earliest_affected_node"], "direct_alignment")
        self.assertIn("direct_alignment", trajectory["recomputed_nodes"])
        self.assertNotIn("ir_load", trajectory["recomputed_nodes"])
        differences = trajectory["arrival_sample_differences"]
        self.assertIsNotNone(differences)
        self.assertTrue(any(item["aligned_delta_samples"] for item in differences))
        self.assertNotEqual(first["field_version"], second["field_version"])

    def test_material_change_reuses_upstream_nodes(self) -> None:
        first = self._rebuild()
        mutate_scene(self.scene_path, {"venue.surfaces.wall": "heavy_curtain"})
        second = self._rebuild()
        trajectory = second["trajectory"]
        self.assertEqual(trajectory["factors_changed"], ["material"])
        self.assertEqual(trajectory["earliest_affected_node"], "decay_model")
        self.assertEqual(
            trajectory["recomputed_nodes"],
            [
                "decay_model",
                "late_reverb",
                "spatial_synthesis",
                "field_coefficients",
            ],
        )
        self.assertEqual(
            trajectory["cache_hits"],
            ["ir_load", "direct_alignment", "early_reflections"],
        )
        self.assertNotEqual(first["field_version"], second["field_version"])

    def test_source_position_recomputes_from_early_reflections(self) -> None:
        self._rebuild()
        mutate_scene(self.scene_path, {"source.position.0": 2.0})
        result = self._rebuild()
        trajectory = result["trajectory"]
        self.assertEqual(trajectory["factors_changed"], ["source_position"])
        self.assertEqual(
            trajectory["earliest_affected_node"], "early_reflections"
        )
        self.assertEqual(
            trajectory["cache_hits"], ["ir_load", "direct_alignment"]
        )
        self.assertEqual(
            trajectory["recomputed_nodes"][0], "early_reflections"
        )
        self.assertIsNotNone(trajectory["reflection_event_impact"])

    def test_frequency_change_marks_affected_reflection_events(self) -> None:
        self._rebuild()
        mutate_scene(
            self.scene_path,
            {"frequency_config.bands_hz": [63.0, 125.0, 250.0, 500.0, 1000.0, 2000.0, 4000.0, 7000.0]},
        )
        result = self._rebuild()
        impact = result["trajectory"]["reflection_event_impact"]
        self.assertIsNotNone(impact)
        self.assertTrue(impact["recomputed"])
        self.assertGreater(len(impact["affected_event_indices"]), 0)
        self.assertIn("frequency_config", impact["reason"])

    def test_stale_render_is_rejected_before_output(self) -> None:
        self._rebuild()
        mutate_scene(self.scene_path, {"source.position.0": 2.1})
        output = self.workbench.renders / "stale.wav"
        with self.assertRaises(AcousticError) as caught:
            self.workbench.render("L1", output="renders/stale.wav")
        self.assertEqual(caught.exception.code, "E_FIELD_STALE")
        self.assertFalse(output.exists())
        self._rebuild()
        rendered = self.workbench.render("L1", output="renders/L1.wav")
        self.assertTrue(Path(rendered["output"]).exists())
        wav = read_wav_16(rendered["output"])
        self.assertEqual(wav.channel_count, 6)
        self.assertEqual(wav.sample_rate, 48000)
        self.assertEqual(wav.channel_mask, 0x3F)

    def test_injection_codes_are_distinct_and_do_not_publish(self) -> None:
        self._rebuild()
        field_path = self.workbench.cache.published_path("L1")
        before = field_path.read_bytes()
        expected = {
            "ir_invalid": "E_IR_INVALID",
            "decay_negative": "E_DECAY_NEGATIVE",
            "position_out_of_bounds": "E_POSITION_OUT_OF_BOUNDS",
            "channel_layout_missing": "E_CHANNEL_LAYOUT_MISMATCH",
        }
        for injection, code in expected.items():
            with self.subTest(injection=injection):
                with self.assertRaises(AcousticError) as caught:
                    self.workbench.rebuild("L1", injections=[injection])
                error = caught.exception
                self.assertEqual(error.code, code)
                self.assertTrue(
                    str(error.injection_point).startswith("input:"),
                    error.to_dict(),
                )
                self.assertEqual(field_path.read_bytes(), before)
        self.assertEqual(list(self.workbench.renders.glob("*.wav")), [])

    def test_busy_guard_is_listener_scoped(self) -> None:
        self._rebuild()
        with self.workbench.tasks.acquire("L1", "holder"):
            with self.assertRaises(AcousticError) as caught:
                self.workbench.rebuild("L1")
            self.assertEqual(caught.exception.code, "E_BUSY")
            with self.workbench.tasks.acquire("L2", "holder"):
                pass

    def test_cancellation_preserves_last_commit(self) -> None:
        self._rebuild()
        field_path = self.workbench.cache.published_path("L1")
        before = field_path.read_bytes()
        mutate_scene(self.scene_path, {"window_params.early_window_ms": 101.0})
        with self.assertRaises(AcousticError) as caught:
            self.workbench.rebuild(
                "L1",
                cancel_after_stage="early_reflections",
            )
        self.assertEqual(caught.exception.code, "E_CANCELLED")
        self.assertEqual(field_path.read_bytes(), before)

    def test_channel_failure_writes_no_partial_wav(self) -> None:
        self._rebuild()
        output = self.workbench.renders / "atomic.wav"
        calls = {"count": 0}

        def failing_convolve(signal, impulse):
            calls["count"] += 1
            if calls["count"] == 4:
                raise RuntimeError("injected channel failure")
            return convolve_same(signal, impulse)

        with mock.patch("acoustic_workbench.dsp.convolve_same", side_effect=failing_convolve):
            with self.assertRaises(AcousticError) as caught:
                self.workbench.render("L1", output="renders/atomic.wav")
        self.assertEqual(caught.exception.code, "E_SYNTHESIS_FAILED")
        self.assertFalse(output.exists())

    def test_byte_deterministic_workspace_replay(self) -> None:
        def run_once(label: str):
            base = self.root / label
            generate_samples(
                base,
                seed=777,
                venue_names=("small",),
                build_baselines=False,
            )
            scene_dir = base / "scenes" / "small"
            scene_path = scene_dir / "scene.json"
            bench = Workbench(scene_dir, scene_path="scene.json")
            bench.rebuild("L1")
            mutate_scene(scene_path, {"venue.surfaces.wall": "heavy_curtain"})
            bench.rebuild("L1")
            rendered = bench.render("L1", output="renders/L1.wav")
            return {
                "log": sha256_file(scene_dir / "logs" / "trajectory.jsonl"),
                "field": sha256_file(bench.cache.published_path("L1")),
                "render": rendered["output_sha256"],
            }

        first = run_once("det_a")
        second = run_once("det_b")
        self.assertEqual(first, second)

    def test_direct_arrival_truth_uses_original_wav_sample_hash(self) -> None:
        result = self._rebuild()
        fingerprints = result["field"]["dependency_manifest"]
        alignment = self.workbench.cache.get_node(
            "direct_alignment", fingerprints["direct_alignment"]
        )
        self.assertIsNotNone(alignment)
        ir_path = self.scene_dir / "irs" / "L1.wav"
        self.assertEqual(
            alignment.summary["truth_source"]["wav_sha256"],
            sha256_file(ir_path),
        )
        self.assertTrue(
            all(
                item["aligned_sample_index"]
                == alignment.summary["reference_sample_index"]
                for item in alignment.summary["channels"]
            )
        )
        wav = read_wav_16(ir_path)
        self.assertEqual(wav.channel_count, 6)


if __name__ == "__main__":
    unittest.main()

