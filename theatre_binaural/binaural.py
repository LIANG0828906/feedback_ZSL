from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass

import numpy as np

from .project import Project, TrackSpec


def _quat_conjugate(q: np.ndarray) -> np.ndarray:
    return np.asarray([q[0], -q[1], -q[2], -q[3]], dtype=np.float64)


def _quat_rotate(q: np.ndarray, vectors: np.ndarray) -> np.ndarray:
    q_vec = q[1:]
    q_w = q[0]
    cross = np.cross(q_vec, vectors)
    return vectors + 2.0 * (q_w * cross + np.cross(q_vec, cross))


def source_direction(
    anchor: np.ndarray,
    head_position: np.ndarray,
    quaternion_wxyz: np.ndarray,
    convention: str,
) -> tuple[float, float, float]:
    relative = np.asarray(anchor, dtype=np.float64) - np.asarray(head_position, dtype=np.float64)
    if convention == "local_to_world":
        local = _quat_rotate(_quat_conjugate(quaternion_wxyz), relative)
    else:
        local = _quat_rotate(quaternion_wxyz, relative)
    distance = float(np.linalg.norm(local))
    if distance <= 1.0e-12:
        return 0.0, 0.0, 0.0
    x, y, z = local
    azimuth = math.atan2(float(x), float(y))
    elevation = math.atan2(float(z), math.hypot(float(x), float(y)))
    return azimuth, elevation, distance


def _slew_limit(series: np.ndarray, times: np.ndarray, max_rate: float) -> np.ndarray:
    if series.size <= 1:
        return series.copy()
    output = series.copy()
    for index in range(1, series.size):
        maximum_step = max_rate * float(times[index] - times[index - 1])
        delta = float(np.clip(series[index] - output[index - 1], -maximum_step, maximum_step))
        output[index] = output[index - 1] + delta
    return output


@dataclass
class SpatialParameters:
    track: TrackSpec
    azimuth_deg: np.ndarray
    elevation_deg: np.ndarray
    left_gain_db: np.ndarray
    right_gain_db: np.ndarray
    left_delay_s: np.ndarray
    right_delay_s: np.ndarray
    ild_db: np.ndarray
    itd_us: np.ndarray
    rate_smoothing_applied: bool


@dataclass
class BinauralState:
    times: np.ndarray
    parameters: dict[str, SpatialParameters]
    rendering_azimuth_at_samples: dict[str, np.ndarray]
    parameter_digest: str
    summary: dict[str, object]


def _head_model(
    azimuth: np.ndarray,
    elevation: np.ndarray,
    distance: np.ndarray,
    project: Project,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    lateral = np.sin(azimuth) * np.cos(elevation)
    max_itd_s = 2.0 * project.head_radius_m / project.speed_of_sound_m_s
    ild = project.max_ild_db * lateral
    itd_s = -max_itd_s * lateral
    distance_gain = 1.0 / (1.0 + 0.12 * np.maximum(distance, 0.0))
    common_gain_db = 20.0 * np.log10(distance_gain)
    left_gain_db = common_gain_db - 0.5 * ild
    right_gain_db = common_gain_db + 0.5 * ild
    common_delay = distance / project.speed_of_sound_m_s + 0.5 * max_itd_s
    left_delay = common_delay - 0.5 * itd_s
    right_delay = common_delay + 0.5 * itd_s
    return (
        left_gain_db,
        right_gain_db,
        left_delay,
        right_delay,
        ild,
        itd_s,
    )


def _source_knot_values(project: Project, track: TrackSpec) -> dict[str, np.ndarray]:
    if track.anchor is None:
        raise ValueError(f"{track.name} has no anchor")
    count = len(project.head_samples)
    azimuth = np.empty(count, dtype=np.float64)
    elevation = np.empty(count, dtype=np.float64)
    distance = np.empty(count, dtype=np.float64)
    for index, sample in enumerate(project.head_samples):
        az, el, dist = source_direction(
            track.anchor,
            sample.position,
            sample.quaternion_wxyz,
            project.orientation_convention,
        )
        azimuth[index] = az
        elevation[index] = el
        distance[index] = dist
    return {
        "azimuth": azimuth,
        "elevation": elevation,
        "distance": distance,
    }


def prepare_binaural_state(project: Project, total_frames: int) -> BinauralState:
    times = np.asarray([sample.time_s for sample in project.head_samples], dtype=np.float64)
    sample_times = np.arange(total_frames, dtype=np.float64) / project.sample_rate
    parameters: dict[str, SpatialParameters] = {}
    rendering_azimuth: dict[str, np.ndarray] = {}
    digest = hashlib.sha256()
    digest.update(np.asarray(times, dtype="<f8").tobytes())
    channel_summaries: list[dict[str, object]] = []

    for track in project.mixed_tracks:
        knots = _source_knot_values(project, track)
        azimuth_unwrapped = np.unwrap(knots["azimuth"])
        azimuth_interp = np.interp(sample_times, times, azimuth_unwrapped)
        elevation_interp = np.interp(sample_times, times, knots["elevation"])
        distance_interp = np.interp(sample_times, times, knots["distance"])

        knot_model = _head_model(
            azimuth_unwrapped,
            knots["elevation"],
            knots["distance"],
            project,
        )
        (
            _left_gain_knot,
            _right_gain_knot,
            _left_delay_knot,
            _right_delay_knot,
            ild_knot,
            itd_s_knot,
        ) = knot_model
        itd_us_knot = itd_s_knot * 1.0e6
        ild_limited = _slew_limit(ild_knot, times, project.max_ild_rate_db_per_s)
        itd_limited = _slew_limit(itd_us_knot, times, project.max_itd_rate_us_per_s)
        smoothing_applied = bool(
            np.any(np.abs(ild_limited - ild_knot) > 1.0e-12)
            or np.any(np.abs(itd_limited - itd_us_knot) > 1.0e-9)
        )

        ild_array = np.interp(sample_times, times, ild_limited)
        itd_us_array = np.interp(sample_times, times, itd_limited)
        common_gain = np.interp(
            sample_times,
            times,
            0.5 * (_left_gain_knot + _right_gain_knot),
        )
        common_delay = np.interp(
            sample_times,
            times,
            0.5 * (_left_delay_knot + _right_delay_knot),
        )
        left_gain_db = common_gain - 0.5 * ild_array
        right_gain_db = common_gain + 0.5 * ild_array
        left_delay_s = common_delay - 0.5 * itd_us_array * 1.0e-6
        right_delay_s = common_delay + 0.5 * itd_us_array * 1.0e-6

        elevation_rad = np.deg2rad(elevation_interp)
        cos_el = np.maximum(np.cos(elevation_rad), 1.0e-6)
        max_itd_s = 2.0 * project.head_radius_m / project.speed_of_sound_m_s
        lateral = np.clip(-itd_us_array * 1.0e-6 / max_itd_s, -1.0, 1.0)
        rendered_azimuth_rad = np.arcsin(np.clip(lateral / cos_el, -1.0, 1.0))
        rendering_azimuth[track.name] = np.rad2deg(rendered_azimuth_rad)

        parameters[track.name] = SpatialParameters(
            track=track,
            azimuth_deg=np.rad2deg(azimuth_interp),
            elevation_deg=elevation_interp,
            left_gain_db=left_gain_db,
            right_gain_db=right_gain_db,
            left_delay_s=left_delay_s,
            right_delay_s=right_delay_s,
            ild_db=ild_array,
            itd_us=itd_us_array,
            rate_smoothing_applied=smoothing_applied,
        )
        digest.update(track.name.encode("utf-8"))
        for array in (
            azimuth_interp,
            elevation_interp,
            left_gain_db,
            right_gain_db,
            left_delay_s,
            right_delay_s,
            ild_array,
            itd_us_array,
        ):
            digest.update(np.asarray(array, dtype="<f8").tobytes())
        channel_summaries.append(
            {
                "channel": track.name,
                "min_ild_db": round(float(np.min(ild_array)), 6),
                "max_ild_db": round(float(np.max(ild_array)), 6),
                "min_itd_us": round(float(np.min(itd_us_array)), 6),
                "max_itd_us": round(float(np.max(itd_us_array)), 6),
                "rate_smoothing_applied": smoothing_applied,
            }
        )

    summary = {
        "algorithm": "analytic_woodworth_ild_itd_linear_interpolation",
        "tracking_samples": int(len(project.head_samples)),
        "sample_rate": project.sample_rate,
        "channels": channel_summaries,
        "parameter_sha256": digest.hexdigest(),
    }
    return BinauralState(
        times=times,
        parameters=parameters,
        rendering_azimuth_at_samples=rendering_azimuth,
        parameter_digest=digest.hexdigest(),
        summary=summary,
    )


def _report_float(value: float) -> float:
    return round(float(value), 9)


def tracking_report(project: Project, state: BinauralState) -> list[dict[str, object]]:
    entries: list[dict[str, object]] = []
    for track_name in sorted(state.parameters):
        params = state.parameters[track_name]
        for index, time_s in enumerate(state.times):
            entries.append(
                {
                    "channel": track_name,
                    "time_s": _report_float(time_s),
                    "azimuth_deg": _report_float(params.azimuth_deg[index]),
                    "elevation_deg": _report_float(params.elevation_deg[index]),
                    "ild_db": _report_float(params.ild_db[index]),
                    "itd_us": _report_float(params.itd_us[index]),
                    "left_gain_db": _report_float(params.left_gain_db[index]),
                    "right_gain_db": _report_float(params.right_gain_db[index]),
                    "left_delay_ms": _report_float(params.left_delay_s[index] * 1000.0),
                    "right_delay_ms": _report_float(params.right_delay_s[index] * 1000.0),
                }
            )
    return entries


def rate_report(project: Project, state: BinauralState) -> dict[str, object]:
    maximum_ild = 0.0
    maximum_itd = 0.0
    for params in state.parameters.values():
        if params.ild_db.size > 1:
            dt = 1.0 / project.sample_rate
            maximum_ild = max(
                maximum_ild,
                float(np.max(np.abs(np.diff(params.ild_db))) / dt),
            )
            maximum_itd = max(
                maximum_itd,
                float(np.max(np.abs(np.diff(params.itd_us))) / dt),
            )
    return {
        "max_ild_rate_db_per_s": _report_float(maximum_ild),
        "ild_rate_limit_db_per_s": project.max_ild_rate_db_per_s,
        "max_itd_rate_us_per_s": _report_float(maximum_itd),
        "itd_rate_limit_us_per_s": project.max_itd_rate_us_per_s,
        "rate_limited": any(
            params.rate_smoothing_applied for params in state.parameters.values()
        ),
    }


def interpolation_report(
    project: Project,
    state: BinauralState,
) -> tuple[list[dict[str, object]], float]:
    checks: list[dict[str, object]] = []
    maximum_error = 0.0
    for check in project.interpolation_checks:
        for track in project.mixed_tracks:
            if check.channel is not None and check.channel != track.name:
                continue
            params = state.parameters[track.name]
            sample_index = int(
                np.clip(
                    round(check.time_s * project.sample_rate),
                    0,
                    params.ild_db.size - 1,
                )
            )
            actual_azimuth = float(
                state.rendering_azimuth_at_samples[track.name][sample_index]
            )
            expected = check.expected_azimuth_deg
            error = (
                abs(actual_azimuth - expected) if expected is not None else None
            )
            if error is not None:
                maximum_error = max(maximum_error, float(error))
            checks.append(
                {
                    "channel": track.name,
                    "time_s": _report_float(check.time_s),
                    "expected_azimuth_deg": (
                        _report_float(expected) if expected is not None else None
                    ),
                    "interpolated_azimuth_deg": _report_float(actual_azimuth),
                    "azimuth_error_deg": _report_float(error) if error is not None else None,
                    "left_gain_db": _report_float(params.left_gain_db[sample_index]),
                    "right_gain_db": _report_float(params.right_gain_db[sample_index]),
                    "left_delay_ms": _report_float(params.left_delay_s[sample_index] * 1000.0),
                    "right_delay_ms": _report_float(params.right_delay_s[sample_index] * 1000.0),
                    "ild_db": _report_float(params.ild_db[sample_index]),
                    "itd_us": _report_float(params.itd_us[sample_index]),
                    "tolerance_deg": project.azimuth_tolerance_deg,
                    "within_tolerance": (
                        bool(error <= project.azimuth_tolerance_deg)
                        if error is not None
                        else None
                    ),
                }
            )
    return checks, maximum_error


def render_source_block(
    source: np.ndarray,
    params: SpatialParameters,
    start: int,
    end: int,
    sample_rate: int,
    weights: tuple[float, float],
) -> np.ndarray:
    sample_numbers = np.arange(start, end, dtype=np.float64)
    output = np.empty((end - start, 2), dtype=np.float64)
    source_positions = np.arange(source.size, dtype=np.float64)
    for ear, delays, gains, weight in (
        (0, params.left_delay_s, params.left_gain_db, weights[0]),
        (1, params.right_delay_s, params.right_gain_db, weights[1]),
    ):
        read_positions = sample_numbers - delays[start:end] * sample_rate
        delayed = np.interp(
            read_positions,
            source_positions,
            source,
            left=0.0,
            right=0.0,
        )
        output[:, ear] = delayed * np.power(10.0, gains[start:end] / 20.0) * weight
    return output


def spatial_parameter_digest(parameters: dict[str, SpatialParameters]) -> str:
    digest = hashlib.sha256()
    for name in sorted(parameters):
        params = parameters[name]
        digest.update(name.encode("utf-8"))
        for array in (
            params.left_gain_db,
            params.right_gain_db,
            params.left_delay_s,
            params.right_delay_s,
        ):
            digest.update(np.asarray(array, dtype="<f8").tobytes())
    return digest.hexdigest()

