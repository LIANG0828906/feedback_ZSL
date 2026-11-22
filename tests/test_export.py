from __future__ import annotations

import hashlib
import tempfile
import unittest
from pathlib import Path

from tests.helpers import sample_path
from vp_pipeline.diff import assert_frames_equal
from vp_pipeline.errors import E_VER, PipelineError
from vp_pipeline.export import (
    build_target_document,
    export_bundle,
    load_export_bundle,
)
from vp_pipeline.loader import load_scene
from vp_pipeline.renderer import render_all_frames


class ExportTests(unittest.TestCase):
    def test_all_target_versions_roundtrip_with_equal_frames(self) -> None:
        for source_version in (1, 2, 3):
            model = load_scene(sample_path(source_version))
            frames = render_all_frames(model)
            for target_version in (1, 2, 3):
                with self.subTest(source=source_version, target=target_version):
                    with tempfile.TemporaryDirectory() as directory:
                        output = Path(directory) / "bundle"
                        export_bundle(
                            model,
                            frames,
                            f"vpfc/{target_version}",
                            output,
                        )
                        readback, readback_frames, report = load_export_bundle(output)
                        self.assertEqual(
                            readback.source_version, f"vpfc/{target_version}"
                        )
                        self.assertEqual(
                            (readback.timeline.fps_num, readback.timeline.fps_den),
                            (model.timeline.fps_num, model.timeline.fps_den),
                        )
                        self.assertEqual(
                            [layer.layer_id for layer in readback.layers],
                            [layer.layer_id for layer in model.layers],
                        )
                        assert_frames_equal(readback_frames, frames, model)
                        self.assertIn(report["code"], {"OK", "E_LOSSY"})

    def test_repeated_export_is_byte_stable(self) -> None:
        model = load_scene(sample_path(3))
        frames = render_all_frames(model)
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "bundle"
            export_bundle(model, frames, "vpfc/1", output)
            first = {
                path.name: hashlib.sha256(path.read_bytes()).hexdigest()
                for path in sorted(output.iterdir())
            }
            export_bundle(model, frames, "vpfc/1", output)
            second = {
                path.name: hashlib.sha256(path.read_bytes()).hexdigest()
                for path in sorted(output.iterdir())
            }
        self.assertEqual(first, second)

    def test_low_version_reports_lossy_mapping_without_reordering(self) -> None:
        model = load_scene(sample_path(3))
        document, warnings, trace = build_target_document(model, "vpfc/1")
        self.assertTrue(warnings)
        self.assertTrue(all(item["code"] == "E_LOSSY" for item in warnings))
        self.assertTrue(trace)
        self.assertTrue(
            any(item["source_field"] == "parent_id" for item in trace)
        )
        self.assertEqual(
            [
                item["id"]
                for item in sorted(document["layers"], key=lambda item: item["order"])
            ],
            [layer.layer_id for layer in model.layers],
        )
        self.assertEqual(
            document["timeline"]["time_base"]["fps"],
            {"num": model.timeline.fps_num, "den": model.timeline.fps_den},
        )

    def test_unsupported_target_returns_e_ver(self) -> None:
        model = load_scene(sample_path(1))
        with self.assertRaises(PipelineError) as caught:
            build_target_document(model, "vpfc/99")
        self.assertEqual(caught.exception.code, E_VER)
        self.assertEqual(caught.exception.field_path, "target_version")


if __name__ == "__main__":
    unittest.main()
