from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import numpy as np

from mastering.automation import SmoothingConfig, load_and_validate_automation
from mastering.errors import LoudnessOutOfTolerance, ProjectRejected
from mastering.limiter import limit_true_peak, true_peak_dbtp
from mastering.loudness import (
    LoudnessResult,
    integrated_loudness,
    loudness_gain_for_target,
)
from mastering.resample import DriftAnchor, DriftMap, render_track
from mastering.sync import detect_sync_pulse, plan_sync_alignment
from mastering.wav import (
    parse_output_format,
    read_wav,
    read_wav_header,
    write_wav_atomic,
)


class WavTests(unittest.TestCase):
    def test_round_trip_16_24_and_float32_with_consistent_headers(self) -> None:
        rng = np.random.default_rng(7)
        samples = rng.uniform(-0.95, 0.95, size=(1001, 2))
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            for fmt in ("pcm_s16", "pcm_s24", "float32"):
                wave_format, bits = parse_output_format(fmt)
                path = root / f"{fmt}.wav"
                info = write_wav_atomic(path, samples, 48000, wave_format, bits)
                header = read_wav_header(path)
                self.assertEqual(header.frame_count, 1001)
                self.assertEqual(header.sample_rate, 48000)
                self.assertEqual(header.channels, 2)
                self.assertEqual(header.bits_per_sample, bits)
                self.assertEqual(
                    header.data_bytes,
                    header.frame_count * header.channels * (bits // 8),
                )
                decoded = read_wav(path)
                self.assertEqual(decoded.samples.shape, samples.shape)
                self.assertEqual(info["frame_count"], 1001)
                if wave_format == "pcm":
                    self.assertLessEqual(
                        float(np.max(np.abs(decoded.samples - samples))),
                        1.0 / (1 << (bits - 1)),
                    )

    def test_24_bit_is_packed_as_little_endian_three_bytes(self) -> None:
        samples = np.asarray(
            [[-1.0], [-0.5], [0.0], [0.5], [0.999999]],
            dtype=np.float64,
        )
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "packed24.wav"
            write_wav_atomic(path, samples, 44100, "pcm", 24)
            raw = path.read_bytes()
            header = read_wav_header(path)
            payload = raw[header.data_offset : header.data_offset + 15]
            self.assertEqual(header.data_bytes, 15)
            self.assertEqual(payload[0:3], b"\x00\x00\x80")
            self.assertEqual(payload[3:6], b"\x00\x00\xc0")
            self.assertEqual(payload[6:9], b"\x00\x00\x00")


class AutomationTests(unittest.TestCase):
    def _config(self) -> SmoothingConfig:
        return SmoothingConfig(
            gain_min_db=-96.0,
            gain_max_db=24.0,
            max_interval_seconds=0.03,
            min_gain_delta_db=6.0,
            duration_seconds=0.2,
        )

    def test_sharp_segments_are_smoothed_within_curve_bounds(self) -> None:
        payload = {
            "tracks": {
                "vox": {
                    "nodes": [
                        [0.0, 0.0],
                        [0.1, 0.0],
                        [0.11, -12.0],
                        [0.5, -12.0],
                        [1.0, 0.0],
                    ]
                }
            }
        }
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "automation.json"
            path.write_text(json.dumps(payload), encoding="utf-8")
            plans = load_and_validate_automation(path, ["vox"], self._config())
            plan = plans["vox"]
            self.assertEqual(plan.sharp_segment_count, 1)
            self.assertEqual(plan.smoothing_region_count, 1)
            self.assertAlmostEqual(plan.smoothing_duration_seconds, 0.2)
            region = plan.regions[0]
            self.assertGreaterEqual(region.start_seconds, 0.0)
            self.assertLessEqual(region.end_seconds, 1.0)
            before = float(plan.gain_db_at(region.start_seconds))
            after = float(plan.gain_db_at(region.end_seconds))
            at_left_edge = float(plan.gain_db_at(region.start_seconds - 1e-6))
            at_right_edge = float(plan.gain_db_at(region.end_seconds + 1e-6))
            self.assertAlmostEqual(before, at_left_edge, places=4)
            self.assertAlmostEqual(after, at_right_edge, places=4)

    def test_invalid_curves_are_all_validated_before_use(self) -> None:
        payload = {
            "tracks": {
                "a": {"nodes": [[0.0, 0.0], [1.0, 50.0]]},
                "b": {"nodes": [[0.0, 0.0], [0.5, -2.0], [0.25, 0.0]]},
            }
        }
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "automation.json"
            path.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaises(ProjectRejected) as raised:
                load_and_validate_automation(path, ["a", "b"], self._config())
            codes = [issue.code for issue in raised.exception.issues]
            tracks = {issue.track for issue in raised.exception.issues}
            self.assertEqual(codes, ["INVALID_AUTOMATION_CURVE"] * 2)
            self.assertEqual(tracks, {"a", "b"})


class DriftTests(unittest.TestCase):
    def test_anchor_impulses_land_within_two_output_samples_and_length_is_exact(self) -> None:
        input_rate = 44100
        output_rate = 48000
        duration = 3.0
        source_frames = int(round(duration * input_rate + 0.05 * input_rate))
        source = np.zeros((source_frames, 1), dtype=np.float64)
        anchors = (
            DriftAnchor(0.0, 0.0),
            DriftAnchor(0.5 * input_rate + 0.020 * input_rate, 0.5),
            DriftAnchor(1.0 * input_rate + 0.015 * input_rate, 1.0),
            DriftAnchor(2.0 * input_rate + 0.005 * input_rate, 2.0),
            DriftAnchor(3.0 * input_rate - 0.010 * input_rate, 3.0),
        )
        for anchor in anchors:
            index = int(round(anchor.source_sample))
            source[index, 0] = 1.0
        drift = DriftMap(anchors, input_rate, output_rate, enabled=True)
        output_frames = int(round(duration * output_rate))
        rendered = render_track(
            source,
            input_rate,
            output_rate,
            output_frames,
            drift,
            1,
        )
        self.assertEqual(rendered.shape[0], output_frames)
        for anchor in anchors:
            expected_frame = int(round(anchor.target_seconds * output_rate))
            radius = 3
            window = rendered[
                max(0, expected_frame - radius) : expected_frame + radius + 1, 0
            ]
            actual_frame = (
                max(0, expected_frame - radius) + int(np.argmax(np.abs(window)))
            )
            self.assertLessEqual(abs(actual_frame - expected_frame), 2)

    def test_outside_anchor_range_uses_constant_adjacent_rate(self) -> None:
        anchors = (
            DriftAnchor(1000.0, 1.0),
            DriftAnchor(3000.0, 2.0),
        )
        drift = DriftMap(anchors, 48000, 48000, enabled=True)
        below = float(drift.target_to_source(0.5))
        above = float(drift.target_to_source(3.0))
        self.assertAlmostEqual(below, 0.0)
        self.assertAlmostEqual(above, 5000.0)

    def test_sync_detection_produces_integer_alignment_offset(self) -> None:
        sample_rate = 48000
        source = np.zeros((4800, 1), dtype=np.float64)
        source[1043, 0] = 0.5
        onset, warning, _ = detect_sync_pulse(
            source,
            sample_rate,
            {
                "search_start_sample": 900,
                "search_end_sample": 1200,
                "threshold_ratio": 0.2,
            },
            "offset_track",
        )
        self.assertIsNone(warning)
        self.assertEqual(onset, 1043)
        drift = DriftMap(
            (
                DriftAnchor(0.0, 0.0),
                DriftAnchor(4800.0, 0.1),
            ),
            sample_rate,
            sample_rate,
            enabled=False,
        )
        detection = plan_sync_alignment(
            "offset_track",
            onset,
            drift,
            sample_rate,
            target_time_seconds=0.02,
            warning=None,
        )
        self.assertEqual(detection.applied_offset_samples, -83)
        self.assertEqual(detection.alignment_residual_samples, 0.0)
        self.assertTrue(detection.aligned)


class LoudnessAndLimiterTests(unittest.TestCase):
    def test_bs1770_reference_sine(self) -> None:
        sample_rate = 48000
        t = np.arange(sample_rate * 2) / sample_rate
        tone = 0.1 * np.sin(2.0 * np.pi * 1000.0 * t)
        mono = integrated_loudness(tone[:, None], sample_rate)
        stereo = integrated_loudness(np.repeat(tone[:, None], 2, axis=1), sample_rate)
        self.assertAlmostEqual(mono.integrated_lufs, -23.0, delta=0.15)
        self.assertAlmostEqual(stereo.integrated_lufs, -20.0, delta=0.15)

    def test_true_peak_limiter_respects_ceiling_and_preserves_frames(self) -> None:
        sample_rate = 48000
        t = np.arange(sample_rate * 2) / sample_rate
        signal = 1.15 * np.sin(2.0 * np.pi * 997.0 * t)
        stereo = np.column_stack((signal, signal * 0.8))
        before, _ = true_peak_dbtp(stereo, 4)
        self.assertGreater(before, -1.0)
        result = limit_true_peak(
            stereo,
            sample_rate,
            -1.0,
            lookahead_ms=2.0,
            release_ms=20.0,
            max_iterations=6,
            safety_margin_db=0.1,
            oversample=4,
        )
        after, _ = true_peak_dbtp(result.samples, 4)
        self.assertLessEqual(after, -1.0)
        self.assertEqual(result.samples.shape, stereo.shape)
        self.assertGreater(result.report["triggered_frames"], 0)
        self.assertGreater(result.report["max_attenuation_db"], 0.0)
        self.assertTrue(result.report["delay_compensated"])

    def test_limiter_lookahead_delay_is_compensated_on_timeline(self) -> None:
        sample_rate = 48000
        signal = np.zeros((sample_rate, 2), dtype=np.float64)
        impulse_frame = 5000
        signal[impulse_frame, :] = 0.99
        result = limit_true_peak(
            signal,
            sample_rate,
            -1.0,
            lookahead_ms=2.0,
            release_ms=10.0,
            max_iterations=4,
            safety_margin_db=0.1,
            oversample=4,
        )
        detected = int(np.argmax(np.max(np.abs(result.samples), axis=1)))
        self.assertEqual(detected, impulse_frame)

    def test_loudness_tolerance_failure_is_explicit(self) -> None:
        samples = np.zeros((48000, 2), dtype=np.float64)
        before = LoudnessResult(-20.0, 10, 10, -70.0, -30.0)
        wrong = LoudnessResult(-15.0, 10, 10, -70.0, -25.0)
        with mock.patch(
            "mastering.loudness.integrated_loudness",
            side_effect=[before, wrong],
        ):
            with self.assertRaises(LoudnessOutOfTolerance) as raised:
                loudness_gain_for_target(samples, 48000, -20.0, tolerance_lu=0.5)
        self.assertEqual(raised.exception.issues[0].code, "LOUDNESS_OUT_OF_TOLERANCE")


if __name__ == "__main__":
    unittest.main()

