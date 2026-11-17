"""Mechanical-noise source tracing inside declared action segments."""

from __future__ import annotations

import numpy as np

from .audioutil import (band_mask, band_power_series, dbfs,
                        frame_time_centers, reference_band_floor, stft)
from ..manifest import Manifest

N_FFT = 4096
HOP = 1024
FRAME_SNR_DB = 6.0
MIN_ACTIVE_RATIO = 0.4


def _peak_center_hz(power_spec, freqs, band, selector):
    mask = band_mask(freqs, band)
    band_freqs = freqs[mask]
    mean_spectrum = power_spec[selector][:, mask].mean(axis=0)
    peak_idx = int(np.argmax(mean_spectrum))
    if 0 < peak_idx < len(mean_spectrum) - 1:
        y0, y1, y2 = (mean_spectrum[peak_idx - 1],
                      mean_spectrum[peak_idx],
                      mean_spectrum[peak_idx + 1])
        denominator = y0 - 2 * y1 + y2
        shift = (0.5 * (y0 - y2) / denominator) if denominator != 0 else 0.0
        shift = float(np.clip(shift, -1.0, 1.0))
        bin_hz = band_freqs[1] - band_freqs[0]
        return float(band_freqs[peak_idx] + shift * bin_hz)
    return float(band_freqs[peak_idx])


def trace_microphone(signal: np.ndarray, manifest: Manifest, mic_name: str,
                     position: str) -> dict:
    sr = manifest.sample_rate
    band = manifest.noise_band_hz
    power_spec, freqs, _, _ = stft(signal, N_FFT, HOP, sr)
    series = band_power_series(power_spec, freqs, band)
    window = np.hanning(N_FFT)
    physical_scale = 2.0 / (N_FFT * np.sum(window ** 2))
    series = series * physical_scale
    times = frame_time_centers(N_FFT, HOP, sr, len(series))
    blocked = [
        (max(0, a.start_sample - int(0.25 * sr)),
         min(len(signal), a.end_sample + int(0.25 * sr)))
        for a in manifest.actions]
    floor = reference_band_floor(signal, blocked, band, sr, N_FFT, HOP)
    floor_db = dbfs(floor)
    frame_snr = 10 * np.log10(np.maximum(series, 1e-20)
                              / max(floor, 1e-20))

    entries = []
    detected_count = 0
    false_segments = 0
    for action in manifest.actions:
        injected_truth = action.position_gains.get(position, 0.0) > 0.0
        selector = (times >= action.start_time) & (times <= action.end_time)
        if not np.any(selector):
            entries.append({
                "mic_name": mic_name,
                "segment_id": action.segment_id,
                "detected": False,
                "injection_expected": injected_truth,
                "start_time": None,
                "end_time": None,
                "center_frequency_hz": None,
                "estimated_snr_db": None,
            })
            continue
        active = selector & (frame_snr >= FRAME_SNR_DB)
        active_ratio = float(np.count_nonzero(active)) / float(
            np.count_nonzero(selector))
        segment_snr = float(np.median(frame_snr[selector]))
        if active_ratio < MIN_ACTIVE_RATIO:
            entries.append({
                "mic_name": mic_name,
                "segment_id": action.segment_id,
                "detected": False,
                "injection_expected": injected_truth,
                "start_time": None,
                "end_time": None,
                "center_frequency_hz": None,
                "estimated_snr_db": round(segment_snr, 3),
            })
            continue
        detected_count += 1
        if not injected_truth:
            false_segments += 1
        center_hz = _peak_center_hz(power_spec, freqs, band, active)
        entries.append({
            "mic_name": mic_name,
            "segment_id": action.segment_id,
            "detected": True,
            "injection_expected": injected_truth,
            "start_time": round(max(0.0, action.start_time
                                    - manifest.injection_margin_ms / 1000.0),
                                6),
            "end_time": round(min(len(signal) / sr,
                                  action.end_time
                                  + manifest.injection_margin_ms / 1000.0),
                              6),
            "center_frequency_hz": round(center_hz, 3),
            "estimated_snr_db": round(segment_snr, 3),
        })
    return {
        "mic_name": mic_name,
        "position": position,
        "reference_floor_dbfs": round(floor_db, 3),
        "detected_segments": detected_count,
        "false_positive_segments": false_segments,
        "entries": entries,
    }


def trace_all(signals: dict, manifest: Manifest) -> list:
    results = []
    for mic in manifest.mics:
        results.append(trace_microphone(signals[mic.name], manifest,
                                        mic.name, mic.position))
    return results
