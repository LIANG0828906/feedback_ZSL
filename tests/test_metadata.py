from __future__ import annotations

import copy
import unittest
from dataclasses import replace

from mediaforge.errors import E_META, E_PREC, E_VER
from mediaforge.metadata import (
    export_metadata,
    load_metadata,
    load_metadata_dict,
    project_model,
)
from mediaforge.timebase import Timebase

from .support import SAMPLES, expect_error, loaded, read_json


def core_view(model):
    return {
        "model_id": model.model_id,
        "title": model.title,
        "timebase": model.timebase.to_dict(),
        "duration_ticks": model.duration_ticks,
        "cover": model.cover,
        "tags": model.tags,
        "tracks": [
            (
                track.track_id,
                track.kind,
                track.language,
                track.timebase.to_dict(),
                track.sample_count,
                track.duration_ticks,
                track.codec,
            )
            for track in model.tracks
        ],
        "chapters": [
            (chapter.chapter_id, chapter.index, chapter.start_ticks, chapter.end_ticks, chapter.title)
            for chapter in model.chapters
        ],
    }


class MetadataTests(unittest.TestCase):
    def test_all_versions_map_to_same_core_model(self) -> None:
        models = [loaded("g01_neon_city", version).model for version in ("v1", "v2", "v3")]
        self.assertEqual([model.source_version for model in models], ["v1", "v2", "v3"])
        views = [core_view(model) for model in models]
        self.assertEqual(views[0], views[1])
        self.assertEqual(views[1], views[2])

    def test_version_specific_storage_locations(self) -> None:
        v1 = read_json(SAMPLES / "g01_neon_city" / "metadata" / "meta_v1.json")
        v2 = read_json(SAMPLES / "g01_neon_city" / "metadata" / "meta_v2.json")
        v3 = read_json(SAMPLES / "g01_neon_city" / "metadata" / "meta_v3.json")
        self.assertIn("tags", v1)
        self.assertIn("tags", v2["metadata"])
        self.assertIsInstance(v3["custom_tags"], list)
        self.assertIn("cover", v1)
        self.assertIn("cover", v2["assets"])
        self.assertIn("cover", v3["resources"])
        self.assertIsInstance(v2["chapters"], dict)
        self.assertIsInstance(v3["chapter_table"], list)

    def test_unknown_version_returns_e_meta_with_path(self) -> None:
        value = read_json(SAMPLES / "g01_neon_city" / "metadata" / "meta_v1.json")
        value["metadata_version"] = 9
        exc = expect_error(E_META, lambda: load_metadata_dict(value))
        self.assertEqual(exc.field_path, "$.metadata_version")

    def test_missing_required_field_returns_e_meta_field_path(self) -> None:
        value = copy.deepcopy(read_json(SAMPLES / "g01_neon_city" / "metadata" / "meta_v2.json"))
        del value["asset"]["asset_id"]
        exc = expect_error(E_META, lambda: load_metadata_dict(value))
        self.assertEqual(exc.field_path, "$.asset.asset_id")

    def test_missing_nested_track_field_returns_precise_path(self) -> None:
        value = copy.deepcopy(read_json(SAMPLES / "g01_neon_city" / "metadata" / "meta_v3.json"))
        del value["tracks"][0]["video"]["width"]
        exc = expect_error(E_META, lambda: load_metadata_dict(value))
        self.assertEqual(exc.field_path, "$.tracks[0].video.width")

    def test_unknown_target_version_returns_e_ver(self) -> None:
        model = loaded("g01_neon_city", "v3").model
        exc = expect_error(E_VER, lambda: export_metadata(model, "v9"))
        self.assertEqual(exc.field_path, "$.target_version")

    def test_lower_version_emits_e_lossy_and_preserves_core_fields(self) -> None:
        model = loaded("g01_neon_city", "v3").model
        result = export_metadata(model, "v1")
        self.assertGreaterEqual(len(result.warnings), 5)
        self.assertTrue(all(warning.code == "E_LOSSY" for warning in result.warnings))
        paths = [entry["source_path"] for entry in result.trace if entry.get("code") == "E_LOSSY"]
        self.assertTrue(any(".hdr" in path for path in paths))
        low = load_metadata_dict(result.metadata)
        self.assertEqual(low.source_version, "v1")
        for original, mapped in zip(model.tracks, low.tracks):
            self.assertEqual(original.sample_count, mapped.sample_count)
            self.assertEqual(original.timebase, mapped.timebase)
            self.assertEqual(original.duration_ticks, mapped.duration_ticks)
        self.assertEqual(
            [(chapter.start_ticks, chapter.end_ticks) for chapter in model.chapters],
            [(chapter.start_ticks, chapter.end_ticks) for chapter in low.chapters],
        )

    def test_projection_matches_target_loader_for_expressible_fields(self) -> None:
        model = loaded("g01_neon_city", "v3").model
        for target in ("v1", "v2", "v3"):
            exported = export_metadata(model, target)
            mapped = load_metadata_dict(exported.metadata)
            projected = project_model(model, target)
            self.assertEqual(core_view(projected), core_view(mapped))
            self.assertEqual(
                [track.roles for track in projected.tracks],
                [track.roles for track in mapped.tracks],
            )
            self.assertEqual(
                [track.hdr for track in projected.tracks],
                [track.hdr for track in mapped.tracks],
            )
            self.assertEqual(
                [track.languages for track in projected.tracks],
                [track.languages for track in mapped.tracks],
            )
            self.assertEqual(
                [track.custom for track in projected.tracks],
                [track.custom for track in mapped.tracks],
            )
            self.assertEqual(
                [chapter.tags for chapter in projected.chapters],
                [chapter.tags for chapter in mapped.chapters],
            )
            self.assertEqual(projected.editorial, mapped.editorial)

    def test_high_to_low_to_high_roundtrip_preserves_counts_and_timebases(self) -> None:
        original = loaded("g01_neon_city", "v3").model
        low = load_metadata_dict(export_metadata(original, "v1").metadata)
        high = load_metadata_dict(export_metadata(low, "v3").metadata)
        self.assertEqual(
            [(track.track_id, track.sample_count, track.timebase) for track in original.tracks],
            [(track.track_id, track.sample_count, track.timebase) for track in high.tracks],
        )
        self.assertEqual(
            [(chapter.start_ticks, chapter.end_ticks) for chapter in original.chapters],
            [(chapter.start_ticks, chapter.end_ticks) for chapter in high.chapters],
        )

    def test_non_integral_unit_conversion_returns_e_prec(self) -> None:
        model = loaded("g01_neon_city", "v3").model
        track = model.tracks[0]
        changed = replace(track, timebase=Timebase(1, 90000), duration_ticks=5001)
        changed_model = model.copy(tracks=(changed, model.tracks[1]))
        exc = expect_error(E_PREC, lambda: export_metadata(changed_model, "v1"))
        self.assertEqual(exc.field_path, "$.tracks[0].duration_ms")


if __name__ == "__main__":
    unittest.main()
