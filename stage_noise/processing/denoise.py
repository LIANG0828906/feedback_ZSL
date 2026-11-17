"""Targeted band-stop attenuation of the mechanical noise band.

Only traced action intervals are modified; samples outside every processed
interval are copied through untouched (identical to the input samples) so the
report can be recomputed from the written WAVs.
"""

from __future__ import annotations

import numpy as np

from .audioutil import dbfs, reference_band_floor, segment_band_energy
from ..manifest import Manifest

FILTER_TAPS = 4097
CROSSFADE_MS = 30.0


def bandstop_kernel(band_hz: tuple[float, float], sr: int,
                    taps: int = FILTER_TAPS) -> np.ndarray:
    """Windowed-sinc band-stop FIR (low-pass and high-pass in parallel).

    Each ideal low-pass sinc is windowed separately; the high-pass is the
    difference of two windowed low-pass designs, which keeps the pass-band
    unity and preserves the deep Blackman stop-band attenuation.
    """
    nyq = sr / 2.0
    low, high = band_hz
    width = min(low * 0.6, (nyq - high) * 0.6, 24.0)
    lp_cut = max(low - width, 5.0) / nyq
    hp_cut = min(high + width, nyq - 5.0) / nyq
    half = taps // 2
    n = np.arange(taps) - half
    window = np.blackman(taps)
    low_pass = lp_cut * np.sinc(lp_cut * n) * window
    high_pass = (np.sinc(n) * window
                 - hp_cut * np.sinc(hp_cut * n) * window)
    kernel = low_pass + high_pass
    # Normalize for unity white-noise power gain (flat pass-band); a plain
    # sum() normalization leaves the windowed filter ~+3.9 dB hot.
    kernel /= np.sqrt(np.sum(kernel ** 2))
    return kernel


def fft_convolve(signal: np.ndarray, kernel: np.ndarray) -> np.ndarray:
    n = len(signal) + len(kernel) - 1
    n_fft = int(2 ** np.ceil(np.log2(n)))
    filtered = np.fft.irfft(np.fft.rfft(signal, n_fft)
                            * np.fft.rfft(kernel, n_fft), n_fft)[:n]
    delay = len(kernel) // 2
    return filtered[delay:delay + len(signal)]


def _intervals_for_mic(trace_entry: dict, manifest: Manifest) -> list:
    action_by_id = {a.segment_id: a for a in manifest.actions}
    intervals = []
    for entry in trace_entry["entries"]:
        if not entry["detected"]:
            continue
        action = action_by_id[entry["segment_id"]]
        pad = int(round(manifest.injection_margin_ms / 1000.0
                        * manifest.sample_rate))
        start = max(0, action.start_sample - pad)
        end = min(manifest.total_samples, action.end_sample + pad)
        intervals.append((entry["segment_id"], start, end))
    return intervals


def denoise_microphone(signal: np.ndarray, kernel: np.ndarray,
                       trace_entry: dict, manifest: Manifest) -> dict:
    sr = manifest.sample_rate
    filtered_full = fft_convolve(signal, kernel)
    processed = signal.copy()
    fade_len = int(round(CROSSFADE_MS / 1000.0 * sr))
    intervals = _intervals_for_mic(trace_entry, manifest)
    modified_ranges = []
    for segment_id, start, end in intervals:
        length = end - start
        if length <= 2 * fade_len:
            fade = max(1, length // 4)
        else:
            fade = fade_len
        ramp = np.linspace(0.0, 1.0, fade)
        weights = np.ones(length)
        weights[:fade] = ramp
        weights[-fade:] = ramp[::-1]
        processed[start:end] = (
            signal[start:end] * (1.0 - weights)
            + filtered_full[start:end] * weights)
        modified_ranges.append((segment_id, start, end))

    band = manifest.noise_band_hz
    blocked = [(a.start_sample, a.end_sample) for a in manifest.actions]
    floor_before = reference_band_floor(signal, blocked, band, sr)
    floor_after = reference_band_floor(processed, blocked, band, sr)

    segment_metrics = []
    for action in manifest.actions:
        s, e = action.start_sample, action.end_sample
        before = segment_band_energy(signal, s, e, band, sr)
        after = segment_band_energy(processed, s, e, band, sr)
        segment_metrics.append({
            "segment_id": action.segment_id,
            "noise_band_energy_before_dbfs": round(dbfs(before), 3),
            "noise_band_energy_after_dbfs": round(dbfs(after), 3),
            "band_attenuation_db": round(dbfs(before) - dbfs(after), 3),
        })

    diff = processed - signal
    modified_mask = np.abs(diff) > 0.0
    modified_sample_count = int(np.count_nonzero(modified_mask))
    max_delta = float(np.max(np.abs(diff))) if modified_sample_count else 0.0
    return {
        "mic_name": trace_entry["mic_name"],
        "processed": processed,
        "modified_interval_count": len(modified_ranges),
        "modified_sample_count": modified_sample_count,
        "max_sample_delta": round(max_delta, 8),
        "reference_floor_before_dbfs": round(dbfs(floor_before), 3),
        "reference_floor_after_dbfs": round(dbfs(floor_after), 3),
        "segment_metrics": segment_metrics,
        "modified_ranges": modified_ranges,
    }


def content_band_loss(original: np.ndarray, processed: np.ndarray,
                      manifest: Manifest, content_band=(200.0, 8000.0)) -> dict:
    sr = manifest.sample_rate
    before, after = [], []
    for action in manifest.actions:
        before.append(segment_band_energy(original, action.start_sample,
                                          action.end_sample, content_band, sr))
        after.append(segment_band_energy(processed, action.start_sample,
                                         action.end_sample, content_band, sr))
    before_p = float(np.mean(before))
    after_p = float(np.mean(after))
    return {
        "content_band_hz": list(content_band),
        "content_energy_before_dbfs": round(dbfs(before_p), 3),
        "content_energy_after_dbfs": round(dbfs(after_p), 3),
        "content_loss_db": round(dbfs(before_p) - dbfs(after_p), 3),
    }
