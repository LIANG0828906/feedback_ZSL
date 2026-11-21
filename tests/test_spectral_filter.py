from __future__ import annotations

import tempfile
import unittest
from dataclasses import replace
from pathlib import Path

import numpy as np

from acoustic_calibration.alignment import align_impulse_responses
from acoustic_calibration.engine import CalibrationEngine
from acoustic_calibration.errors import CalibrationError, E_FILTER_INVALID
from acoustic_calibration.filters import design_compensation_filter, smooth_band_powers
from acoustic_calibration.models import CalibrationParams, SpectralConfig, TargetCurve
from acoustic_calibration.propagation import build_error_report, compute_channel_errors
from acoustic_calibration.samples import generate_sample_wav
from acoustic_calibration.spectral import estimate_channel_spectrum, make_bands
from acoustic_calibration.wavio import read_wav_16


class SpectralAndFilterTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.info = generate_sample_wav(self.root / "sample.wav", duration_s=0.5)
        self.wav = read_wav_16(self.info.path)
        self.params = CalibrationParams()
        self.alignment, self.aligned = align_impulse_responses(self.wav, self.params.alignment)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_band_modes_and_error_bounds(self) -> None:
        third = make_bands(SpectralConfig(band_mode="third_octave"))
        octave = make_bands(SpectralConfig(band_mode="octave"))
        self.assertGreater(len(third), len(octave))
        self.assertGreaterEqual(len(octave), 7)
        spectral = estimate_channel_spectrum(
            self.aligned[0],
            self.wav.sample_rate,
            third,
            self.params.spectral,
        )
        self.assertEqual(len(spectral.bands), len(third))
        self.assertTrue(all(np.isfinite(band.power_db) for band in spectral.bands))
        self.assertTrue(all(band.segments > 1 for band in spectral.bands))
        self.assertTrue(all(band.relative_upper >= band.rounding_rel for band in spectral.bands))
        self.assertTrue(all(band.coherent_gain > 0.0 for band in spectral.bands))

    def test_linear_phase_fir_and_group_delay(self) -> None:
        bands = make_bands(self.params.spectral)
        spectral = estimate_channel_spectrum(
            self.aligned[0],
            self.wav.sample_rate,
            bands,
            self.params.spectral,
        )
        smoothing = smooth_band_powers(bands, [band.power_db for band in spectral.bands], 1.0 / 3.0)
        design = design_compensation_filter(
            bands,
            smoothing.power_db,
            self.wav.sample_rate,
            self.params,
        )
        coefficients = np.asarray(design.coefficients, dtype=np.float64)
        self.assertEqual(coefficients.size, self.params.filter_order)
        self.assertTrue(np.all(np.isfinite(coefficients)))
        self.assertLess(float(np.max(np.abs(coefficients - coefficients[::-1]))), 1e-14)
        self.assertEqual(design.group_delay_samples, (self.params.filter_order - 1) / 2.0)
        self.assertGreater(design.group_delay_ms, 0.0)
        self.assertGreaterEqual(design.reused_frequency_bins, 0)

        errors = compute_channel_errors(
            self.alignment.channels[0],
            spectral,
            smoothing,
            design,
            threshold_db=0.1,
        )
        self.assertEqual(len(errors), len(bands))
        self.assertTrue(all(error.threshold_crossed for error in errors))
        self.assertTrue(all(error.attribution_node for error in errors))
        self.assertTrue(all(error.nodes for error in errors))
        self.assertTrue(all(any(node.name == "filter_truncation" for node in error.nodes) for error in errors))

    def test_filter_order_and_target_errors_use_business_code(self) -> None:
        bands = make_bands(self.params.spectral)
        spectral = estimate_channel_spectrum(
            self.aligned[0], self.wav.sample_rate, bands, self.params.spectral
        )
        smoothing = smooth_band_powers(bands, [band.power_db for band in spectral.bands], 1.0 / 3.0)
        with self.assertRaises(CalibrationError) as context:
            design_compensation_filter(
                bands,
                smoothing.power_db,
                self.wav.sample_rate,
                replace(self.params, filter_order=7),
            )
        self.assertEqual(context.exception.code, E_FILTER_INVALID)

        payload = self.params.to_dict()
        payload["target_curve"]["points"] = [[1000.0, 0.0]]
        with self.assertRaises(CalibrationError) as target_context:
            CalibrationEngine.parse_params(payload)
        self.assertEqual(target_context.exception.code, E_FILTER_INVALID)

    def test_invalid_filter_keeps_last_published_filter(self) -> None:
        engine = CalibrationEngine(self.root / "workspace", self.info.path)
        published = engine.submit(self.params)
        old_coefficients = tuple(channel.filter.coefficients for channel in published.channels)
        with self.assertRaises(CalibrationError) as context:
            engine.submit(replace(self.params, filter_order=7))
        self.assertEqual(context.exception.code, E_FILTER_INVALID)
        current = engine.current_state()
        self.assertIsNotNone(current)
        self.assertEqual(current.version, published.version)
        self.assertEqual(tuple(channel.filter.coefficients for channel in current.channels), old_coefficients)

    def test_error_report_attributes_threshold_crossing(self) -> None:
        bands = make_bands(self.params.spectral)
        spectral = estimate_channel_spectrum(
            self.aligned[0], self.wav.sample_rate, bands, self.params.spectral
        )
        smoothing = smooth_band_powers(bands, [band.power_db for band in spectral.bands], 1.0 / 3.0)
        design = design_compensation_filter(
            bands, smoothing.power_db, self.wav.sample_rate, self.params
        )
        errors = compute_channel_errors(
            self.alignment.channels[0],
            spectral,
            smoothing,
            design,
            threshold_db=0.1,
        )
        from acoustic_calibration.models import ChannelComputation

        channel = ChannelComputation(
            alignment=self.alignment.channels[0],
            spectral=spectral,
            smoothing=smoothing,
            filter=design,
            errors=errors,
        )
        report = build_error_report(("FL",), (channel,), bands, 0.1)
        self.assertTrue(report.alerts)
        for alert in report.alerts:
            self.assertGreater(alert["upper_db"], alert["threshold_db"])
            self.assertIn("node", alert)
            self.assertIn("center_hz", alert)
            self.assertTrue(alert["node_relative_error"] >= 0.0)


if __name__ == "__main__":
    unittest.main()
