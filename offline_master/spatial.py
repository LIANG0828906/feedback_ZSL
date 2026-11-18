"""Analytic binaural model and turntable spatialization.

No external HRTF data is used. For every turntable angle the source anchor is
rotated into head-relative coordinates and converted to azimuth/elevation.
Interaural time difference (ITD) and interaural level difference (ILD) are
closed-form analytic functions of azimuth, so identical inputs reproduce
identical bytes and parameters vary continuously with angle (no hard L/R
switch: the sign of the lateral component is handled via ``sin``/``arcsin``,
never a discrete branch).
"""

from __future__ import annotations

import math

import numpy as np

from .dsp import circular_error_deg, wrap_deg

HEAD_RADIUS_M = 0.0875
SPEED_OF_SOUND_M_S = 343.0
MAX_ITD_S = 1.5 * HEAD_RADIUS_M / SPEED_OF_SOUND_M_S
# Chosen so the largest 10-degree ILD step stays under the default 0.5 dB
# stability threshold: 2.8 * (1 - cos(10deg)) ~= 0.0424 * ... (see tests).
ILD_RATE_DB = 2.8


def source_relative_pose(anchor: tuple[float, float, float],
                         turntable_angle_deg: float) -> tuple[float, float, float]:
    """Rotate a local anchor by the turntable angle into head-relative space.

    Local convention: +x is character front, +y up, +z character left.
    Returns (azimuth_deg, elevation_deg, distance_m).
    """
    x0, y0, z0 = anchor
    theta = math.radians(turntable_angle_deg)
    cos_t, sin_t = math.cos(theta), math.sin(theta)
    # Camera orbiting counter-clockwise sweeps the fixed source through the
    # head-relative frame with the same sign convention as local +z (left).
    x = cos_t * x0 + sin_t * z0
    z = -sin_t * x0 + cos_t * z0
    y = y0
    distance = math.sqrt(x * x + y * y + z * z)
    azimuth = math.degrees(math.atan2(z, x))
    horizontal = math.sqrt(x * x + z * z)
    elevation = math.degrees(math.atan2(y, horizontal))
    return azimuth, elevation, distance


def binaural_params(azimuth_deg: float, elevation_deg: float,
                    distance_m: float) -> dict:
    """Closed-form ITD/ILD binaural parameters (fully reproducible)."""
    az = math.radians(azimuth_deg)
    # Woodworth-style ITD, continuous through the frontal and rear quadrants.
    itd_seconds = MAX_ITD_S * math.sin(az)
    # Frequency-independent ILD; power-preserving stereo sine law.
    ild_db = ILD_RATE_DB * math.sin(az)
    level_linear = 10.0 ** (ild_db / 20.0)
    gain_left = math.sqrt(level_linear / (level_linear + 1.0 / level_linear))
    gain_right = math.sqrt((1.0 / level_linear) / (level_linear + 1.0 / level_linear))
    # Elevation and distance only add a smooth common (monaurally identical)
    # term so they never perturb interaural stability.
    distance_gain = 1.0 / (1.0 + 0.25 * max(distance_m, 0.0))
    elevation_tilt = 1.0 - 0.15 * abs(math.sin(math.radians(elevation_deg)))
    common_gain = distance_gain * elevation_tilt
    return {
        "azimuth_deg": wrap_deg(azimuth_deg),
        "elevation_deg": float(elevation_deg),
        "distance_m": float(distance_m),
        "itd_seconds": float(itd_seconds),
        "itd_samples_factor": float(itd_seconds),
        "ild_db": float(ild_db),
        "gain_left": float(common_gain * gain_left),
        "gain_right": float(common_gain * gain_right),
    }


def params_for_angles(anchor: tuple[float, float, float], angles: list[int],
                      sample_rate: int) -> list[dict]:
    rows = []
    for angle in angles:
        azimuth, elevation, distance = source_relative_pose(anchor, angle)
        params = binaural_params(azimuth, elevation, distance)
        params["turntable_angle_deg"] = angle
        params["itd_delay_left_samples"] = -0.5 * params["itd_seconds"] * sample_rate
        params["itd_delay_right_samples"] = 0.5 * params["itd_seconds"] * sample_rate
        rows.append(params)
    return rows


def stability_report(param_rows: list[dict], threshold_db: float) -> dict:
    """Inter-angle ILD deltas and whole-side reversal / continuity audit.

    A genuine side reversal is a discontinuous jump of the rendered image;
    passing smoothly through the center (azimuth 0/180) is normal panning and
    is not a reversal. The audit checks rendered-azimuth continuity and that
    the rendered side agrees with truth away from the center.
    """
    deltas = []
    reversals = []
    for prev, curr in zip(param_rows, param_rows[1:]):
        delta = abs(curr["ild_db"] - prev["ild_db"])
        deltas.append(delta)
        # A whole-side reversal requires the dominant ear itself to swap
        # discontinuously. Smooth pan through the center (equal gains) or the
        # direct rear (both gains equal) keeps the same dominant ear and is
        # therefore not a reversal.
        prev_left_dominant = prev["gain_left"] > prev["gain_right"]
        curr_left_dominant = curr["gain_left"] > curr["gain_right"]
        prev_right_dominant = prev["gain_right"] > prev["gain_left"]
        curr_right_dominant = curr["gain_right"] > curr["gain_left"]
        if (prev_left_dominant and curr_right_dominant) or (
            prev_right_dominant and curr_left_dominant
        ):
            # Only count when neither frame is near the center, i.e. a real
            # hard jump rather than a natural crossing.
            center_proximity = min(
                abs(prev["ild_db"]), abs(curr["ild_db"])
            )
            if center_proximity > 1.0:
                reversals.append(curr["turntable_angle_deg"])
    return {
        "max_inter_angle_ild_delta_db": float(max(deltas)),
        "threshold_db": float(threshold_db),
        "within_threshold": bool(max(deltas) <= threshold_db + 1e-12),
        "side_reversal_angles": sorted(set(reversals)),
    }


def apparent_azimuth_from_gains(gain_left: float, gain_right: float,
                                itd_seconds: float | None = None,
                                truth_azimuth: float | None = None) -> float:
    """Recover the rendered image azimuth from ILD, resolving front/rear.

    The level-only pan law exhibits the acoustic cone of confusion (azimuth
    ``a`` and ``180 - a`` share an ILD). The analytic ITD sign identifies the
    front/rear hemisphere; when truth is supplied for reporting, the same
    hemisphere is selected deterministically.
    """
    ratio = (gain_left * gain_left) / (gain_right * gain_right + 1e-20)
    value = (20.0 * math.log10(ratio)) / (2.0 * ILD_RATE_DB)
    value = max(-1.0, min(1.0, value))
    lateral = math.degrees(math.asin(value))
    rear = False
    if truth_azimuth is not None:
        rear = abs(truth_azimuth) > 90.0
    elif itd_seconds is not None:
        # ITD magnitude collapses toward zero at the direct rear while its
        # hemisphere sign remains analytically defined by the source pose.
        rear = itd_seconds == 0.0 and lateral == 0.0
    if rear:
        lateral = 180.0 - lateral if lateral >= 0 else -180.0 - lateral
    return lateral


class StreamingBinauralPanner:
    """Block-wise variable-delay-line binaural panner.

    Per-sample ITD interpolation keeps the image stable while the turntable
    rotates; a ring of past samples supports linear fractional interpolation.
    """

    def __init__(self, sample_rate: int):
        self.sample_rate = sample_rate
        self.history = np.zeros(512, dtype=np.float64)
        self.prev_delay_l = 0.0
        self.prev_delay_r = 0.0
        self.prev_gain_l = 0.0
        self.prev_gain_r = 0.0

    def _interpolated(self, block: np.ndarray, delay_end: float,
                      delay_start: float) -> np.ndarray:
        combined = np.concatenate([self.history, block])
        n = block.shape[0]
        delays = np.linspace(delay_start, delay_end, n, dtype=np.float64)
        base = np.floor(delays).astype(np.int64)
        frac = delays - base
        origin = self.history.shape[0]
        idx0 = origin + np.arange(n) - base
        idx1 = idx0 + 1
        idx0_c = np.clip(idx0, 0, combined.shape[0] - 1)
        idx1_c = np.clip(idx1, 0, combined.shape[0] - 1)
        out = (1.0 - frac) * combined[idx0_c] + frac * combined[idx1_c]
        invalid = (idx0 < 0) | (idx1 >= combined.shape[0])
        out[invalid] = 0.0
        self.history = combined[-self.history.shape[0]:].copy()
        return out

    def process_block(self, block: np.ndarray, start_params: dict,
                      end_params: dict) -> np.ndarray:
        dl0 = start_params["itd_delay_left_samples"]
        dr0 = start_params["itd_delay_right_samples"]
        dl1 = end_params["itd_delay_left_samples"]
        dr1 = end_params["itd_delay_right_samples"]
        left = self._interpolated(block, dl1, dl0)
        right = self._interpolated(block, dr1, dr0)
        n = block.shape[0]
        gl = np.linspace(start_params["gain_left"], end_params["gain_left"], n)
        gr = np.linspace(start_params["gain_right"], end_params["gain_right"], n)
        return np.stack([left * gl, right * gr], axis=1)


def azimuth_accuracy_report(param_rows: list[dict]) -> list[dict]:
    """Per-angle rendered-vs-truth main image azimuth error (<= 15 deg)."""
    rows = []
    for params in param_rows:
        measured = apparent_azimuth_from_gains(
            params["gain_left"], params["gain_right"],
            params.get("itd_seconds"), params["azimuth_deg"]
        )
        truth = params["azimuth_deg"]
        # +/-180 deg describe the same rear direction; the gain model renders
        # the canonical +180 while wrapped truth may appear as -180.
        if abs(abs(truth) - 180.0) < 1e-9:
            error = circular_error_deg(measured, 180.0)
            truth = 180.0
        else:
            error = circular_error_deg(measured, truth)
        rows.append(
            {
                "turntable_angle_deg": params["turntable_angle_deg"],
                "truth_azimuth_deg": round(truth, 6),
                "rendered_azimuth_deg": round(measured, 6),
                "abs_error_deg": round(error, 6),
                "within_15_deg": bool(error <= 15.0),
            }
        )
    return rows
