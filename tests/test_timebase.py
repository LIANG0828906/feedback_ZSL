from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from livetranscode.config import parse_config
from livetranscode.errors import BusinessError, ErrorCode
from livetranscode.source import (
    build_timeline,
    read_source_audio,
    read_video_description,
)
from livetranscode.util import write_json_atomic

from .helpers import base_config, write_frames, write_wav


class TimebaseTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_unified_integer_boundaries_partition_source(self) -> None:
        write_wav(self.root / "audio.wav", 44100, 2, 4.0)
        write_frames(self.root / "frames.json", 4.0, fps=25)
        audio = read_source_audio(self.root / "audio.wav")
        video = read_video_description(self.root / "frames.json")
        config = parse_config(base_config(["a", "b"]))
        timeline = build_timeline(audio, video, config)
        self.assertEqual(timeline.boundaries[0].start_ticks, 0)
        self.assertEqual(timeline.boundaries[-1].end_ticks, timeline.source_end_ticks)
        for index, boundary in enumerate(timeline.boundaries):
            self.assertEqual(boundary.start_source_sample, boundary.start_ticks * 44100 // timeline.ticks_per_second)
            if index:
                self.assertEqual(boundary.start_ticks, timeline.boundaries[index - 1].end_ticks)
                self.assertEqual(
                    boundary.start_source_sample,
                    timeline.boundaries[index - 1].end_source_sample,
                )

    def test_invalid_sample_rate_is_timebase_error(self) -> None:
        write_wav(self.root / "audio.wav", 8000, 1, 1.0)
        with self.assertRaises(BusinessError) as caught:
            read_source_audio(self.root / "audio.wav")
        self.assertEqual(caught.exception.code, ErrorCode.TIMEBASE_INVALID)

    def test_invalid_channel_layout_is_timebase_error(self) -> None:
        write_wav(self.root / "audio.wav", 16000, 3, 1.0)
        with self.assertRaises(BusinessError) as caught:
            read_source_audio(self.root / "audio.wav")
        self.assertEqual(caught.exception.code, ErrorCode.TIMEBASE_INVALID)

    def test_non_monotonic_or_gapped_frames_are_rejected(self) -> None:
        write_json_atomic(
            self.root / "frames.json",
            {
                "timescale": 90000,
                "frames": [
                    {"index": 0, "pts": 0, "duration": 3000},
                    {"index": 1, "pts": 6000, "duration": 3000},
                ],
            },
        )
        with self.assertRaises(BusinessError) as caught:
            read_video_description(self.root / "frames.json")
        self.assertEqual(caught.exception.code, ErrorCode.TIMEBASE_INVALID)

    def test_fractional_timebase_remainder_is_rejected(self) -> None:
        write_json_atomic(
            self.root / "frames.json",
            {
                "timescale": 1000,
                "frames": [{"index": 0, "pts": "0.0005", "duration": 1}],
            },
        )
        with self.assertRaises(BusinessError) as caught:
            read_video_description(self.root / "frames.json")
        self.assertEqual(caught.exception.code, ErrorCode.TIMEBASE_INVALID)
        self.assertIn("remainder", caught.exception.message)


if __name__ == "__main__":
    unittest.main()

