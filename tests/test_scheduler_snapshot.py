from __future__ import annotations

import dataclasses
import json
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

import numpy as np

from glyph_atlas.config import load_config
from glyph_atlas.errors import (
    E_BUDGET_EXCEEDED,
    E_BUSY,
    E_CANCELLED,
    E_CONFIG_INVALID,
    E_SNAPSHOT_MISMATCH,
    E_TILE_MISMATCH,
    GlyphAtlasError,
)
from glyph_atlas.font import load_font
from glyph_atlas.pipeline import run_pipeline
from glyph_atlas.raster import coverage_sha256, rasterize_tile
from glyph_atlas.scheduler import RasterScheduler
from tests.helpers import simple_config, square_hole_font, write_json


class SchedulerSnapshotTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        write_json(self.root / "font.json", square_hole_font())

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def _clean_run(self, name: str) -> tuple[Path, dict]:
        root = self.root / name
        root.mkdir()
        write_json(root / "font.json", square_hole_font())
        config_path = simple_config(root, glyphs=["hole"])
        report = run_pipeline(config_path)
        return config_path, report.glyphs_for("t12")["hole"].coverage_hash

    def test_budget_exhaustion_and_disk_snapshot_resume(self) -> None:
        config_path = simple_config(
            self.root,
            glyphs=["hole"],
            parallelism=1,
            budget={
                "max_task_sampling_points": 10_000,
                "max_task_memory_bytes": 10_000_000,
                "max_total_sampling_points": 150,
                "max_total_memory_bytes": 10_000_000,
            },
        )
        with self.assertRaises(GlyphAtlasError) as caught:
            run_pipeline(config_path)
        error = caught.exception
        self.assertEqual(error.code, E_BUDGET_EXCEEDED)
        self.assertIn("task", error.details)
        self.assertIn("coord", error.details)
        self.assertIn("used_sampling_points", error.details)
        self.assertGreater(len(error.details["completed_indices"]), 0)
        snapshot_path = Path(error.details["snapshot"])
        self.assertTrue(snapshot_path.exists())

        document = json.loads(config_path.read_text(encoding="utf-8"))
        document["budget"]["max_total_sampling_points"] = 100_000_000
        write_json(config_path, document)
        resumed = run_pipeline(config_path, resume=True)
        resumed_glyph = resumed.glyphs_for("t12")["hole"]
        self.assertTrue(resumed_glyph.resumed)

        _, clean_hash = self._clean_run("clean")
        self.assertEqual(resumed_glyph.coverage_hash, clean_hash)
        self.assertEqual(resumed_glyph.whole_reference["max_difference"], 0)

    def test_cancel_then_resume_uses_only_committed_tiles(self) -> None:
        config_path = simple_config(self.root, glyphs=["hole"], parallelism=1)
        cancel_event = threading.Event()
        cancel_event.set()
        with self.assertRaises(GlyphAtlasError) as caught:
            run_pipeline(config_path, cancel_event=cancel_event)
        self.assertEqual(caught.exception.code, E_CANCELLED)
        resumed = run_pipeline(config_path, resume=True)
        resumed_glyph = resumed.glyphs_for("t12")["hole"]
        self.assertEqual(len(resumed_glyph.tiles), len(resumed_glyph.plan.tile_boxes(6, 6)))
        self.assertEqual(
            len({(tile.coord, tile.index) for tile in resumed_glyph.tiles}),
            len(resumed_glyph.tiles),
        )

    def test_snapshot_version_and_hash_chain_mismatch(self) -> None:
        config_path = simple_config(self.root, glyphs=["hole"])
        run_pipeline(config_path)
        snapshot_files = sorted((self.root / "snapshots").glob("*.snapshot.json"))
        self.assertEqual(len(snapshot_files), 1)
        snapshot_path = snapshot_files[0]
        original = snapshot_path.read_text(encoding="utf-8")

        tampered = json.loads(original)
        tampered["version"] = 99
        write_json(snapshot_path, tampered)
        with self.assertRaises(GlyphAtlasError) as caught:
            run_pipeline(config_path, resume=True)
        self.assertEqual(caught.exception.code, E_SNAPSHOT_MISMATCH)

        snapshot_path.write_text(original, encoding="utf-8")
        tampered = json.loads(original)
        payload = bytearray(__import__("base64").b64decode(tampered["tiles"][0]["coverage_b64"]))
        payload[0] ^= 0x01
        tampered["tiles"][0]["coverage_b64"] = __import__("base64").b64encode(bytes(payload)).decode("ascii")
        write_json(snapshot_path, tampered)
        with self.assertRaises(GlyphAtlasError) as caught:
            run_pipeline(config_path, resume=True)
        self.assertEqual(caught.exception.code, E_SNAPSHOT_MISMATCH)

    def test_busy_submission_and_fixed_parallelism(self) -> None:
        config_path = simple_config(self.root, glyphs=["hole"], parallelism=2)
        config = load_config(config_path)
        font = load_font(config.font_path)
        scheduler = RasterScheduler(font, config, config.targets[0])
        scheduler.submit_glyph("hole")
        with self.assertRaises(GlyphAtlasError) as caught:
            scheduler.submit_glyph("hole")
        self.assertEqual(caught.exception.code, E_BUSY)
        with self.assertRaises(GlyphAtlasError) as caught:
            scheduler.set_parallelism(3)
        self.assertEqual(caught.exception.code, E_CONFIG_INVALID)

    def test_tile_mismatch_is_located(self) -> None:
        config_path = simple_config(self.root, glyphs=["hole"], tile_width=6, tile_height=6)
        original = rasterize_tile

        def altered(plan, box, sampling, coord=(0, 0)):
            tile = original(plan, box, sampling, coord)
            if coord == (1, 0):
                coverage = tile.coverage.copy()
                coverage[0, 0] = (int(coverage[0, 0]) + 1) % 256
                return dataclasses.replace(
                    tile,
                    coverage=coverage,
                    content_hash=coverage_sha256(coverage),
                )
            return tile

        with mock.patch("glyph_atlas.scheduler.rasterize_tile", side_effect=altered):
            with self.assertRaises(GlyphAtlasError) as caught:
                run_pipeline(config_path)
        self.assertEqual(caught.exception.code, E_TILE_MISMATCH)
        self.assertEqual(caught.exception.details["coord"], [1, 0])
        self.assertIn("pixel", caught.exception.details)


if __name__ == "__main__":
    unittest.main()

