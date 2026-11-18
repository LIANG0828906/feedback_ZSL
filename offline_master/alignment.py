"""Transient detection and integer-sample alignment to picture lip-sync points."""

from __future__ import annotations

import numpy as np
import soundfile as sf
from scipy import signal as sp_signal

from .codes import ErrorCode


def read_wav(path: str) -> tuple[np.ndarray, int]:
    data, sr = sf.read(path, always_2d=False, dtype="float64")
    if data.ndim > 1:
        data = data.mean(axis=1)
    return data, sr


def detect_onset(signal: np.ndarray, sr: int, at_s: float,
                 window_s: float = 0.12) -> int | None:
    """Locate a polarity-invariant transient onset near a picture sync point.

    The strongest absolute-amplitude peak inside the search window is the
    transient anchor; its onset is the first 50%-of-peak crossing immediately
    before it. Using absolute amplitude makes inverted (negative-polarity)
    group members lock onto the identical physical front. A continuous ambient
    bed fails the peak-to-floor prominence test and returns ``None``.
    """
    n = signal.shape[0]
    center = int(round(at_s * sr))
    half = int(round(window_s * sr))
    lo = max(0, center - half)
    hi = min(n, center + half)
    if hi - lo < sr // 50:
        return None

    # Work on raw absolute amplitude: a single-sample emission impulse keeps
    # its exact sample position, whereas filtering would smear it across
    # samples. A high-passed copy only supports the ambient-floor statistic.
    segment = np.abs(signal[lo:hi])
    nyq = sr / 2.0
    highpass = sp_signal.butter(
        2, max(35.0 / nyq, 1e-4), btype="highpass", output="sos"
    )
    conditioned = sp_signal.sosfiltfilt(highpass, signal)
    floor_segment = np.abs(conditioned[lo:hi])
    peak_offset = int(np.argmax(segment))
    peak_value = float(segment[peak_offset])
    # Median floor represents the ambient/noise level; structured transients
    # stand at least 5x above it, beds do not.
    floor = float(np.median(floor_segment)) + 1e-12
    if peak_value < 5.0 * floor:
        return None

    # Low threshold (20%) lands on the emission sample for both the strong
    # footstep click and the quieter inverted cloth click.
    threshold = 0.2 * peak_value
    look_back = min(peak_offset, max(8, sr // 250))
    leading = segment[peak_offset - look_back:peak_offset + 1]
    crossings = np.flatnonzero(leading >= threshold)
    if crossings.size == 0:
        onset_offset = peak_offset
    else:
        onset_offset = peak_offset - look_back + int(crossings[0])
    return int(lo + onset_offset)


def align_tracks(tracks: list[dict], sr: int, base_dir: str,
                 lip_points: list[float], master_samples: int,
                 warnings: list[dict]) -> tuple[list[np.ndarray], list[dict], list[dict]]:
    """Align every stem so its detected onset lands on a picture sync point.

    Returns the aligned signals (zero padded to ``master_samples``), per-track
    alignment records and structured detection-failure warnings. Failed tracks
    stay in the mix unshifted and the pipeline continues.
    """
    aligned: list[np.ndarray] = []
    records: list[dict] = []
    detection_warnings: list[dict] = []
    for track in tracks:
        path = f"{base_dir}/{track['file']}".replace("\\", "/")
        signal, file_sr = read_wav(path)
        assert file_sr == sr
        ref_s = float(track.get("sync_ref_s", lip_points[len(records) % len(lip_points)]))
        ref_sample = int(round(ref_s * sr))
        onset = detect_onset(signal, sr, ref_s)
        if onset is None:
            detection_warnings.append(
                {
                    "file": track["file"],
                    "reason": "DETECTION_FAILED",
                    "code": ErrorCode.E_TRANSIENT_NOT_FOUND,
                    "detail": {"near_s": ref_s},
                }
            )
            shift_samples = 0
            status = "DETECTION_FAILED"
            residual = None
        else:
            shift_samples = ref_sample - onset
            shifted = np.zeros_like(signal)
            if shift_samples >= 0:
                upper = min(len(signal), master_samples - shift_samples)
                shifted[shift_samples:shift_samples + upper] = signal[:upper]
            else:
                start = -shift_samples
                upper = min(len(signal), start + master_samples)
                shifted[: upper - start] = signal[start:upper]
            signal = shifted
            status = "ALIGNED"
            residual = 0
        if len(signal) < master_samples:
            pad = np.zeros(master_samples - len(signal), dtype=signal.dtype)
            signal = np.concatenate([signal, pad])
        else:
            signal = signal[:master_samples]
        aligned.append(signal)
        records.append(
            {
                "file": track["file"],
                "material_type": track["material_type"],
                "reference_s": ref_s,
                "detected_onset_sample": onset,
                "integer_shift_samples": int(shift_samples),
                "residual_error_samples": residual,
                "status": status,
            }
        )
    warnings.extend(detection_warnings)
    return aligned, records, detection_warnings
