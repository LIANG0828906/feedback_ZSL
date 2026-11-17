"""Sparse applause event detection and cross-microphone interval fusion."""

from __future__ import annotations

import numpy as np
from scipy.signal import butter, sosfiltfilt

FRAME_MS = 5.0
# Individual claps arrive every 40-90 ms, so gaps up to 100 ms inside an
# applause burst must be bridged; unrelated events are separated by >= 900 ms.
MERGE_GAP_MS = 100.0
MIN_EVENT_MS = 150.0


def _highpass(signal, sample_rate, cutoff_hz=500.0):
    sos = butter(4, cutoff_hz / (sample_rate / 2.0), btype="highpass", output="sos")
    pad = min(len(signal) - 1, 3 * max(len(coef) for coef in sos))
    return sosfiltfilt(sos, signal, padlen=max(pad, 0))


def _frame_activity(signal, sample_rate, threshold):
    """Per-frame broadband energy above the project threshold (boolean)."""
    frame_len = max(1, int(round(sample_rate * FRAME_MS / 1000.0)))
    usable = (len(signal) // frame_len) * frame_len
    frames = signal[:usable].reshape(-1, frame_len)
    rms = np.sqrt(np.mean(frames * frames, axis=1) + 1e-20)
    active = rms > threshold
    return active, frame_len


def _runs_to_intervals(active, frame_len, sample_rate):
    intervals = []
    if not active.any():
        return intervals
    padded = np.concatenate(([False], active, [False]))
    edges = np.flatnonzero(padded[1:] != padded[:-1])
    for start_frame, end_frame in edges.reshape(-1, 2):
        intervals.append((
            start_frame * frame_len / sample_rate,
            end_frame * frame_len / sample_rate,
        ))
    return intervals


def _merge(intervals, gap_seconds, min_duration):
    if not intervals:
        return []
    intervals = sorted(intervals)
    merged = [list(intervals[0])]
    for start, end in intervals[1:]:
        if start - merged[-1][1] <= gap_seconds:
            merged[-1][1] = max(merged[-1][1], end)
        else:
            merged.append([start, end])
    return [tuple(pair) for pair in merged if pair[1] - pair[0] >= min_duration]


def detect_events(mic_signals, sample_rate, threshold):
    """Detect global applause events from a dict of mic name -> mono signal.

    A 500 Hz high-pass removes low-frequency footstep thumps; frame energy is
    thresholded per microphone, runs are merged, then cross-mic overlaps are
    fused into one global interval list. All operations are deterministic.
    """
    gap = MERGE_GAP_MS / 1000.0
    min_duration = MIN_EVENT_MS / 1000.0
    per_mic = []
    for name in sorted(mic_signals):
        filtered = _highpass(mic_signals[name], sample_rate)
        active, frame_len = _frame_activity(filtered, sample_rate, threshold)
        intervals = _merge(_runs_to_intervals(active, frame_len, sample_rate), gap, min_duration)
        per_mic.append((name, intervals))

    union = []
    for _, intervals in per_mic:
        union.extend(intervals)
    global_intervals = _merge(union, gap, min_duration)

    events = []
    for index, (start, end) in enumerate(global_intervals):
        # Intervals already live on the frame lattice; round for JSON stability.
        start_q = round(start, 4)
        end_q = round(end, 4)
        events.append({
            "event_index": index,
            "start": start_q,
            "end": end_q,
            "duration": round(end_q - start_q, 4),
        })
    return events
