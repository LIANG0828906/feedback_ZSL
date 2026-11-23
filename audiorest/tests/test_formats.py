"""要求 1/2/4：多版本加载、转换痕迹、降级损失、跨版本等价性。"""

from __future__ import annotations

import unittest
from pathlib import Path

from audiorest.errors import WorkbenchError
from audiorest.formats.loader import (
    compare_projects,
    export_project,
    load_project,
)
from audiorest.model import StageKind

SAMPLES = Path(__file__).resolve().parents[1] / "samples"


class FormatTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        if not (SAMPLES / "manifest.json").exists():
            from audiorest.generate_samples import generate

            generate(SAMPLES)

    def _project_dir(self, name: str) -> Path:
        return SAMPLES / name

    def test_three_versions_load_with_source_marker(self) -> None:
        for project_dir in SAMPLES.glob("voice_*"):
            docs = sorted(project_dir.glob("*.v*.json"))
            self.assertEqual(len(docs), 3)
            versions = []
            for doc in docs:
                project = load_project(doc)
                versions.append(project.source_version)
                self.assertEqual(len(project.stages), 3)
            self.assertEqual(versions, ["1.0", "2.0", "3.0"])

    def test_cross_version_equivalence_all_projects(self) -> None:
        import json

        manifest = json.loads((SAMPLES / "manifest.json").read_text(encoding="utf-8"))
        for item in manifest["projects"]:
            pdir = SAMPLES / item["project_id"]
            projects = [
                load_project(pdir / item["documents"][version])
                for version in ("1.0", "2.0", "3.0")
            ]
            for index in range(1, 3):
                report = compare_projects(
                    projects[0],
                    projects[index],
                    left_audio_root=pdir,
                    right_audio_root=pdir,
                )
                self.assertTrue(report.equivalent, report.first_diff)
                self.assertEqual(report.audio_hash_left, report.audio_hash_right)

    def test_xver_locates_first_diff(self) -> None:
        import json

        manifest = json.loads((SAMPLES / "manifest.json").read_text(encoding="utf-8"))
        item = manifest["projects"][0]
        pdir = SAMPLES / item["project_id"]
        left = load_project(pdir / item["documents"]["3.0"])
        right = load_project(pdir / item["documents"]["2.0"])
        tampered = right.with_runtime_params(gate_threshold_db=-60.0)
        report = compare_projects(
            left, tampered, left_audio_root=pdir, right_audio_root=pdir
        )
        self.assertFalse(report.equivalent)
        self.assertIn("threshold_db", report.first_diff["path"])
        with self.assertRaises(WorkbenchError) as ctx:
            from audiorest.formats.loader import assert_equivalent

            assert_equivalent(
                left, tampered, left_audio_root=pdir, right_audio_root=pdir
            )
        self.assertEqual(ctx.exception.code, "E_XVER")

    def test_upgrade_trace_records_field_old_and_default(self) -> None:
        import json

        manifest = json.loads((SAMPLES / "manifest.json").read_text(encoding="utf-8"))
        item = manifest["projects"][0]
        pdir = SAMPLES / item["project_id"]
        v1_project = load_project(pdir / item["documents"]["1.0"])
        paths = {entry["field_path"] for entry in v1_project.conversion_trace}
        self.assertIn("framing.fft_size", paths)
        self.assertIn("stages[mask_smoothing].config.smoothing", paths)
        for entry in v1_project.conversion_trace:
            self.assertIn("old_value", entry)
            self.assertIn("default_value", entry)
            self.assertIn("default_source", entry)

    def test_downgrade_lossy_keeps_stages(self) -> None:
        import json

        manifest = json.loads((SAMPLES / "manifest.json").read_text(encoding="utf-8"))
        item = manifest["projects"][0]
        project = load_project(SAMPLES / item["project_id"] / item["documents"]["3.0"])
        with self.assertRaises(WorkbenchError) as ctx:
            export_project(project, "1.0")
        self.assertEqual(ctx.exception.code, "E_LOSSY")
        losses = ctx.exception.details["losses"]
        # 三个修复阶段仍然存在于损失清单语义中，没有被静默丢弃
        affected = {loss["field_path"] for loss in losses}
        self.assertTrue(any("mask_smoothing" in path for path in affected))
        self.assertTrue(any("noise_gate" in path for path in affected))

    def test_corrupt_projects_return_e_ver_with_path(self) -> None:
        cases = [
            ("missing_field.v2.json", "audioRef"),
            ("undecodable.json", None),
            ("unknown_version.json", "format_version"),
        ]
        for filename, expected_path in cases:
            with self.assertRaises(WorkbenchError) as ctx:
                load_project(SAMPLES / "corrupt" / filename)
            self.assertEqual(ctx.exception.code, "E_VER")
            if expected_path:
                self.assertEqual(ctx.exception.path, expected_path)

    def test_roundtrip_v3_preserves_stage_config(self) -> None:
        import json

        manifest = json.loads((SAMPLES / "manifest.json").read_text(encoding="utf-8"))
        item = manifest["projects"][0]
        project = load_project(SAMPLES / item["project_id"] / item["documents"]["3.0"])
        doc = export_project(project, "3.0")
        self.assertEqual(len(doc["stages"]), 3)
        self.assertEqual(
            doc["stages"][0]["kind"], StageKind.SPECTRAL_SUBTRACTION.value
        )


if __name__ == "__main__":
    unittest.main()
