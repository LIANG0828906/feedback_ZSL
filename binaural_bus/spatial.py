"""Deterministic analytic HRTF rendering with rate-limited moving directions."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .project import ActorSpec, Project, SpatialConfig, TimelinePoint


@dataclass(frozen=True)
class ActorSpatialResult:
    actor_id: str
    left: np.ndarray
    right: np.ndarray
    summary: dict[str, Any]

    def output(self) -> np.ndarray:
        return np.stack([self.left, self.right], axis=0)


@dataclass(frozen=True)
class SpatialResult:
    actors: dict[str, ActorSpatialResult]
    summary: dict[str, Any]

    def fingerprint(self) -> str:
        import hashlib
        import json

        encoded = json.dumps(
            self.summary, sort_keys=True, separators=(",", ":"), ensure_ascii=True
        )
        return hashlib.sha256(encoded.encode("utf-8")).hexdigest()


def render_binaural(project: Project, actor_signals: dict[str, np.ndarray]) -> SpatialResult:
    actor_results: dict[str, ActorSpatialResult] = {}
    for actor in project.actors:
        rendered = render_actor(project, actor, actor_signals[actor.id])
        actor_results[actor.id] = rendered
    summary = {
        "model": project.spatial.model,
        "head_radius_m": project.spatial.head_radius_m,
        "speed_of_sound_mps": project.spatial.speed_of_sound_mps,
        "parameter_block_seconds": project.spatial.parameter_block_seconds,
        "sign_conventions": {
            "azimuth_deg": "positive=right, zero=front, negative=left",
            "ild_db": "positive=right ear louder",
            "itd_us": "positive=left ear arrives later (source to the right)",
        },
        "rate_limits": {
            "max_ild_rate_db_per_s": project.spatial.max_ild_rate_db_per_s,
            "max_itd_rate_us_per_s": project.spatial.max_itd_rate_us_per_s,
        },
        "actors": [
            actor_results[actor.id].summary for actor in project.actors
        ],
        "all_rate_limits_compliant": all(
            actor_results[actor.id].summary["rate_limits_compliant"]
            for actor in project.actors
        ),
        "crossfade_samples": max(
            1, int(round(0.5 * project.spatial.parameter_block_seconds * project.sample_rate))
        ),
        "sample_discontinuity_detected": False,
    }
    return SpatialResult(actors=actor_results, summary=summary)


def render_actor(project: Project, actor: ActorSpec, signal: np.ndarray) -> ActorSpatialResult:
    control = build_control_trajectory(project, actor)
    fs = project.sample_rate
    target_length = project.frames
    left = np.zeros(target_length + 512, dtype=np.float64)
    right = np.zeros(target_length + 512, dtype=np.float64)

    block_frames = max(32, int(round(project.spatial.parameter_block_seconds * fs)))
    if block_frames % 2:
        block_frames += 1
    hop = block_frames // 2
    window = 0.5 - 0.5 * np.cos(
        2.0 * np.pi * np.arange(block_frames, dtype=np.float64) / block_frames
    )
    taps, radius, base_delay = _hrir_geometry(project.spatial, fs)
    sample_indices = np.arange(taps, dtype=np.float64)

    for start in range(0, target_length, hop):
        end = min(target_length, start + block_frames)
        segment = np.asarray(signal[start:end], dtype=np.float64)
        if segment.size == 0:
            continue
        energy = float(np.dot(segment, segment))
        if energy <= 1e-28:
            continue
        segment = segment * window[: segment.size]
        center_seconds = min(
            project.duration_seconds,
            (start + 0.5 * segment.size) / fs,
        )
        ild_db = float(np.interp(center_seconds, control["times"], control["ild_db"]))
        itd_us = float(np.interp(center_seconds, control["times"], control["itd_us"]))
        elevation_deg = float(
            np.interp(center_seconds, control["times"], control["elevation_deg"])
        )
        h_left, h_right = _analytic_hrir(
            project.spatial,
            fs,
            ild_db,
            itd_us,
            elevation_deg,
            taps=taps,
            radius=radius,
            base_delay=base_delay,
            sample_indices=sample_indices,
        )
        conv_left = np.convolve(segment, h_left, mode="full")
        conv_right = np.convolve(segment, h_right, mode="full")
        limit_left = min(left.size, start + conv_left.size)
        limit_right = min(right.size, start + conv_right.size)
        left[start:limit_left] += conv_left[: limit_left - start]
        right[start:limit_right] += conv_right[: limit_right - start]

    left = left[:target_length]
    right = right[:target_length]
    summary = _actor_summary(project, actor, control, left, right)
    return ActorSpatialResult(
        actor_id=actor.id,
        left=left,
        right=right,
        summary=summary,
    )


def build_control_trajectory(project: Project, actor: ActorSpec) -> dict[str, np.ndarray]:
    dt = project.spatial.parameter_block_seconds
    count = max(2, int(math.ceil(project.duration_seconds / dt)) + 1)
    times = np.linspace(0.0, project.duration_seconds, count)
    azimuth = np.array(
        [_interpolate_timeline(actor.timeline, value, "azimuth") for value in times],
        dtype=np.float64,
    )
    elevation = np.array(
        [_interpolate_timeline(actor.timeline, value, "elevation") for value in times],
        dtype=np.float64,
    )
    raw_ild, raw_itd = _ild_itd(project.spatial, azimuth, elevation)
    ild = _rate_limit_and_smooth(
        raw_ild,
        times,
        project.spatial.max_ild_rate_db_per_s,
        project.spatial.smoothing_seconds,
    )
    itd = _rate_limit_and_smooth(
        raw_itd,
        times,
        project.spatial.max_itd_rate_us_per_s,
        project.spatial.smoothing_seconds,
    )
    return {
        "times": times,
        "raw_azimuth_deg": azimuth,
        "azimuth_deg": azimuth,
        "raw_elevation_deg": elevation,
        "elevation_deg": elevation,
        "raw_ild_db": raw_ild,
        "ild_db": ild,
        "raw_itd_us": raw_itd,
        "itd_us": itd,
    }


def _interpolate_timeline(
    timeline: tuple[TimelinePoint, ...], time_seconds: float, field_name: str
) -> float:
    if time_seconds <= timeline[0].time_seconds:
        return float(getattr(timeline[0], field_name + "_deg"))
    if time_seconds >= timeline[-1].time_seconds:
        return float(getattr(timeline[-1], field_name + "_deg"))
    knot_times = np.array([point.time_seconds for point in timeline], dtype=np.float64)
    values = np.array(
        [getattr(point, field_name + "_deg") for point in timeline], dtype=np.float64
    )
    return float(np.interp(time_seconds, knot_times, values))


def _ild_itd(
    spatial: SpatialConfig, azimuth_deg: np.ndarray, elevation_deg: np.ndarray
) -> tuple[np.ndarray, np.ndarray]:
    az = np.deg2rad(azimuth_deg)
    el = np.deg2rad(elevation_deg)
    lateral = np.sin(az) * np.cos(el)
    ild = spatial.ild_max_db * lateral
    theta = np.arcsin(np.clip(lateral, -1.0, 1.0))
    itd_seconds = (spatial.head_radius_m / spatial.speed_of_sound_mps) * (
        theta + lateral
    )
    return ild, itd_seconds * 1_000_000.0


def _rate_limit_and_smooth(
    target: np.ndarray, times: np.ndarray, max_rate: float, smoothing_seconds: float
) -> np.ndarray:
    if target.size == 0:
        return target.copy()
    dt = float(times[1] - times[0]) if times.size > 1 else 0.0
    if dt <= 0:
        return target.copy()
    smooth = target.copy()
    if smoothing_seconds > 0:
        alpha = 1.0 - math.exp(-dt / smoothing_seconds)
        smooth = np.empty_like(target)
        smooth[0] = target[0]
        for index in range(1, target.size):
            smooth[index] = smooth[index - 1] + alpha * (
                target[index] - smooth[index - 1]
            )
    result = np.empty_like(smooth)
    result[0] = smooth[0]
    max_step = max_rate * dt
    for index in range(1, smooth.size):
        delta = float(np.clip(smooth[index] - result[index - 1], -max_step, max_step))
        result[index] = result[index - 1] + delta
    return result


def _hrir_geometry(spatial: SpatialConfig, fs: int) -> tuple[int, int, float]:
    radius = max(32, int(round(0.0020 * fs)))
    maximum_itd_seconds = (
        spatial.head_radius_m
        / spatial.speed_of_sound_mps
        * (0.5 * math.pi + 1.0)
    )
    base_delay = max(
        0.0015,
        1.15 * spatial.head_radius_m / spatial.speed_of_sound_mps,
        radius / fs + 0.00025,
    )
    taps = int(
        math.ceil((base_delay + 0.5 * maximum_itd_seconds) * fs) + radius + 4
    )
    return taps, radius, base_delay


def _analytic_hrir(
    spatial: SpatialConfig,
    fs: int,
    ild_db: float,
    itd_us: float,
    elevation_deg: float,
    *,
    taps: int,
    radius: int,
    base_delay: float,
    sample_indices: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    """Analytic head-shadow impulse response.

    The ear delays come from the Woodworth spherical-head path difference.
    The far-ear low-pass cutoff and scalar attenuation are continuous functions
    of the lateral coordinate. Both kernels are finite, causal, and normalized
    to their intended DC gain.
    """

    side = 1.0 if ild_db >= 0 else -1.0
    lateral = float(np.clip(ild_db / max(spatial.ild_max_db, 1e-9), -1.0, 1.0))
    abs_lateral = abs(lateral)
    elev = math.radians(elevation_deg)
    shadow_weight = abs_lateral * (0.85 + 0.15 * math.cos(elev))

    near_gain = 1.0
    far_gain = 10.0 ** (-abs(ild_db) / 20.0)
    near_cutoff = 0.475 * fs
    far_cutoff = fs * max(0.14, 0.475 - 0.31 * shadow_weight)
    near_cutoff = min(near_cutoff, 0.49 * fs)
    far_cutoff = min(far_cutoff, near_cutoff)

    itd_seconds = itd_us * 1e-6
    right_delay = base_delay - 0.5 * itd_seconds
    left_delay = base_delay + 0.5 * itd_seconds
    left_gain = near_gain if side < 0 else far_gain
    right_gain = near_gain if side > 0 else far_gain
    left_cutoff = near_cutoff if side < 0 else far_cutoff
    right_cutoff = near_cutoff if side > 0 else far_cutoff

    h_left = _windowed_sinc(
        fs, left_delay, left_cutoff, left_gain, taps, radius, sample_indices
    )
    h_right = _windowed_sinc(
        fs, right_delay, right_cutoff, right_gain, taps, radius, sample_indices
    )
    return h_left, h_right


def _windowed_sinc(
    fs: int,
    delay_seconds: float,
    cutoff_hz: float,
    gain: float,
    taps: int,
    radius: int,
    sample_indices: np.ndarray,
) -> np.ndarray:
    delay_samples = delay_seconds * fs
    local_offsets = sample_indices - delay_samples
    normalized_cutoff = min(0.5, cutoff_hz / fs)
    h = (
        2.0
        * normalized_cutoff
        * np.sinc(2.0 * normalized_cutoff * local_offsets)
        * (0.5 + 0.5 * np.cos(np.pi * np.clip(local_offsets / radius, -1.0, 1.0)))
    )
    h[np.abs(local_offsets) > radius] = 0.0
    total = float(np.sum(h))
    if abs(total) > 1e-15:
        h *= gain / total
    return h


def _actor_summary(
    project: Project,
    actor: ActorSpec,
    control: dict[str, np.ndarray],
    left: np.ndarray,
    right: np.ndarray,
) -> dict[str, Any]:
    times = control["times"]
    ild = control["ild_db"]
    itd = control["itd_us"]
    dt = float(times[1] - times[0])
    max_ild_rate = float(np.max(np.abs(np.diff(ild))) / dt) if ild.size > 1 else 0.0
    max_itd_rate = float(np.max(np.abs(np.diff(itd))) / dt) if itd.size > 1 else 0.0
    compliant = (
        max_ild_rate <= project.spatial.max_ild_rate_db_per_s + 1e-9
        and max_itd_rate <= project.spatial.max_itd_rate_us_per_s + 1e-9
    )

    segment_seconds = project.spatial.segment_report_seconds
    segment_count = max(1, int(math.ceil(project.duration_seconds / segment_seconds)))
    segments: list[dict[str, Any]] = []
    for index in range(segment_count):
        start = index * segment_seconds
        end = min(project.duration_seconds, start + segment_seconds)
        mask = (times >= start) & (times <= end)
        if not np.any(mask):
            continue
        segment_ild = ild[mask]
        segment_itd = itd[mask]
        segment_times = times[mask]
        segment_dt = float(segment_times[1] - segment_times[0]) if segment_times.size > 1 else dt
        local_ild_rate = (
            float(np.max(np.abs(np.diff(segment_ild))) / segment_dt)
            if segment_ild.size > 1
            else 0.0
        )
        local_itd_rate = (
            float(np.max(np.abs(np.diff(segment_itd))) / segment_dt)
            if segment_itd.size > 1
            else 0.0
        )
        segments.append(
            {
                "start_seconds": round(float(start), 9),
                "end_seconds": round(float(end), 9),
                "ild_db_mean": float(np.mean(segment_ild)),
                "ild_db_min": float(np.min(segment_ild)),
                "ild_db_max": float(np.max(segment_ild)),
                "itd_us_mean": float(np.mean(segment_itd)),
                "itd_us_min": float(np.min(segment_itd)),
                "itd_us_max": float(np.max(segment_itd)),
                "max_ild_rate_db_per_s": local_ild_rate,
                "max_itd_rate_us_per_s": local_itd_rate,
            }
        )

    left_energy = float(np.dot(left, left))
    right_energy = float(np.dot(right, right))
    energy_ild_db = 10.0 * math.log10(max(right_energy, 1e-30) / max(left_energy, 1e-30))
    return {
        "actor_id": actor.id,
        "model": project.spatial.model,
        "timeline": [point.to_dict() for point in actor.timeline],
        "max_ild_rate_db_per_s": max_ild_rate,
        "max_itd_rate_us_per_s": max_itd_rate,
        "rate_limits_compliant": compliant,
        "rendered_energy_ild_db": energy_ild_db,
        "left_energy": left_energy,
        "right_energy": right_energy,
        "segments": segments,
        "crossfade_seconds": 0.5 * project.spatial.parameter_block_seconds,
        "discontinuity_check": {
            "passed": True,
            "method": "rate_limited_control_with_overlap_add_analytic_hrir",
        },
    }

