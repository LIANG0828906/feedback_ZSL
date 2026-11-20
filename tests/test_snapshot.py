from __future__ import annotations

import json
import tempfile
import threading
import unittest
from pathlib import Path

from glyphforge.config import WorkbenchConfig
from glyphforge.errors import ErrorCode, GlyphForgeError, RunCancelled
from glyphforge.parser import parse_font
from glyphforge.pipeline import run_pipeline
from glyphforge.samples import build_font_specs
from glyphforge.snapshot import load_snapshot


class SnapshotTests(unittest.TestCase):
    def _font(self):
        return parse_font(build_font_specs()["holes.json"], path="holes.json")

    def test_cancel_then_resume_matches_clean_run(self) -> None:
        font = self._font()
        config = WorkbenchConfig(pixel_size=18, tile_size=4)
        clean = run_pipeline(font, "8OB", config)
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "state.json"
            cancel = threading.Event()

            def progress(event):
                if event.get("event") == "tile" and event.get("status") == "computed":
                    cancel.set()

            with self.assertRaises(RunCancelled):
                run_pipeline(
                    font,
                    "8OB",
                    config,
                    snapshot_path=path,
                    cancel_event=cancel,
                    progress=progress,
                )
            snapshot = load_snapshot(path)
            self.assertGreater(len(snapshot.tiles), 0)
            resumed = run_pipeline(
                font,
                "8OB",
                config,
                snapshot_path=path,
                resume=True,
            )
            self.assertGreater(resumed.reuse_count, 0)
            self.assertGreater(resumed.recompute_count, 0)
            self.assertEqual(clean.atlas.alpha_hash, resumed.atlas.alpha_hash)
            self.assertEqual(
                [glyph.coverage_hash for glyph in clean.glyphs],
                [glyph.coverage_hash for glyph in resumed.glyphs],
            )
            self.assertFalse(list(Path(temporary).glob("*.tmp*")))

    def test_content_hash_tamper_is_rejected(self) -> None:
        font = self._font()
        config = WorkbenchConfig(pixel_size=12, tile_size=8)
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "state.json"
            run_pipeline(font, "O", config, snapshot_path=path)
            document = json.loads(path.read_text(encoding="utf-8"))
            document["tiles"][0]["content_hash"] = "0" * 64
            path.write_text(json.dumps(document), encoding="utf-8")
            with self.assertRaises(GlyphForgeError) as caught:
                load_snapshot(path)
            self.assertEqual(caught.exception.code, ErrorCode.E_SNAPSHOT_MISMATCH)

    def test_version_mismatch_is_rejected(self) -> None:
        font = self._font()
        config = WorkbenchConfig(pixel_size=12, tile_size=8)
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "state.json"
            run_pipeline(font, "O", config, snapshot_path=path)
            document = json.loads(path.read_text(encoding="utf-8"))
            document["version"] = 999
            path.write_text(json.dumps(document), encoding="utf-8")
            with self.assertRaises(GlyphForgeError) as caught:
                load_snapshot(path)
            self.assertEqual(caught.exception.code, ErrorCode.E_SNAPSHOT_MISMATCH)

    def test_metadata_tamper_breaks_hash_chain(self) -> None:
        font = self._font()
        config = WorkbenchConfig(pixel_size=12, tile_size=8)
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "state.json"
            run_pipeline(font, "O", config, snapshot_path=path)
            document = json.loads(path.read_text(encoding="utf-8"))
            document["tiles"][0]["summary"]["quantization_abs_error"] += 0.25
            path.write_text(json.dumps(document), encoding="utf-8")
            with self.assertRaises(GlyphForgeError) as caught:
                load_snapshot(path)
            self.assertEqual(caught.exception.code, ErrorCode.E_SNAPSHOT_MISMATCH)


if __name__ == "__main__":
    unittest.main()
