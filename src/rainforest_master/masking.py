from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from typing import Any

import numpy as np
import soundfile as sf

from .manifest import Manifest, pan_gains


@dataclass(frozen=True)
class DuckWindow:
    target_scene: str
    event_time_seconds: float
    event_sample: int
    start: int
    end: int
    core_gain: float
    attack_frames: int
    release_frames: int


@dataclass
class MaskingPlan:
    reference_scene: str
    residual_energy_ratio: float
    core_gain: float
    windows: list[DuckWindow]
    channel_reports: dict[str, dict[str, Any]]
    estimated_noise_floor_dbfs: dict[str, float]

    def digest_payload(self) -> dict[str, Any]:
        return {
            "algorithm": "rain-reference-onset-duck-v1",
            "reference_scene": self.reference_scene,
            "residual_energy_ratio": self.residual_energy_ratio,
            "core_gain": self.core_gain,
            "windows": [
                {
                    "target_scene": window.target_scene,
                    "event_time_seconds": window.event_time_seconds,
                    "event_sample": window.event_sample,
                    "start": window.start,
                    "end": window.end,
                    "core_gain": window.core_gain,
                    "attack_frames": window.attack_frames,
                    "release_frames": window.release_frames,
                }
                for window in self.windows
            ],
        }

    def digest(self) -> str:
        payload = json.dumps(self.digest_payload(), sort_keys=True, separators=(",", ":")).encode("utf-8")
        return hashlib.sha256(payload).hexdigest()

    def as_dict(self) -> dict[str, Any]:
        all_spans = [(window.start, window.end) for window in self.windows]
        reference_modified = _union_sample_count(all_spans)
        return {
            "digest": self.digest(),
            "reference_scene": self.reference_scene,
            "residual_energy_ratio": self.residual_energy_ratio,
            "core_gain": self.core_gain,
            "window_count": len(self.windows),
            "estimated_noise_floor_dbfs": self.estimated_noise_floor_dbfs,
            "channels": self.channel_reports,
            "reference_channel": {
                "modified_sample_count": reference_modified,
                "max_modification_amplitude": float(1.0 - self.core_gain),
                "max_modification_db": float(20.0 * math.log10(self.core_gain)),
            },
            "outside_window_modified_samples": 0,
            "outside_window_max_amplitude_delta": 0.0,
            "outside_window_samples_unmodified": True,
        }


def _read_mono(path: str, start: int, frames: int) -> np.ndarray:
    with sf.SoundFile(path) as handle:
        handle.seek(max(0, start))
        return handle.read(max(0, frames), dtype="float32", always_2d=False)


def _union_sample_count(spans: list[tuple[int, int]]) -> int:
    if not spans:
        return 0
    ordered = sorted(spans)
    total = 0
    start, end = ordered[0]
    for next_start, next_end in ordered[1:]:
        if next_start <= end:
            end = max(end, next_end)
        else:
            total += end - start
            start, end = next_start, next_end
    return total + end - start


def build_masking_plan(manifest: Manifest) -> MaskingPlan:
    sample_rate = manifest.sample_rate
    residual = manifest.masking.residual_energy_ratio
    core_gain = math.sqrt(residual)
    reference_track = manifest.track(manifest.masking.reference_scene)
    core_frames = max(32, int(round(0.030 * sample_rate)))
    windows: list[DuckWindow] = []
    reports: dict[str, dict[str, Any]] = {}
    noise_floors: dict[str, float] = {}

    for target_scene in manifest.masking.target_scenes:
        if target_scene == manifest.masking.reference_scene:
            continue
        target_track = manifest.track(target_scene)
        total_signal_energy = 0.0
        total_noise_energy = 0.0
        detected_events = 0
        transient_energies: list[float] = []
        noise_energies: list[float] = []
        spans: list[tuple[int, int]] = []
        for event_index, seconds in enumerate(target_track.transient_reference_times):
            event_sample = int(round(seconds * sample_rate))
            signal = _read_mono(str(target_track.path), event_sample, core_frames)
            noise = _read_mono(str(reference_track.path), event_sample, core_frames)
            if signal.size == 0 or noise.size == 0:
                continue
            signal_energy = float(np.sum(signal.astype(np.float64) ** 2))
            signal_rms = math.sqrt(max(signal_energy / signal.size, 0.0))
            transient_dbfs = 20.0 * math.log10(max(signal_rms, 1.0e-12))
            noise_rms = math.sqrt(float(np.mean(noise.astype(np.float64) ** 2)))
            noise_dbfs = 20.0 * math.log10(max(noise_rms, 1.0e-12))
            transient_energies.append(signal_energy)
            noise_energies.append(float(np.mean(noise.astype(np.float64) ** 2)))
            if transient_dbfs < manifest.masking.detection_min_transient_dbfs:
                continue
            detected_events += 1
            declared = [
                window
                for window in manifest.masking.windows
                if window[0] <= seconds <= window[1]
            ]
            if declared:
                chosen_start, chosen_end = declared[0]
            else:
                chosen_start = seconds - manifest.masking.window_before_seconds
                chosen_end = seconds + manifest.masking.window_after_seconds
            start = max(0, int(round(chosen_start * sample_rate)))
            end = min(manifest.frames, int(round(chosen_end * sample_rate)))
            if end <= start:
                continue
            attack = min(
                end - start,
                max(1, int(round(manifest.masking.attack_seconds * sample_rate))),
            )
            release = min(
                end - start - attack,
                max(1, int(round(manifest.masking.release_seconds * sample_rate))),
            )
            windows.append(
                DuckWindow(
                    target_scene=target_scene,
                    event_time_seconds=float(seconds),
                    event_sample=event_sample,
                    start=start,
                    end=end,
                    core_gain=core_gain,
                    attack_frames=attack,
                    release_frames=release,
                )
            )
            spans.append((start, end))

            pan_bird_l, pan_bird_r = pan_gains(
                target_track, np.array([event_sample]), manifest.frames, sample_rate
            )
            pan_rain_l, pan_rain_r = pan_gains(
                reference_track,
                np.array([event_sample]),
                manifest.frames,
                sample_rate,
            )
            bird_scale = (
                target_track.mix_weight
                * target_track.mix_weight
                * (float(pan_bird_l[0]) ** 2 + float(pan_bird_r[0]) ** 2)
            )
            rain_scale = (
                reference_track.mix_weight
                * reference_track.mix_weight
                * (float(pan_rain_l[0]) ** 2 + float(pan_rain_r[0]) ** 2)
            )
            total_signal_energy += signal_energy * bird_scale
            total_noise_energy += float(np.sum(noise.astype(np.float64) ** 2)) * rain_scale

        if total_noise_energy > 0.0:
            estimated_floor = 10.0 * math.log10(
                max(float(np.mean(noise_energies)) if noise_energies else 0.0, 1.0e-24)
            )
        else:
            estimated_floor = -240.0
        noise_floors[target_scene] = float(estimated_floor)
        snr_before = (
            10.0 * math.log10(max(total_signal_energy, 1.0e-30) / max(total_noise_energy, 1.0e-30))
            if total_noise_energy > 0.0
            else math.inf
        )
        snr_after = (
            10.0
            * math.log10(
                max(total_signal_energy, 1.0e-30)
                / max(total_noise_energy * core_gain * core_gain, 1.0e-30)
            )
            if total_noise_energy > 0.0
            else math.inf
        )
        improvement = (
            -(10.0 * math.log10(core_gain * core_gain)) if total_noise_energy > 0.0 else 0.0
        )
        reports[target_scene] = {
            "channel": target_track.name,
            "estimated_noise_floor_dbfs": float(estimated_floor),
            "detected_transient_count": detected_events,
            "total_reference_transient_count": len(target_track.transient_reference_times),
            "modified_sample_count": _union_sample_count(spans),
            "max_modification_amplitude": float(1.0 - core_gain),
            "max_modification_db": float(20.0 * math.log10(core_gain)),
            "residual_energy_ratio": float(residual),
            "snr_before_db": float(snr_before),
            "snr_after_db": float(snr_after),
            "snr_improvement_db": float(improvement),
            "required_snr_improvement_db": manifest.masking.required_snr_improvement_db,
            "passes_snr": bool(improvement + 1.0e-9 >= manifest.masking.required_snr_improvement_db),
            "window_samples": [[window.start, window.end] for window in windows if window.target_scene == target_scene],
            "window_times_seconds": [
                [window.start / sample_rate, window.end / sample_rate]
                for window in windows
                if window.target_scene == target_scene
            ],
        }

    return MaskingPlan(
        reference_scene=manifest.masking.reference_scene,
        residual_energy_ratio=residual,
        core_gain=core_gain,
        windows=windows,
        channel_reports=reports,
        estimated_noise_floor_dbfs=noise_floors,
    )


def duck_envelope(start: int, end: int, windows: list[DuckWindow]) -> np.ndarray:
    frames = max(0, end - start)
    gain = np.ones(frames, dtype=np.float32)
    for window in windows:
        low = max(start, window.start)
        high = min(end, window.end)
        if high <= low:
            continue
        local_start = low - start
        local_end = high - start
        count = local_end - local_start
        profile = np.ones(count, dtype=np.float64)
        attack = min(window.attack_frames, count)
        release = min(window.release_frames, max(0, count - attack))
        if attack > 0:
            u = (np.arange(attack, dtype=np.float64) + 0.5) / attack
            profile[:attack] = 1.0 + (window.core_gain - 1.0) * (0.5 - 0.5 * np.cos(np.pi * u))
        core_start = attack
        core_end = count - release
        if core_end > core_start:
            profile[core_start:core_end] = window.core_gain
        if release > 0:
            u = (np.arange(release, dtype=np.float64) + 0.5) / release
            profile[core_end:] = window.core_gain + (1.0 - window.core_gain) * (
                0.5 - 0.5 * np.cos(np.pi * u)
            )
        gain[local_start:local_end] = np.minimum(gain[local_start:local_end], profile.astype(np.float32))
    return gain

