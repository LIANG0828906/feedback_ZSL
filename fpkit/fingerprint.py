"""Deterministic STFT peak-pair fingerprinting.

The fingerprint is a pure function of (pcm signal, sample rate, config):
same input + same config always yields the same records and content hash,
independent of the degradation stage used for internal buffering.
"""

import hashlib
import json

import numpy as np


def _canonical(obj):
    return json.dumps(obj, sort_keys=True, separators=(",", ":"))


def hash_pair(f1, f2, dt, band1, band2):
    """Peak-pair hash carrying the band identity."""
    payload = f"{f1}|{f2}|{dt}|{band1}|{band2}".encode()
    return hashlib.sha256(payload).hexdigest()[:16]


def content_hash(records):
    return hashlib.sha256(_canonical(records).encode()).hexdigest()


def frame_count(n_samples, frame_size, hop):
    if n_samples <= 0:
        return 0
    if n_samples <= frame_size:
        return 1
    return 1 + (n_samples - frame_size) // hop


def stft_magnitudes(signal, frame_size, hop, frame_batch):
    """Deterministic STFT magnitude matrix, computed in frame batches."""
    n = len(signal)
    n_frames = frame_count(n, frame_size, hop)
    if n_frames == 0:
        return np.zeros((0, frame_size // 2 + 1))
    window = np.hanning(frame_size)
    out = []
    for start in range(0, n_frames, frame_batch):
        stop = min(start + frame_batch, n_frames)
        batch = np.zeros((stop - start, frame_size))
        for row, fr in enumerate(range(start, stop)):
            off = fr * hop
            seg = signal[off:off + frame_size]
            batch[row, :len(seg)] = seg
        out.append(np.abs(np.fft.rfft(batch * window, axis=1)))
    return np.concatenate(out, axis=0)


def band_bins(fp_cfg, rate):
    frame_size = fp_cfg["frame_size"]
    bins = []
    for lo_hz, hi_hz in fp_cfg["bands"]:
        lo = int(round(lo_hz * frame_size / rate))
        hi = int(round(hi_hz * frame_size / rate))
        bins.append((lo, hi))
    return bins


def pick_peaks(mag_row, bins, peaks_per_band):
    """Top local maxima per band; deterministic tie-break by lower bin."""
    frame_max = mag_row.max()
    if frame_max <= 0.0:
        return []
    thresh = 0.05 * frame_max
    peaks = []
    n_bins = len(mag_row)
    for band_id, (lo, hi) in enumerate(bins):
        lo = max(lo, 1)
        hi = min(hi, n_bins - 2)
        if hi <= lo:
            continue
        seg = mag_row[lo:hi + 1]
        local = np.where((seg[1:-1] > seg[:-2]) & (seg[1:-1] >= seg[2:]))[0] + 1 + lo
        cands = [(float(mag_row[k]), int(k)) for k in local if mag_row[k] >= thresh]
        cands.sort(key=lambda km: (-km[0], km[1]))
        for _, k in cands[:peaks_per_band]:
            peaks.append((k, band_id))
    return peaks


def fingerprint_signal(signal, rate, fp_cfg, frame_batch=None, hash_chunk=None):
    """Extract (records, stats). records: list of [pair_hash, t_offset, band].

    `frame_batch` / `hash_chunk` are degradation-stage buffering knobs and
    do not affect the produced records.
    """
    frame_size = fp_cfg["frame_size"]
    hop = fp_cfg["hop_size"]
    if frame_batch is None:
        frame_batch = fp_cfg["frame_batch"]
    mag = stft_magnitudes(signal, frame_size, hop, frame_batch)
    bins = band_bins(fp_cfg, rate)
    peaks = []
    for t in range(mag.shape[0]):
        for f, band_id in pick_peaks(mag[t], bins, fp_cfg["peaks_per_band"]):
            peaks.append((t, f, band_id))
    fan_out = fp_cfg["fan_out"]
    dt_min = fp_cfg["target_dt_min"]
    dt_max = fp_cfg["target_dt_max"]
    records = []
    n = len(peaks)
    for i in range(n):
        t1, f1, b1 = peaks[i]
        taken = 0
        j = i + 1
        while j < n and taken < fan_out:
            t2, f2, b2 = peaks[j]
            dt = t2 - t1
            if dt > dt_max:
                break
            if dt >= dt_min:
                records.append([hash_pair(f1, f2, dt, b1, b2), t1, b1])
                taken += 1
            j += 1
    if hash_chunk:
        parts = [records[k:k + hash_chunk] for k in range(0, len(records), hash_chunk)]
        flat = [rec for part in parts for rec in part]
        assert flat == records
    stats = {
        "n_frames": int(mag.shape[0]),
        "n_peaks": len(peaks),
        "n_hashes": len(records),
    }
    return records, stats


def diff_summary(records_a, records_b):
    """新旧指纹差异摘要 (e.g. after resampling / window change)."""
    set_a = {tuple(r) for r in records_a}
    set_b = {tuple(r) for r in records_b}
    only_a = sorted(set_a - set_b)
    only_b = sorted(set_b - set_a)
    return {
        "hash_a": content_hash(records_a),
        "hash_b": content_hash(records_b),
        "records_a": len(records_a),
        "records_b": len(records_b),
        "common": len(set_a & set_b),
        "only_in_a": len(only_a),
        "only_in_b": len(only_b),
        "sample_only_in_a": [list(r) for r in only_a[:5]],
        "sample_only_in_b": [list(r) for r in only_b[:5]],
    }
