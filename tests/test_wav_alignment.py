from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import numpy as np

from acoustic_calibration.alignment import align_impulse_responses, alignment_difference
from acoustic_calibration.models import AlignmentParams, ThresholdConfig
from acoustic_calibration.samples import generate_sample_wav
from acoustic_calibration.wavio import read_wav_16, write_wav_16


class WavAndAlignmentTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.info = generate_sample_wav(self.root / "sample_5_1.wav", seed=19093)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_wav_5_1_round_trip(self) -> None:
        wav = read_wav_16(self.info.path)
        self.assertEqual(wav.channel_count, 6)
        self.assertEqual(wav.sample_rate, 48000)
        self.assertEqual(wav.channel_names, ("FL", "FR", "FC", "LFE", "BL", "BR"))
        self.assertEqual(wav.layout_name, "5.1")
        output = write_wav_16(self.root / "roundtrip.wav", wav.channels, wav.sample_rate)
        reread = read_wav_16(output)
        self.assertEqual(reread.channels.shape, wav.channels.shape)
        self.assertLessEqual(float(np.max(np.abs(reread.channels - wav.channels))), 1.0 / 32768.0)

    def test_mono_round_trip(self) -> None:
        mono = generate_sample_wav(self.root / "mono.wav", layout="mono", seed=8)
        wav = read_wav_16(mono.path)
        self.assertEqual(wav.channel_count, 1)
        self.assertEqual(wav.channel_names, ("M",))
        output = write_wav_16(self.root / "mono-roundtrip.wav", wav.channels)
        self.assertEqual(read_wav_16(output).channels.shape, (1, wav.sample_count))

    def test_arrival_detection_and_alignment(self) -> None:
        wav = read_wav_16(self.info.path)
        params = AlignmentParams()
        alignment, aligned = align_impulse_responses(wav, params)
        self.assertEqual(tuple(channel.arrival_sample for channel in alignment.channels), self.info.arrivals)
        self.assertEqual(alignment.reference_arrival, self.info.arrivals[0])
        self.assertEqual(
            tuple(channel.offset for channel in alignment.channels),
            tuple(arrival - self.info.arrivals[0] for arrival in self.info.arrivals),
        )
        self.assertTrue(all(channel.found for channel in alignment.channels))
        self.assertTrue(all(channel.threshold > 0 for channel in alignment.channels))
        reference_peak = abs(aligned[0, alignment.reference_arrival])
        for channel_index, channel in enumerate(alignment.channels):
            self.assertAlmostEqual(
                abs(aligned[channel_index, alignment.reference_arrival]),
                reference_peak,
                delta=max(1e-9, reference_peak * 0.02),
            )
            self.assertEqual(channel.gain, 1.0 if channel_index == 0 else channel.gain)

    def test_reference_change_reports_arrival_and_offset_difference(self) -> None:
        wav = read_wav_16(self.info.path)
        old, _ = align_impulse_responses(wav, AlignmentParams(reference_channel=0))
        new, _ = align_impulse_responses(wav, AlignmentParams(reference_channel=2))
        difference = alignment_difference(old, new)
        self.assertEqual(difference["old_reference_channel"], 0)
        self.assertEqual(difference["new_reference_channel"], 2)
        self.assertEqual(difference["new_reference_arrival"], self.info.arrivals[2])
        self.assertEqual(difference["changed_channels"], [0, 1, 2, 3, 4, 5])
        for row in difference["channels"]:
            self.assertEqual(row["old_arrival_sample"], row["new_arrival_sample"])
            self.assertNotEqual(row["old_offset"], row["new_offset"])

    def test_threshold_change_reports_threshold_induced_difference(self) -> None:
        wav = read_wav_16(self.info.path)
        old, _ = align_impulse_responses(
            wav,
            AlignmentParams(threshold=ThresholdConfig(floor_dbfs=-60.0)),
        )
        new, _ = align_impulse_responses(
            wav,
            AlignmentParams(threshold=ThresholdConfig(floor_dbfs=-35.0)),
        )
        difference = alignment_difference(old, new)
        self.assertIn("channels", difference)
        self.assertEqual(len(difference["channels"]), 6)
        self.assertTrue(any(channel.threshold < new_threshold for channel, new_threshold in zip(old.channels, [item.threshold for item in new.channels], strict=True)))


if __name__ == "__main__":
    unittest.main()
