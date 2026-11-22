from __future__ import annotations

import copy
import json
import shutil
import tempfile
import unittest
from pathlib import Path

from mediaforge.errors import E_CONFLICT, E_CORRUPT, E_ORDER, E_TRACK
from mediaforge.injection import InjectionConfig
from mediaforge.jsonutil import canonical_json_bytes
from mediaforge.pipeline import load_sample

from .support import SAMPLES, copy_group, expect_error, loaded, read_json, write_json


class RawImportTests(unittest.TestCase):
    def test_import_all_versions(self) -> None:
        for version in ("v1", "v2", "v3"):
            sample = loaded("g01_neon_city", version)
            self.assertEqual(len(sample.model.tracks), 2)
            self.assertEqual(sample.model.tracks[0].sample_count, 90)
            self.assertEqual(sample.model.tracks[1].sample_count, 72000)
            self.assertEqual(sample.model.tracks[0].timebase.to_dict(), {"num": 1, "den": 600})
            self.assertEqual(sample.model.tracks[1].language, "zh-CN")
            self.assertTrue(all(track.payload for track in sample.model.tracks))

    def test_metadata_conflict_injection(self) -> None:
        config = InjectionConfig(conflict_track="video_main", conflict_language="ja-JP")
        exc = expect_error(
            E_CONFLICT,
            lambda: load_sample(SAMPLES / "g01_neon_city", "v3", injection=config),
        )
        self.assertEqual(exc.field_path, "$.tracks[0].language")

    def test_timestamp_inversion_injection(self) -> None:
        config = InjectionConfig(order_track="video_main", order_frame_index=2)
        exc = expect_error(
            E_ORDER,
            lambda: load_sample(SAMPLES / "g01_neon_city", "v3", injection=config),
        )
        self.assertIn("frames[2].pts_ticks", exc.field_path)

    def test_missing_track_injection(self) -> None:
        config = InjectionConfig(missing_track="audio_main")
        exc = expect_error(
            E_TRACK,
            lambda: load_sample(SAMPLES / "g01_neon_city", "v3", injection=config),
        )
        self.assertEqual(exc.field_path, "$.tracks[1]")

    def test_corrupt_payload_injection(self) -> None:
        config = InjectionConfig(corrupt_track="audio_main", corrupt_offset=51)
        exc = expect_error(
            E_CORRUPT,
            lambda: load_sample(SAMPLES / "g01_neon_city", "v3", injection=config),
        )
        self.assertEqual(exc.field_path, "$.tracks[1].payload_sha256")

    def test_input_missing_track_file(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mf-missing-") as temp:
            group = Path(temp) / "group"
            shutil.copytree(SAMPLES / "g01_neon_city", group)
            (group / "raw" / "audio_main.json").unlink()
            exc = expect_error(E_TRACK, lambda: load_sample(group, "v3"))
            self.assertEqual(exc.field_path, "$.tracks[1]")

    def test_input_timestamp_interval_error(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mf-order-") as temp:
            group = Path(temp) / "group"
            shutil.copytree(SAMPLES / "g01_neon_city", group)
            path = group / "raw" / "video_main.json"
            value = read_json(path)
            value["frames"][1]["pts_ticks"] = 3
            body = {
                "pts_ticks": value["frames"][1]["pts_ticks"],
                "rows": value["frames"][1]["rows"],
                "cols": value["frames"][1]["cols"],
                "cells": value["frames"][1]["cells"],
            }
            from mediaforge.jsonutil import sha256_bytes

            value["frames"][1]["sha256"] = sha256_bytes(canonical_json_bytes(body))
            write_json(path, value)
            exc = expect_error(E_ORDER, lambda: load_sample(group, "v3"))
            self.assertIn("frames[1].pts_ticks", exc.field_path)

    def test_input_corrupt_audio_hash(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mf-corrupt-") as temp:
            group = Path(temp) / "group"
            shutil.copytree(SAMPLES / "g01_neon_city", group)
            path = group / "raw" / "audio_main.json"
            value = read_json(path)
            encoded = value["samples_base64"]
            position = len(encoded) // 2
            encoded = encoded[:position] + ("A" if encoded[position] != "A" else "B") + encoded[position + 1 :]
            value["samples_base64"] = encoded
            write_json(path, value)
            exc = expect_error(E_CORRUPT, lambda: load_sample(group, "v3"))
            self.assertIn("samples_sha256", exc.field_path)

    def test_metadata_timebase_mismatch_returns_e_track(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mf-timebase-") as temp:
            group = Path(temp) / "group"
            shutil.copytree(SAMPLES / "g01_neon_city", group)
            path = group / "metadata" / "meta_v2.json"
            value = read_json(path)
            value["tracks"][0]["timebase"]["den"] = 1000
            write_json(path, value)
            exc = expect_error(E_TRACK, lambda: load_sample(group, "v2"))
            self.assertEqual(exc.field_path, "$.tracks[0].timebase")


if __name__ == "__main__":
    unittest.main()
