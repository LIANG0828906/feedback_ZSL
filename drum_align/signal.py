"""Kick detection and sample-accurate local alignment primitives."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np


DETECTION_HOP = 16
DETECTION_FRAME = 384
DETECTION_SMOOTH = 240
DETECTION_DIFF_LAG = 4
DEFAULT_MIN_GAP_SECONDS = 0.080


@dataclass(frozen=True)
class Detection:
    hits: np.ndarray
    onset: np.ndarray
    hop: int = DETECTION_HOP

    @property
    def intervals(self) -> np.ndarray:
        """Adjacent hit distances in samples."""
        return np.diff(self.hits)


def _moving_average(values: np.ndarray, width: int) -> np.ndarray:
    width = max(1, int(width))
    if width == 1:
        return values.copy()
    left = width // 2
    right = width - 1 - left
    padded = np.pad(values, (left, right), mode="edge")
    cumulative = np.concatenate(([0.0], np.cumsum(padded, dtype=np.float64)))
    return (cumulative[width:] - cumulative[:-width]) / float(width)


def kick_envelope(mono: np.ndarray) -> tuple[np.ndarray, int]:
    """Return a kick-biased attack envelope on a fixed 16-sample grid.

    Low-frequency body energy supplies localization. A short high-frequency
    transient gate suppresses the slow decaying lobes of the same kick while
    retaining a genuine closely spaced double-kick attack.
    """
    x = np.asarray(mono, dtype=np.float64)
    if x.size == 0:
        return np.zeros(0, dtype=np.float64), DETECTION_HOP
    x = x - float(np.mean(x))
    smooth = _moving_average(x, DETECTION_SMOOTH)
    energy = smooth * smooth
    envelope_full = _moving_average(energy, DETECTION_FRAME)
    # Keep a compact frame grid. The frame length is centered, so frame i is
    # reported at i * hop + frame/2 in sample coordinates.
    envelope = envelope_full[::DETECTION_HOP]
    if envelope.size < DETECTION_DIFF_LAG + 2:
        return envelope, DETECTION_HOP
    shifted = np.empty_like(envelope)
    shifted[:DETECTION_DIFF_LAG] = envelope[:DETECTION_DIFF_LAG]
    shifted[DETECTION_DIFF_LAG:] = envelope[:-DETECTION_DIFF_LAG]
    low_onset = np.maximum(envelope - shifted, 0.0)

    high = x - smooth
    high_energy = _moving_average(high * high, 64)[::DETECTION_HOP]
    high_shifted = np.empty_like(high_energy)
    high_lag = 2
    high_shifted[:high_lag] = high_energy[:high_lag]
    high_shifted[high_lag:] = high_energy[:-high_lag]
    high_onset = np.maximum(high_energy - high_shifted, 0.0)
    high_peak = float(np.max(high_onset)) if high_onset.size else 0.0
    if high_peak > 0.0:
        gate = 0.18 + 0.82 * (high_onset / high_peak)
    else:
        gate = np.ones_like(low_onset)
    onset = _moving_average(low_onset * gate, 3)
    return onset, DETECTION_HOP


def detect_kicks(
    mono: np.ndarray,
    sample_rate: int,
    *,
    min_required: int = 1,
    min_gap_seconds: float = DEFAULT_MIN_GAP_SECONDS,
) -> Detection:
    """Detect kick attacks deterministically.

    The detector uses a centered 5 ms low-frequency moving average, frame energy,
    and the positive short-term energy difference. Peak picking is global-threshold
    based with a refractory interval. If the strict pass yields too few attacks,
    the threshold is progressively relaxed; exact duplicate frame locations are
    discarded.
    """
    onset, hop = kick_envelope(mono)
    if onset.size < 3:
        return Detection(np.zeros(0, dtype=np.int64), onset, hop)

    positive = onset[onset > 0]
    if positive.size:
        median = float(np.median(positive))
        mad = float(np.median(np.abs(positive - median)))
        strict_floor = median + 8.0 * mad
        top_floor = float(np.max(onset)) * 0.015
        percentile_floor = float(np.percentile(positive, 99.0)) * 0.08
    else:
        strict_floor = top_floor = percentile_floor = float("inf")

    min_gap_frames = max(1, int(round(min_gap_seconds * sample_rate / hop)))
    strongest = float(np.max(onset))
    thresholds = [
        max(strict_floor, top_floor, percentile_floor, strongest * 0.10),
        max(top_floor * 0.35, median + 3.0 * mad, strongest * 0.035),
        max(top_floor * 0.08, median + 1.5 * mad, strongest * 0.012),
        max(top_floor * 0.02, median),
    ]

    best: np.ndarray = np.zeros(0, dtype=np.int64)
    for threshold in thresholds:
        hits = _peaks(onset, threshold=threshold, min_gap_frames=min_gap_frames)
        if hits.size > best.size:
            best = hits
        if best.size >= min_required:
            break

    return Detection(best.astype(np.int64, copy=False), onset, hop)


def _peaks(
    onset: np.ndarray, *, threshold: float, min_gap_frames: int
) -> np.ndarray:
    if not np.isfinite(threshold):
        return np.zeros(0, dtype=np.int64)
    candidates = np.flatnonzero(onset >= threshold)
    if candidates.size == 0:
        return np.zeros(0, dtype=np.int64)

    # Keep local maxima, including plateaus only once.
    local: list[int] = []
    for index in candidates:
        i = int(index)
        left = onset[i - 1] if i > 0 else -1.0
        right = onset[i + 1] if i + 1 < onset.size else -1.0
        if onset[i] >= left and onset[i] > right:
            local.append(i)
    if not local:
        return np.zeros(0, dtype=np.int64)

    ordered = sorted(local, key=lambda i: (-float(onset[i]), i))
    selected: list[int] = []
    for i in ordered:
        if all(abs(i - previous) >= min_gap_frames for previous in selected):
            selected.append(i)
    selected.sort()
    # The envelope window is centered by kick_envelope's moving average.
    return np.asarray(
        [index * DETECTION_HOP + DETECTION_FRAME // 2 for index in selected],
        dtype=np.int64,
    )


def envelope_lag(
    base_onset: np.ndarray,
    aux_onset: np.ndarray,
    *,
    start_frame: int,
    segment_frames: int,
    max_lag_frames: int,
    hop: int,
) -> int:
    """Estimate the integer sample lag d with aux[n + d] ~= base[n]."""
    if base_onset.size == 0 or aux_onset.size == 0:
        return 0
    start = max(0, min(int(start_frame), base_onset.size - 2))
    end = min(base_onset.size, start + max(2, int(segment_frames)))
    base = base_onset[start:end].astype(np.float64)
    aux = aux_onset.astype(np.float64)
    base -= float(np.mean(base))
    aux -= float(np.mean(aux))
    base_norm = float(np.linalg.norm(base))
    aux_norm = float(np.linalg.norm(aux))
    if base_norm == 0.0 or aux_norm == 0.0:
        return 0
    base /= base_norm
    aux /= aux_norm

    fft_size = 1 << (aux.size + base.size - 1).bit_length()
    spectrum = np.fft.rfft(aux, fft_size) * np.conj(np.fft.rfft(base, fft_size))
    correlation = np.fft.irfft(spectrum, fft_size)

    low = max(-int(max_lag_frames), -(base.size - 1))
    high = min(int(max_lag_frames), aux.size - 1)
    if low > high:
        return 0
    lags = np.arange(low, high + 1, dtype=np.int64)
    scores = correlation[lags % fft_size]
    # Penalize partial-overlap lags slightly so a short tail cannot win.
    overlap = np.minimum(
        base.size,
        np.minimum(aux.size - lags, base.size + lags),
    ).astype(np.float64)
    scores = scores * (overlap / max(1.0, base.size))
    best = int(lags[int(np.argmax(scores))])
    return best * hop


def nearest_hit(hits: np.ndarray, target: int, tolerance: int) -> int | None:
    if hits.size == 0:
        return None
    index = int(np.searchsorted(hits, target))
    candidates: list[tuple[int, int]] = []
    if index < hits.size:
        candidates.append((abs(int(hits[index]) - target), int(hits[index])))
    if index > 0:
        candidates.append((abs(int(hits[index - 1]) - target), int(hits[index - 1])))
    if not candidates:
        return None
    distance, value = min(candidates)
    return value if distance <= tolerance else None


def refine_event_offset(
    base_mono: np.ndarray,
    aux_mono: np.ndarray,
    base_hit: int,
    aux_hit: int,
    *,
    search_samples: int = 320,
    window_samples: int = 2_048,
) -> tuple[int, float]:
    """Refine an event offset by local normalized waveform cross-correlation.

    Returns (aux_sample_minus_base_sample, absolute_correlation). A polarity
    inversion is intentionally accepted by taking the absolute correlation.
    """
    half = window_samples // 2
    base_start = base_hit - half
    base_end = base_hit + half
    if base_start < 0 or base_end > base_mono.size:
        return int(aux_hit - base_hit), 0.0
    base = base_mono[base_start:base_end].astype(np.float64)
    base = base - float(np.mean(base))
    base = _moving_average(base, 9)
    base_norm = float(np.linalg.norm(base))
    if base_norm <= 1e-12:
        return int(aux_hit - base_hit), 0.0

    aux_start = aux_hit - half - search_samples
    aux_end = aux_hit + half + search_samples
    if aux_start < 0 or aux_end > aux_mono.size:
        return int(aux_hit - base_hit), 0.0
    aux = aux_mono[aux_start:aux_end].astype(np.float64)
    aux = aux - float(np.mean(aux))
    aux = _moving_average(aux, 9)

    best_shift = 0
    best_corr = 0.0
    length = base.size
    for shift in range(-search_samples, search_samples + 1):
        offset = search_samples + shift
        segment = aux[offset : offset + length]
        norm = float(np.linalg.norm(segment))
        if norm <= 1e-12:
            continue
        correlation = abs(float(np.dot(base, segment)) / (base_norm * norm))
        if correlation > best_corr:
            best_corr = correlation
            best_shift = shift
    return int(aux_hit + best_shift - base_hit), best_corr


def pearson_correlation(a: np.ndarray, b: np.ndarray) -> float:
    left = np.asarray(a, dtype=np.float64)
    right = np.asarray(b, dtype=np.float64)
    if left.size != right.size or left.size < 2:
        return 0.0
    left = left - float(np.mean(left))
    right = right - float(np.mean(right))
    left_norm = float(np.linalg.norm(left))
    right_norm = float(np.linalg.norm(right))
    if left_norm <= 1e-12 or right_norm <= 1e-12:
        return 0.0
    value = float(np.dot(left, right) / (left_norm * right_norm))
    if not np.isfinite(value):
        return 0.0
    return max(-1.0, min(1.0, value))
