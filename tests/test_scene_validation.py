from __future__ import annotations

import copy
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from gallery_louvers.errors import ErrorCode, SceneValidationError
from gallery_louvers.scene import validate_scene
from gallery_louvers.scene_builder import build_scene


class SceneValidationTests(unittest.TestCase):
    def assert_issue(self, raw: dict, code: str, layer: int | None = None) -> None:
        with self.assertRaises(SceneValidationError) as caught:
            validate_scene(raw)
        issues = caught.exception.as_dicts()
        self.assertTrue(any(issue["code"] == code for issue in issues), issues)
        if layer is not None:
            self.assertTrue(
                any(
                    issue["code"] == code and issue["layer"] == layer
                    for issue in issues
                ),
                issues,
            )

    def test_mesh_index_out_of_range(self) -> None:
        raw = copy.deepcopy(build_scene())
        raw["leaves"][0]["triangles"][0][0] = 999
        self.assert_issue(raw, ErrorCode.MESH_INDEX_OUT_OF_RANGE, 0)

    def test_degenerate_triangle(self) -> None:
        raw = copy.deepcopy(build_scene())
        raw["leaves"][0]["triangles"][0] = [0, 0, 0]
        self.assert_issue(raw, ErrorCode.DEGENERATE_TRIANGLE, 0)

    def test_thickness_bounds(self) -> None:
        raw = copy.deepcopy(build_scene())
        raw["leaves"][3]["thickness"] = 0.5
        self.assert_issue(raw, ErrorCode.THICKNESS_OUT_OF_RANGE, 0)

    def test_duplicate_leaf_id(self) -> None:
        raw = copy.deepcopy(build_scene())
        raw["leaves"][1]["leaf"] = 0
        self.assert_issue(raw, ErrorCode.LEAF_ID_MISSING_OR_DUPLICATE, 0)

    def test_missing_leaf_id(self) -> None:
        raw = copy.deepcopy(build_scene())
        del raw["leaves"][2]["leaf"]
        self.assert_issue(raw, ErrorCode.LEAF_ID_MISSING_OR_DUPLICATE, 0)

    def test_duplicate_layer_id(self) -> None:
        raw = copy.deepcopy(build_scene())
        raw["layers"][2]["layer"] = 1
        self.assert_issue(raw, ErrorCode.LAYER_ID_MISSING_OR_DUPLICATE, 1)

    def test_zero_brush_direction(self) -> None:
        raw = copy.deepcopy(build_scene())
        raw["leaves"][2]["brush_direction"] = [0.0, 0.0, 0.0]
        self.assert_issue(raw, ErrorCode.ZERO_BRUSH_DIRECTION, 0)

    def test_cli_exit_code_and_structured_warning(self) -> None:
        raw = copy.deepcopy(build_scene())
        raw["leaves"][0]["brush_direction"] = [0.0, 0.0, 0.0]
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "bad.json"
            path.write_text(json.dumps(raw), encoding="utf-8")
            result = subprocess.run(
                [sys.executable, "-m", "gallery_louvers", "validate", str(path)],
                cwd=Path(__file__).resolve().parents[1],
                capture_output=True,
                text=True,
                check=False,
            )
        self.assertNotEqual(result.returncode, 0)
        records = [
            json.loads(line)
            for line in result.stderr.splitlines()
            if line.strip()
        ]
        self.assertTrue(
            any(
                record["code"] == ErrorCode.ZERO_BRUSH_DIRECTION
                and record["layer"] == 0
                for record in records
            ),
            records,
        )


if __name__ == "__main__":
    unittest.main()
