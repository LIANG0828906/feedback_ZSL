"""WAV -> fingerprint feature extraction pipeline.

STFT -> per-band spectral peaks -> peak pairs -> (hash, quantized time).
Frame length and band boundaries are derived from the sample rate so the
same physical parameters apply to any supported layout; the parameters and
their provenance are written into the fingerprint header.
"""

from __future__ import annotations

import numpy as np

from .model import (BAND_EDGES_HZ, EXTRACTOR_ID, FAN_OUT, TARGET_ZONE_FRAMES,
                    TIME_QUANT_MS, Fingerprint, PeakPair, band_edges_bins,
                    frame_length_for, hop_length_for)
from .wavio import read_wav

PEAKS_PER_BAND = 2


def _stft_mag(signal: np.ndarray, frame_length: int, hop_length: int) -> np.ndarray:
    n_frames = 1 + max(0, (len(signal) - frame_length) // hop_length)
    if n_frames <= 0:
        padded = np.zeros(frame_length, dtype=np.float32)
        padded[: len(signal)] = signal
        frames = padded[None, :]
    else:
        idx = np.arange(frame_length)[None, :] + \
              hop_length * np.arange(n_frames)[:, None]
        frames = signal[idx]
    window = np.hanning(frame_length).astype(np.float32)
    return np.abs(np.fft.rfft(frames * window, axis=1))


def _band_peaks(mag: np.ndarray, band_bins: list[int]) -> list[list[tuple[int, int]]]:
    """For each frame, top-PEAKS_PER_BAND peak bins per band.

    Returns per-frame list of (frame, bin) tuples.
    """
    n_frames = mag.shape[0]
    out = []
    for f in range(n_frames):
        row = mag[f]
        peaks = []
        for b in range(len(band_bins) - 1):
            lo, hi = band_bins[b], band_bins[b + 1]
            if hi - lo < 3:
                continue
            seg = row[lo:hi]
            # local maxima above the band mean
            thresh = seg.mean() * 1.5
            cand = []
            for k in range(1, len(seg) - 1):
                if seg[k] > thresh and seg[k] >= seg[k - 1] and seg[k] > seg[k + 1]:
                    cand.append((seg[k], lo + k))
            cand.sort(reverse=True)
            for _, bidx in cand[:PEAKS_PER_BAND]:
                peaks.append((f, bidx))
        out.append(peaks)
    return out


def extract_fingerprint(path: str, sample_id: str) -> Fingerprint:
    signal, sample_rate, channels = read_wav(path)
    frame_length = frame_length_for(sample_rate)
    hop_length = hop_length_for(sample_rate)
    band_bins = band_edges_bins(sample_rate)

    mag = _stft_mag(signal, frame_length, hop_length)
    peaks_per_frame = _band_peaks(mag, band_bins)

    peak_pairs: list[PeakPair] = []
    for f, peaks in enumerate(peaks_per_frame):
        for _, anchor_bin in peaks:
            zone_end = min(len(peaks_per_frame), f + TARGET_ZONE_FRAMES)
            fan = 0
            for g in range(f + 1, zone_end):
                for _, pair_bin in peaks_per_frame[g]:
                    peak_pairs.append(PeakPair(f, anchor_bin, pair_bin, g - f))
                    fan += 1
                    if fan >= FAN_OUT:
                        break
                if fan >= FAN_OUT:
                    break

    duration_ms = len(signal) * 1000.0 / sample_rate
    fp = Fingerprint(
        sample_id=sample_id, sample_rate=sample_rate, channels=channels,
        duration_ms=round(duration_ms, 3), frame_length=frame_length,
        hop_length=hop_length, time_quant_ms=TIME_QUANT_MS,
        peak_pairs=peak_pairs, bands_hz=BAND_EDGES_HZ,
        channel_merge=(channels == 2),
        compression=None,
    )
    fp.hashes = [(p.hash(), fp.quantize_time(p.anchor_frame)) for p in peak_pairs]
    fp.provenance = {
        "extractor": EXTRACTOR_ID,
        "source": "wav-extract",
        "params": {
            "frame_ms": round(frame_length * 1000.0 / sample_rate, 3),
            "hop_ms": round(hop_length * 1000.0 / sample_rate, 3),
            "time_quant_ms": TIME_QUANT_MS,
            "bands_hz": list(BAND_EDGES_HZ),
            "target_zone_frames": TARGET_ZONE_FRAMES,
            "fan_out": FAN_OUT,
            "peaks_per_band": PEAKS_PER_BAND,
        },
    }
    return fp
