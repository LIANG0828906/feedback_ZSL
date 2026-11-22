from __future__ import annotations

import json
import unittest

from npr_workbench.errors import DecodeError, ParameterError
from npr_workbench.model import RenderParams
from npr_workbench.scene import load_scene, validate_scene_data

from tests.helpers import ROOT, sample_scene


class ParameterAndSceneTests(unittest.TestCase):
    def test_parameter_boundaries_report_field(self) -> None:
        cases = [
            ({"levels": 1}, "levels"),
            ({"levels": True}, "levels"),
            ({"density": -0.01}, "density"),
            ({"density": 1.01}, "density"),
            ({"length": 0.5}, "length"),
            ({"length": 65.0}, "length"),
            ({"preview_width": 360}, "preview_width"),
        ]
        base = RenderParams(
            preview_width=80,
            preview_height=60,
            exact_width=240,
            exact_height=180,
            tile_width=60,
            tile_height=60,
            thread_pixel_budget=8192,
            stroke_cache_budget=128,
            guide_width=64,
            guide_height=48,
        ).to_dict()
        for override, field in cases:
            payload = {**base, **override}
            with self.subTest(field=field, value=override):
                with self.assertRaises(ParameterError) as caught:
                    RenderParams(**payload)
                self.assertEqual(caught.exception.code, "E_PARA")
                self.assertEqual(caught.exception.details["field"], field)

    def test_scene_hash_is_stable_across_disk_round_trip(self) -> None:
        scene = sample_scene()
        reloaded = validate_scene_data(json.loads(json.dumps(scene.to_dict())))
        self.assertEqual(scene.content_hash(), reloaded.content_hash())

    def test_corrupt_scene_returns_decode(self) -> None:
        path = ROOT / "data" / "cases" / "corrupt_scene.json"
        with self.assertRaises(DecodeError) as caught:
            load_scene(path)
        self.assertEqual(caught.exception.code, "E_DECODE")

    def test_invalid_geometry_returns_decode(self) -> None:
        path = ROOT / "data" / "cases" / "invalid_scene_schema.json"
        with self.assertRaises(DecodeError) as caught:
            load_scene(path)
        self.assertEqual(caught.exception.code, "E_DECODE")
        self.assertIn("kind", json.dumps(caught.exception.details))

    def test_local_invalid_parameter_fixture_returns_field_error(self) -> None:
        payload = json.loads(
            (ROOT / "data" / "cases" / "invalid_params.json").read_text(
                encoding="utf-8"
            )
        )
        with self.assertRaises(ParameterError) as caught:
            RenderParams.from_dict(payload)
        self.assertEqual(caught.exception.code, "E_PARA")
        self.assertEqual(caught.exception.details["field"], "levels")


if __name__ == "__main__":
    unittest.main()

