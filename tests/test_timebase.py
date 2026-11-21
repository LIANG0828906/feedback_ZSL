from __future__ import annotations

import json
import struct
import tempfile
import unittest
from pathlib import Path

from a11y_rhythm.errors import E_TIMEBASE_INVALID, RhythmError
from a11y_rhythm.timebase import FRAME_HEADER, load_source_bundle

from tests.helpers import SAMPLE_RATE, item, load_fixture, write_fixture


class TimebaseTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.fixture = write_fixture(
            self.root,
            name="timebase",
            duration_seconds=6.0,
            speech=[
                item("sp001", 1.0, 2.0, "First spoken sentence."),
                item("sp002", 2.0, 3.0, "Second spoken sentence."),
            ],
            descriptions=[
                item(
                    "ds001",
                    2.5,
                    3.5,
                    "A small boat crosses.",
                    speaker="AUDIO_DESCRIPTION",
                    min_gap_ms=120,
                )
            ],
        )

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_all_sources_share_sample_base_and_frame_ranges(self) -> None:
        bundle, frame_table, coverage, wav_info = load_fixture(self.fixture)
        self.assertEqual(bundle.sample_rate, SAMPLE_RATE)
        self.assertEqual(wav_info.sample_count, 288_000)
        self.assertEqual(frame_table.frame_count, 150)
        self.assertEqual(coverage, 288_000)
        first = next(item for item in bundle.items if item.item_id == "sp001")
        self.assertEqual(first.start_sample, 48_000)
        frame_start, frame_end = frame_table.frame_range(first.start_sample, first.end_sample)
        self.assertEqual(frame_start, 25)
        self.assertEqual(frame_end, 50)

    def test_frame_table_header_count_conflict_is_located(self) -> None:
        data = bytearray(self.fixture["frame_path"].read_bytes())
        fields = list(FRAME_HEADER.unpack_from(data))
        fields[3] += 1
        FRAME_HEADER.pack_into(data, 0, *fields)
        self.fixture["frame_path"].write_bytes(data)
        with self.assertRaises(RhythmError) as caught:
            load_source_bundle(self.fixture["source_path"])
        self.assertEqual(caught.exception.code, E_TIMEBASE_INVALID)
        self.assertEqual(caught.exception.item_id, "frame_table")
        self.assertIn("contradicts", caught.exception.message)

    def test_sample_rate_mismatch_is_rejected(self) -> None:
        source = json.loads(self.fixture["source_path"].read_text(encoding="utf-8"))
        source["sample_rate"] = 44_100
        self.fixture["source_path"].write_text(json.dumps(source), encoding="utf-8")
        with self.assertRaises(RhythmError) as caught:
            load_source_bundle(self.fixture["source_path"])
        self.assertEqual(caught.exception.code, E_TIMEBASE_INVALID)
        self.assertEqual(caught.exception.item_id, "json")

    def test_frame_offsets_must_match_header_fps(self) -> None:
        data = bytearray(self.fixture["frame_path"].read_bytes())
        fields = list(FRAME_HEADER.unpack_from(data))
        fields[4] = 30
        FRAME_HEADER.pack_into(data, 0, *fields)
        self.fixture["frame_path"].write_bytes(data)
        with self.assertRaises(RhythmError) as caught:
            load_source_bundle(self.fixture["source_path"])
        self.assertEqual(caught.exception.code, E_TIMEBASE_INVALID)
        self.assertEqual(caught.exception.item_id, "frame_table")
        self.assertIn("contradicts", caught.exception.message)

    def test_out_of_bounds_timecode_names_first_conflict(self) -> None:
        source = json.loads(self.fixture["source_path"].read_text(encoding="utf-8"))
        source["speech"][1]["end"] = "00:00:07.000"
        self.fixture["source_path"].write_text(json.dumps(source), encoding="utf-8")
        with self.assertRaises(RhythmError) as caught:
            load_source_bundle(self.fixture["source_path"])
        self.assertEqual(caught.exception.code, E_TIMEBASE_INVALID)
        self.assertEqual(caught.exception.item_id, "sp002")
        self.assertEqual(caught.exception.evidence["first_conflict"], "sp002")

    def test_negative_or_empty_interval_is_rejected(self) -> None:
        source = json.loads(self.fixture["source_path"].read_text(encoding="utf-8"))
        source["speech"][0]["start_sample"] = 10
        source["speech"][0]["end_sample"] = 10
        self.fixture["source_path"].write_text(json.dumps(source), encoding="utf-8")
        with self.assertRaises(RhythmError) as caught:
            load_source_bundle(self.fixture["source_path"])
        self.assertEqual(caught.exception.code, E_TIMEBASE_INVALID)
        self.assertEqual(caught.exception.item_id, "sp001")

    def test_mono_and_51_channels_are_supported(self) -> None:
        surround = write_fixture(
            self.root,
            name="surround",
            duration_seconds=0.4,
            channels=6,
            speech=[item("sp001", 0.02, 0.30, "Short surround sentence.")],
        )
        _, _, _, wav_info = load_fixture(surround)
        self.assertEqual(wav_info.channels, 6)
        self.assertEqual(wav_info.sample_width, 2)


if __name__ == "__main__":
    unittest.main()
