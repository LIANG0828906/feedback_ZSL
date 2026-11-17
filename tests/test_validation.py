import copy
import unittest
from pathlib import Path

import numpy as np

from space_audio.demo import build_demo_scene
from space_audio.errors import (
    DRY_FILE_MISSING,
    DUPLICATE_SOURCE_ID,
    PATH_TIME_ORDER,
    SAMPLE_RATE_MISMATCH,
    TRANSMISSION_RANGE,
)
from space_audio.scene import validate_scene
from space_audio.synthesis import generate_all_dry_sources


class ValidationTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(__file__).parent / "_tmp_validation"
        import shutil

        shutil.rmtree(self.tmp, ignore_errors=True)
        self.tmp.mkdir(parents=True, exist_ok=True)
        self.scene = build_demo_scene()
        generate_all_dry_sources(self.scene, self.tmp)

    def test_valid_scene_passes(self) -> None:
        errors = validate_scene(self.scene, base_dir=self.tmp)
        self.assertEqual(errors, [])

    def test_duplicate_source_id(self) -> None:
        broken = copy.deepcopy(self.scene)
        broken["sources"][1]["id"] = broken["sources"][0]["id"]
        codes = {error.code for error in validate_scene(broken)}
        self.assertIn(DUPLICATE_SOURCE_ID, codes)

    def test_path_time_order(self) -> None:
        broken = copy.deepcopy(self.scene)
        broken["path"][5]["t"] = -1.0
        codes = {error.code for error in validate_scene(broken)}
        self.assertIn(PATH_TIME_ORDER, codes)

    def test_transmission_range(self) -> None:
        broken = copy.deepcopy(self.scene)
        broken["layers"][0]["transmission"] = 1.5
        codes = {error.code for error in validate_scene(broken)}
        self.assertIn(TRANSMISSION_RANGE, codes)

    def test_missing_dry_file(self) -> None:
        broken = copy.deepcopy(self.scene)
        broken["sources"][0]["file"] = "dry/nope.wav"
        codes = {error.code for error in validate_scene(broken, base_dir=self.tmp)}
        self.assertIn(DRY_FILE_MISSING, codes)

    def test_sample_rate_mismatch(self) -> None:
        from space_audio.wavio import write_wav

        broken = copy.deepcopy(self.scene)
        write_wav(self.tmp / broken["sources"][0]["file"], np.zeros(160, dtype=np.float32), 16000)
        codes = {error.code for error in validate_scene(broken, base_dir=self.tmp)}
        self.assertIn(SAMPLE_RATE_MISMATCH, codes)

    def test_errors_carry_field_paths(self) -> None:
        broken = copy.deepcopy(self.scene)
        broken["layers"][2]["transmission"] = -0.1
        errors = validate_scene(broken)
        self.assertTrue(any("$.layers[2].transmission" == error.field_path for error in errors))


if __name__ == "__main__":
    unittest.main()

