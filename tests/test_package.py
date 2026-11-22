from __future__ import annotations

import unittest
from dataclasses import replace

from mediaforge.errors import E_CHAP, E_PREC, E_VER
from mediaforge.metadata import export_metadata, load_metadata_dict, project_model
from mediaforge.package import build_package, parse_package_bytes, read_artifact_model
from mediaforge.timebase import Timebase

from .support import expect_error, loaded


class PackageTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.model = loaded("g01_neon_city", "v3").model

    def test_repeated_packaging_is_byte_identical(self) -> None:
        first = build_package(self.model, "v3")
        second = build_package(self.model, "v3")
        self.assertEqual(first.artifact, second.artifact)
        self.assertEqual(first.artifact_sha256, second.artifact_sha256)

    def test_package_declares_sorted_chapters(self) -> None:
        reversed_model = self.model.copy(chapters=tuple(reversed(self.model.chapters)))
        result = build_package(reversed_model, "v3")
        chapters = result.declaration["chapters"]
        self.assertEqual(
            [chapter["chapter_id"] for chapter in chapters],
            ["ch_intro", "ch_develop", "ch_finale"],
        )
        bounds = [(chapter["start_ticks"], chapter["end_ticks"]) for chapter in chapters]
        self.assertEqual(bounds, sorted(bounds))

    def test_overlapping_chapters_return_e_chap_with_number(self) -> None:
        chapters = list(self.model.chapters)
        chapters[0] = replace(chapters[0], end_ticks=chapters[1].start_ticks + 1)
        model = self.model.copy(chapters=tuple(chapters))
        exc = expect_error(E_CHAP, lambda: build_package(model, "v3"))
        self.assertEqual(exc.field_path, "$.chapters[1].start_ticks")
        self.assertEqual(exc.details["chapter_number"], 2)

    def test_out_of_bounds_chapter_returns_e_chap(self) -> None:
        chapters = list(self.model.chapters)
        chapters[-1] = replace(chapters[-1], end_ticks=self.model.duration_ticks + 1)
        model = self.model.copy(chapters=tuple(chapters))
        exc = expect_error(E_CHAP, lambda: build_package(model, "v3"))
        self.assertEqual(exc.field_path, "$.chapters[2].end_ticks")
        self.assertEqual(exc.details["chapter_number"], 3)

    def test_unknown_target_returns_e_ver(self) -> None:
        exc = expect_error(E_VER, lambda: build_package(self.model, "v7"))
        self.assertEqual(exc.field_path, "$.target_version")

    def test_precision_error_is_not_silently_rounded(self) -> None:
        track = self.model.tracks[0]
        changed = replace(track, timebase=Timebase(1, 90000), duration_ticks=5001)
        model = self.model.copy(tracks=(changed, self.model.tracks[1]))
        exc = expect_error(E_PREC, lambda: build_package(model, "v1"))
        self.assertEqual(exc.field_path, "$.tracks[0].duration_ms")

    def test_artifact_readback_matches_projection(self) -> None:
        for target in ("v1", "v2", "v3"):
            result = build_package(self.model, target)
            parsed = parse_package_bytes(result.artifact)
            self.assertEqual(parsed.target_version, target)
            self.assertEqual(parsed.declaration["track_count"], 2)
            with self.subTest(target=target):
                import tempfile
                from pathlib import Path

                with tempfile.TemporaryDirectory(prefix="mf-readback-") as temp:
                    path = Path(temp) / "artifact.mfz"
                    path.write_bytes(result.artifact)
                    readback = read_artifact_model(path)
                projected = project_model(self.model, target)
                self.assertEqual(readback.source_version, target)
                self.assertEqual(readback.model_id, projected.model_id)
                self.assertEqual(readback.duration_ticks, projected.duration_ticks)
                self.assertEqual(readback.timebase, projected.timebase)
                self.assertEqual(readback.tags, projected.tags)
                self.assertEqual(
                    [(track.track_id, track.sample_count, track.timebase, track.language) for track in readback.tracks],
                    [(track.track_id, track.sample_count, track.timebase, track.language) for track in projected.tracks],
                )
                self.assertEqual(
                    [track.languages for track in readback.tracks],
                    [track.languages for track in projected.tracks],
                )
                self.assertEqual(
                    [(chapter.chapter_id, chapter.start_ticks, chapter.end_ticks) for chapter in readback.chapters],
                    [(chapter.chapter_id, chapter.start_ticks, chapter.end_ticks) for chapter in projected.chapters],
                )

    def test_high_low_high_roundtrip_keeps_sample_count_and_timebase(self) -> None:
        low = load_metadata_dict(export_metadata(self.model, "v1").metadata)
        high = load_metadata_dict(export_metadata(low, "v3").metadata)
        self.assertEqual(
            [(track.sample_count, track.timebase) for track in self.model.tracks],
            [(track.sample_count, track.timebase) for track in high.tracks],
        )

    def test_payload_corruption_changes_artifact_digest(self) -> None:
        result = build_package(self.model, "v3")
        corrupted = bytearray(result.artifact)
        corrupted[-40] ^= 0x01
        self.assertNotEqual(bytes(corrupted), result.artifact)


if __name__ == "__main__":
    unittest.main()
