from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from mediaforge.package import build_package
from mediaforge.truth import compute_truth_fingerprint
from mediaforge.validator import validate_artifact

from .support import SAMPLES, loaded, patch_package_header, read_json, write_json


class ValidatorTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.model = loaded("g01_neon_city", "v3").model
        cls.truth = SAMPLES / "g01_neon_city" / "truth.json"

    def _artifact(self, target: str = "v3") -> bytes:
        return build_package(self.model, target).artifact

    def test_all_target_versions_validate(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mf-validate-") as temp:
            for target in ("v1", "v2", "v3"):
                path = Path(temp) / f"{target}.mfz"
                path.write_bytes(self._artifact(target))
                result = validate_artifact(path, self.truth)
                self.assertTrue(result.ok, result.error)
                self.assertGreater(len(result.checks), 5)

    def test_digest_corruption_reports_first_mismatch(self) -> None:
        artifact = bytearray(self._artifact("v3"))
        artifact[-40] ^= 0x40
        with tempfile.TemporaryDirectory(prefix="mf-corrupt-") as temp:
            path = Path(temp) / "corrupt.mfz"
            path.write_bytes(bytes(artifact))
            result = validate_artifact(path, self.truth)
        self.assertFalse(result.ok)
        self.assertEqual(result.error["code"], "E_CORRUPT")
        self.assertEqual(result.error["field_path"], "$.integrity.sha256")
        self.assertIn("measured", result.error)
        self.assertIn("declared", result.error)

    def test_declaration_sample_count_mismatch_is_detected(self) -> None:
        artifact = patch_package_header(
            self._artifact("v3"),
            lambda header: next(
                track
                for track in header["declaration"]["tracks"]
                if track["track_id"] == "video_main"
            ).__setitem__("sample_count", 91),
        )
        with tempfile.TemporaryDirectory(prefix="mf-decl-") as temp:
            path = Path(temp) / "decl.mfz"
            path.write_bytes(artifact)
            result = validate_artifact(path, self.truth)
        self.assertFalse(result.ok)
        self.assertEqual(result.error["field_path"], "$.tracks[1].sample_count")
        self.assertEqual(result.error["measured"], 90)
        self.assertEqual(result.error["declared"], 91)

    def test_chapter_declaration_mismatch_is_detected(self) -> None:
        artifact = patch_package_header(
            self._artifact("v3"),
            lambda header: header["declaration"]["chapters"][1].__setitem__("start_ticks", 45001),
        )
        with tempfile.TemporaryDirectory(prefix="mf-chapter-") as temp:
            path = Path(temp) / "chapter.mfz"
            path.write_bytes(artifact)
            result = validate_artifact(path, self.truth)
        self.assertFalse(result.ok)
        self.assertEqual(result.error["field_path"], "$.chapters[1].start_ticks")

    def test_truth_tag_mismatch_is_detected_after_fingerprint_recompute(self) -> None:
        truth = read_json(self.truth)
        truth["tags"]["studio"] = "Tampered Studio"
        truth["fingerprint"] = compute_truth_fingerprint(truth)
        with tempfile.TemporaryDirectory(prefix="mf-truth-") as temp:
            truth_path = Path(temp) / "truth.json"
            artifact_path = Path(temp) / "artifact.mfz"
            write_json(truth_path, truth)
            artifact_path.write_bytes(self._artifact("v3"))
            result = validate_artifact(artifact_path, truth_path)
        self.assertFalse(result.ok)
        self.assertEqual(result.error["field_path"], "$.tags.studio")

    def test_validator_does_not_need_in_memory_model(self) -> None:
        artifact = self._artifact("v3")
        with tempfile.TemporaryDirectory(prefix="mf-independent-") as temp:
            artifact_path = Path(temp) / "artifact.mfz"
            artifact_path.write_bytes(artifact)
            result = validate_artifact(artifact_path, self.truth)
        self.assertTrue(result.ok)
        serialized = json.dumps(result.to_dict(), sort_keys=True)
        self.assertIn("artifact_sha256", serialized)


if __name__ == "__main__":
    unittest.main()
