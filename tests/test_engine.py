from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import numpy as np

from acoustic_workbench.alignment import align_impulse_responses
from acoustic_workbench.cancellation import CancellationToken
from acoustic_workbench.errors import AcousticError
from acoustic_workbench.model import Scene, apply_factor, load_materials
from acoustic_workbench.samplegen import VENUES, generate_venue
from acoustic_workbench.service import AcousticEngine


class EngineTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temporary = tempfile.TemporaryDirectory(prefix="acoustic-tests-")
        cls.root = Path(cls.temporary.name)
        generate_venue(
            cls.root,
            "club",
            VENUES["club"],
            build_baseline=True,
        )
        cls.venue_dir = cls.root / "venue_club"
        cls.scene = Scene.load(cls.venue_dir / "scene.json")
        cls.materials = load_materials(
            cls.venue_dir / cls.scene.materials_file
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temporary.cleanup()

    def _engine(
        self,
        cache_name: str,
        scene: Scene | None = None,
    ) -> AcousticEngine:
        return AcousticEngine(
            self.venue_dir,
            cache_dir=self.root / cache_name,
            scene=scene,
        )

    def test_incremental_material_and_full_rebuild_match(self) -> None:
        engine = self._engine("cache-incremental", self.scene)
        baseline = engine.rebuild(self.scene, mode="baseline")
        self.assertTrue(baseline.to_dict()["recomputed_nodes"])

        change = {
            "factor": "material",
            "wall_id": "east",
            "material": "acoustic_panel",
        }
        updated, factor, _ = apply_factor(self.scene, self.materials, change)
        self.assertEqual(factor, "material")
        report = engine.rebuild(
            updated,
            changes=[change],
            previous_scene=self.scene,
            mode="incremental",
        )
        payload = report.to_dict()
        self.assertIn("direct_align", payload["cache_hit_nodes"])
        self.assertIn("early_reflections", payload["recomputed_nodes"])
        self.assertNotIn("direct_align", payload["recomputed_nodes"])

        full_engine = self._engine("cache-full", updated)
        full_engine.rebuild(updated, mode="full")
        for listener in updated.listeners:
            _, incremental_coeffs = engine.validate_field(updated, listener.id)
            _, full_coeffs = full_engine.validate_field(updated, listener.id)
            np.testing.assert_array_equal(incremental_coeffs, full_coeffs)

        restarted = self._engine("cache-incremental", updated)
        reuse = restarted.rebuild(updated, mode="restart")
        self.assertEqual(reuse.to_dict()["recomputed_nodes"], [])
        self.assertEqual(
            set(reuse.to_dict()["cache_hit_nodes"]),
            {
                "direct_align",
                "early_reflections",
                "band_decay",
                "late_reverb",
                "multichannel_stitch",
                "field_coefficients",
            },
        )

    def test_earliest_affected_node_is_recomputed_for_source_move(self) -> None:
        engine = self._engine("cache-source", self.scene)
        engine.rebuild(self.scene, mode="baseline")
        change = {
            "factor": "source_position",
            "position": [2.7, 4.3, 1.5],
        }
        updated, _, _ = apply_factor(self.scene, self.materials, change)
        report = engine.rebuild(
            updated,
            changes=[change],
            previous_scene=self.scene,
        )
        for point in report.points:
            self.assertEqual(point.invalidated_nodes[0], "direct_align")
            self.assertEqual(point.nodes[0].status, "recomputed")
            self.assertTrue(point.affected_reflection_events)

    def test_stale_field_render_is_rejected(self) -> None:
        engine = self._engine("cache-stale", self.scene)
        engine.rebuild(self.scene, mode="baseline")
        change = {
            "factor": "source_position",
            "position": [3.1, 4.2, 1.4],
        }
        updated, _, _ = apply_factor(self.scene, self.materials, change)
        with self.assertRaises(AcousticError) as caught:
            engine.render_point(updated, "L1")
        self.assertEqual(caught.exception.code, "E_FIELD_STALE")

    def test_old_render_is_not_latest_after_field_update(self) -> None:
        engine = self._engine("cache-render-version", self.scene)
        engine.rebuild(self.scene, mode="baseline")
        first_render = engine.render_point(self.scene, "L1")
        change = {
            "factor": "source_position",
            "position": [3.3, 4.4, 1.5],
        }
        updated, _, _ = apply_factor(self.scene, self.materials, change)
        engine.rebuild(
            updated,
            changes=[change],
            previous_scene=self.scene,
        )
        self.assertIsNone(engine.latest_render("L1"))
        second_render = engine.render_point(updated, "L1")
        self.assertNotEqual(first_render["field_version"], second_render["field_version"])
        self.assertIsNotNone(engine.latest_render("L1"))

    def test_same_point_task_lock_returns_busy(self) -> None:
        engine = self._engine("cache-busy", self.scene)
        engine.rebuild(self.scene, mode="baseline")
        with engine.locks.acquire("L1"):
            with self.assertRaises(AcousticError) as caught:
                engine.rebuild(self.scene, mode="busy")
        self.assertEqual(caught.exception.code, "E_BUSY")

    def test_align_failure_does_not_create_field(self) -> None:
        broken = np.zeros((6, 2048), dtype=np.float64)
        broken[1, 100] = 0.5
        with self.assertRaises(AcousticError) as caught:
            align_impulse_responses(broken, self.scene, "L1")
        self.assertEqual(caught.exception.code, "E_ALIGN_FAIL")

    def test_invalid_band_config_returns_band_invalid(self) -> None:
        scene = self.scene.clone()
        scene.early["end_ms"] = 5000.0
        engine = self._engine("cache-band", scene)
        with self.assertRaises(AcousticError) as caught:
            engine.rebuild(scene, mode="invalid-band")
        self.assertEqual(caught.exception.code, "E_BAND_INVALID")

    def test_cancellation_retains_previous_committed_cache(self) -> None:
        engine = self._engine("cache-cancel", self.scene)
        baseline = engine.rebuild(self.scene, mode="baseline")
        previous_heads = engine.cache.read_heads()
        token = CancellationToken()
        token.cancel()
        with self.assertRaises(AcousticError) as caught:
            engine.rebuild(self.scene, mode="cancelled", token=token)
        self.assertEqual(caught.exception.code, "E_CANCELLED")
        self.assertEqual(engine.cache.read_heads(), previous_heads)
        self.assertTrue(baseline.points)


if __name__ == "__main__":
    unittest.main()

