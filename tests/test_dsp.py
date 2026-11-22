from __future__ import annotations

import tempfile
import unittest
from copy import deepcopy
from pathlib import Path
from unittest.mock import patch

import numpy as np

from audio_sync.config import AlignmentConfig
from audio_sync.drift import estimate_drift_model, segment_boundaries
from audio_sync.engine import AlignmentEngine
from audio_sync.errors import E_BOUND, E_PARA, SyncError
from audio_sync.generator import SyntheticSessionConfig, generate_session
from audio_sync.manifest import LoadedSession, load_session
from audio_sync.markers import detect_marker_offset
from audio_sync.resample import align_channel_range


class DspAcceptanceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory(prefix="audio-sync-dsp-")
        cls.session_dir = Path(cls.temp.name) / "session"
        generate_session(
            cls.session_dir,
            config=SyntheticSessionConfig(session_id="dsp-test", seed=20261123),
        )
        cls.session = load_session(cls.session_dir)
        cls.config = AlignmentConfig()
        cls.boundaries = segment_boundaries(
            cls.session.manifest.session_samples,
            cls.config.segment_length,
        )
        cls.detections = {}
        cls.models = {}
        for channel_id in cls.session.manifest.channel_ids():
            if channel_id == cls.session.manifest.reference_channel:
                continue
            detections = [
                detect_marker_offset(
                    cls.session.channels[channel_id],
                    marker,
                    max_offset_samples=cls.config.max_offset_samples,
                    source_offset=next(
                        spec.guard_before
                        for spec in cls.session.manifest.channels
                        if spec.id == channel_id
                    ),
                )
                for marker in cls.session.manifest.markers
            ]
            cls.detections[channel_id] = detections
            cls.models[channel_id] = estimate_drift_model(
                channel_id,
                cls.session.manifest.reference_channel,
                list(cls.session.manifest.markers),
                detections,
                cls.boundaries,
                cls.config,
            )
        cls.result = AlignmentEngine(cls.session, cls.config).run("full")

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def test_observable_marker_estimates_match_truth(self) -> None:
        for channel_id, detections in self.detections.items():
            truth = self.session.truth["channels"][channel_id]
            truth_offsets = np.array([item["offset_samples"] for item in truth["markers"]])
            observed = np.array([item.offset_samples for item in detections])
            self.assertLess(float(np.max(np.abs(observed - truth_offsets))), 0.10)

    def test_drift_model_matches_truth_with_confidence(self) -> None:
        for channel_id, model in self.models.items():
            truth = self.session.truth["channels"][channel_id]
            self.assertLess(abs(model.initial_offset_samples - truth["initial_offset_samples"]), 0.05)
            truth_rates = np.asarray(truth["rates_ppm"], dtype=np.float64)
            self.assertLess(float(np.max(np.abs(model.rates_ppm - truth_rates))), 2.0)
            self.assertTrue(np.all(model.initial_ci_samples > 0.0))
            self.assertTrue(np.all(model.rates_ci_ppm > 0.0))

    def test_alignment_length_and_residual_bound(self) -> None:
        reference_samples = self.session.manifest.session_samples
        for channel_id, output in self.result.channels.items():
            self.assertEqual(output.shape, (reference_samples,))
            report = self.result.reports[channel_id]
            residuals = np.asarray(report["marker_residual_offsets_samples"])
            bounds = np.asarray(report["marker_residual_bounds_samples"])
            self.assertTrue(np.all(np.abs(residuals) <= bounds + 1.0e-12))
            self.assertLess(report["max_abs_residual_samples"], 0.05)
            self.assertEqual(report["bound_source"], "observed_marker_spacing_confidence_and_interpolation_order")

    def test_illegal_interpolation_parameters_return_e_para(self) -> None:
        with self.assertRaises(SyncError) as context:
            AlignmentConfig(interpolation_order=16).validate()
        self.assertEqual(context.exception.code, E_PARA)
        self.assertEqual(context.exception.field, "interpolation_order")

    def test_rate_beyond_parameter_limit_returns_e_para(self) -> None:
        channel_id = "capture_a"
        with self.assertRaises(SyncError) as context:
            estimate_drift_model(
                channel_id,
                self.session.manifest.reference_channel,
                list(self.session.manifest.markers),
                self.detections[channel_id],
                self.boundaries,
                AlignmentConfig(max_drift_ppm=0.01),
            )
        self.assertEqual(context.exception.code, E_PARA)
        self.assertEqual(context.exception.field, "max_drift_ppm")

    def test_interpolation_buffer_never_exceeds_limit(self) -> None:
        model = self.models["capture_a"]
        config = AlignmentConfig(max_buffer_bytes=4096)
        guard_before = next(
            spec.guard_before
            for spec in self.session.manifest.channels
            if spec.id == "capture_a"
        )
        output, stats = align_channel_range(
            self.session.channels["capture_a"],
            model,
            0,
            2000,
            config,
            source_offset=guard_before,
        )
        taps = 2 * config.interpolation_half_width
        self.assertEqual(output.shape, (2000,))
        self.assertLessEqual(stats.block_samples * taps * 24, 4096)

    def test_wide_window_uses_guard_without_zero_padding(self) -> None:
        result = AlignmentEngine(
            self.session,
            AlignmentConfig(interpolation_order=15, window_half_width=32),
        ).run("full")
        for output in result.channels.values():
            self.assertEqual(output.shape, (self.session.manifest.session_samples,))
            self.assertTrue(np.all(np.isfinite(output)))

    def test_engine_raises_e_bound_when_measured_residual_exceeds_bound(self) -> None:
        tight = np.zeros(len(self.session.manifest.markers), dtype=np.float64) + 1.0e-5
        with patch("audio_sync.engine.residual_bounds", return_value=tight):
            with self.assertRaises(SyncError) as context:
                AlignmentEngine(self.session, AlignmentConfig()).run("full")
        self.assertEqual(context.exception.code, E_BOUND)
        self.assertIsNotNone(context.exception.channel)
        self.assertIsNotNone(context.exception.marker)
        self.assertIsNotNone(context.exception.sample)

    def test_engine_truth_tolerance_failure_returns_e_bound(self) -> None:
        with self.assertRaises(SyncError) as context:
            AlignmentEngine(self.session, AlignmentConfig(tolerance_samples=0.0001)).run("full")
        self.assertEqual(context.exception.code, E_BOUND)
        self.assertEqual(context.exception.field, "tolerance_samples")

    def test_estimator_and_bound_do_not_read_truth_values(self) -> None:
        poisoned_truth = deepcopy(self.session.truth)
        for channel_id, entry in poisoned_truth["channels"].items():
            entry["initial_offset_samples"] += 1000.0
            entry["rates_ppm"] = [value + 1000.0 for value in entry["rates_ppm"]]
            for marker in entry["markers"]:
                marker["offset_samples"] += 1000.0
        poisoned = LoadedSession(
            manifest=self.session.manifest,
            channels=self.session.channels,
            truth=poisoned_truth,
            truth_sha256=self.session.truth_sha256,
        )
        result = AlignmentEngine(poisoned, self.config).run(
            "full",
            enforce_truth_tolerance=False,
        )
        for channel_id in self.result.channels:
            self.assertTrue(
                np.array_equal(
                    result.channels[channel_id],
                    self.result.channels[channel_id],
                )
            )
            self.assertTrue(
                np.array_equal(
                    result.reports[channel_id]["marker_residual_bounds_samples"],
                    self.result.reports[channel_id]["marker_residual_bounds_samples"],
                )
            )


if __name__ == "__main__":
    unittest.main()

