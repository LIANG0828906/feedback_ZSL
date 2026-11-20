from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from glyph_atlas.atlas import build_atlas, combined_coverage_hash, verify_output_hashes
from glyph_atlas.config import AtlasConfig
from glyph_atlas.errors import E_ATLAS_OVERFLOW, E_SNAPSHOT_MISMATCH, GlyphAtlasError
from glyph_atlas.generator import generate_samples
from glyph_atlas.pipeline import run_pipeline
from glyph_atlas.snapshot import canonical_json_bytes
from tests.helpers import simple_config, square_hole_font, write_json


class AtlasTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        write_json(self.root / "font.json", square_hole_font())
        self.config_path = simple_config(self.root, glyphs=["hole"])
        self.report = run_pipeline(self.config_path)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_output_hashes_and_snapshot_outputs(self) -> None:
        run = self.report.targets[0]
        manifest = verify_output_hashes(run.atlas.output_dir)
        expected = combined_coverage_hash(run.schedule.glyphs)
        self.assertEqual(manifest["source_coverage_sha256"], expected)
        self.assertEqual(run.atlas.manifest["atlas_sha256"], manifest["atlas_sha256"])
        for glyph in run.schedule.glyphs:
            state = glyph.snapshot_store.load()
            self.assertIsNotNone(state.outputs)
            self.assertEqual(state.outputs["glyph_coverage_sha256"], glyph.coverage_hash)

    def test_atlas_overflow_lists_unplaced_glyphs(self) -> None:
        run = self.report.targets[0]
        with self.assertRaises(GlyphAtlasError) as caught:
            build_atlas(
                run.schedule.glyphs,
                run.schedule.target,
                AtlasConfig(width=2, height=2, padding=1),
                self.root / "tiny",
                self.root / ".tmp",
            )
        self.assertEqual(caught.exception.code, E_ATLAS_OVERFLOW)
        self.assertIn("glyphs", caught.exception.details)

    def test_tampered_atlas_fails_hash_verification(self) -> None:
        atlas_path = self.report.targets[0].atlas.atlas_path
        data = bytearray(atlas_path.read_bytes())
        data[-20] ^= 0x01
        atlas_path.write_bytes(bytes(data))
        with self.assertRaises(GlyphAtlasError) as caught:
            verify_output_hashes(atlas_path.parent)
        self.assertEqual(caught.exception.code, E_SNAPSHOT_MISMATCH)


class DeterminismTests(unittest.TestCase):
    def test_generated_artifacts_are_byte_identical(self) -> None:
        with tempfile.TemporaryDirectory() as first_tmp, tempfile.TemporaryDirectory() as second_tmp:
            first = Path(first_tmp) / "samples"
            second = Path(second_tmp) / "samples"
            summary_a = generate_samples(first, seed=1947)
            summary_b = generate_samples(second, seed=1947)
            self.assertEqual(summary_a, summary_b)
            files_a = sorted(path.relative_to(first) for path in first.rglob("*") if path.is_file())
            files_b = sorted(path.relative_to(second) for path in second.rglob("*") if path.is_file())
            self.assertEqual(files_a, files_b)
            for relative in files_a:
                with self.subTest(relative=str(relative)):
                    self.assertEqual((first / relative).read_bytes(), (second / relative).read_bytes())

    def test_resume_of_complete_snapshot_does_not_duplicate_tiles(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            write_json(root / "font.json", square_hole_font())
            config_path = simple_config(root, glyphs=["hole"])
            first = run_pipeline(config_path)
            second = run_pipeline(config_path, resume=True)
            glyph_a = first.glyphs_for("t12")["hole"]
            glyph_b = second.glyphs_for("t12")["hole"]
            self.assertEqual(glyph_a.coverage_hash, glyph_b.coverage_hash)
            self.assertEqual(len(glyph_b.tiles), len(glyph_b.plan.tile_boxes(6, 6)))
            self.assertTrue(all(tile.index == index for index, tile in enumerate(glyph_b.tiles)))


if __name__ == "__main__":
    unittest.main()

