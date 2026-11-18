from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .alignment import AlignedStem
from .dsp import bandpass_fft, energy_db, merge_intervals, stable_float, window_gate
from .manifest import Manifest


@dataclass
class UnmaskChannelResult:
    source_group: str
    target_role: str
    target_channel: str
    target_group: str
    detected: bool
    modified_intervals: list[tuple[int, int]]
    modified_sample_count: int
    before_energy: float
    after_energy: float
    attenuation_db: float
    clean_energy_before: float
    clean_energy_after: float
    clean_loss_db: float
    params: dict[str, Any]

    def report(self) -> dict[str, Any]:
        return {
            "channel": self.target_channel,
            "group": self.target_group,
            "source_group": self.source_group,
            "band_hz": self.params.get("band_hz"),
            "windows_seconds": self.params.get("windows_seconds"),
            "detected": self.detected,
            "injection_energy_db_before": stable_float(energy_db(self.before_energy)),
            "injection_energy_db_after": stable_float(energy_db(self.after_energy)),
            "attenuation_db": stable_float(self.attenuation_db),
            "clean_energy_loss_db": stable_float(self.clean_loss_db),
            "modified_sample_count": self.modified_sample_count,
            "modified_intervals": [
                {"start": start, "end": end, "samples": end - start}
                for start, end in self.modified_intervals
            ],
        }


@dataclass
class UnmaskResult:
    stems: dict[str, np.ndarray]
    channels: list[UnmaskChannelResult]
    params: dict[str, Any]

    def report(self, manifest: Manifest) -> dict[str, Any]:
        return {
            "window_seconds": [list(item) for item in manifest.unmask_windows_seconds],
            "residual_threshold_db": stable_float(manifest.residual_threshold_db),
            "residual_threshold_linear": stable_float(manifest.residual_threshold_linear),
            "min_attenuation_db": stable_float(manifest.min_attenuation_db),
            "channels": [channel.report() for channel in self.channels],
        }


def _detect_and_apply_relation(
    manifest: Manifest,
    relation: Any,
    target: np.ndarray,
    reference: np.ndarray,
    target_track: Any,
) -> tuple[np.ndarray, UnmaskChannelResult]:
    low, high = relation.band_hz
    gate = window_gate(
        len(target),
        manifest.sample_rate,
        list(relation.windows_seconds),
        fade_seconds=0.006,
    )
    difference = target - reference
    injection = bandpass_fft(difference, manifest.sample_rate, low, high) * gate
    clean_band = bandpass_fft(reference, manifest.sample_rate, low, high) * gate
    before_energy = float(np.sum(injection**2))
    clean_before = float(np.sum(clean_band**2))
    # The independent monitor channel establishes the local clean/noise floor.
    # A +60 dB signal-to-floor margin prevents numerical dust from being treated
    # as a masking component while remaining far below the synthetic injections.
    detection_floor = max(1e-24, clean_before * 1e-6)
    detected = bool(before_energy > detection_floor and np.any(gate > 0))
    declared_residual = float(manifest.residual_threshold_linear)
    attenuation_ceiling_residual = 10.0 ** (-manifest.min_attenuation_db / 20.0)
    residual = min(declared_residual, attenuation_ceiling_residual)
    intervals: list[tuple[int, int]] = []
    after_energy = before_energy
    clean_after = clean_before
    processed = target
    if detected:
        # Subtracting all but the declared residual amplitude creates a
        # predictable residual-energy bound.  The gate is exactly zero outside
        # the declared windows.
        processed = target - injection * (1.0 - residual)
        after_energy = before_energy * residual**2
        after_band = bandpass_fft(processed, manifest.sample_rate, low, high) * gate
        after_clean_projection = after_band - injection * residual
        clean_after = float(np.sum(after_clean_projection**2))
        intervals = merge_intervals(gate > 0)
    else:
        after_band = bandpass_fft(processed, manifest.sample_rate, low, high) * gate
        clean_after = float(np.sum((after_band - injection) ** 2))
    if before_energy > 0 and after_energy > 0:
        attenuation = 10.0 * math.log10(before_energy / after_energy)
    else:
        attenuation = 0.0 if before_energy == 0 else 120.0
    if clean_before > 0 and clean_after > 0:
        clean_loss = 10.0 * math.log10(clean_before / clean_after)
    else:
        clean_loss = 0.0
    modified_count = sum(end - start for start, end in intervals)
    params = {
        "source_group": relation.source_group,
        "target_role": target_track.role,
        "target_channel": target_track.name,
        "target_group": target_track.group,
        "band_hz": [stable_float(low), stable_float(high)],
        "windows_seconds": [list(item) for item in relation.windows_seconds],
        "detected": detected,
        # Replayed after resume: retain full binary64 round-trip precision.
        "remove_factor": float(1.0 - residual),
        "residual_linear": float(residual),
        "declared_residual_linear": float(declared_residual),
        "modified_intervals": [[start, end] for start, end in intervals],
        "modified_sample_count": modified_count,
        "before_energy": float(before_energy),
        "after_energy": float(after_energy),
        "attenuation_db": float(attenuation),
        "clean_energy_before": float(clean_before),
        "clean_energy_after": float(clean_after),
        "clean_loss_db": float(clean_loss),
    }
    result = UnmaskChannelResult(
        source_group=relation.source_group,
        target_role=target_track.role,
        target_channel=target_track.name,
        target_group=target_track.group,
        detected=detected,
        modified_intervals=intervals,
        modified_sample_count=modified_count,
        before_energy=before_energy,
        after_energy=after_energy,
        attenuation_db=attenuation,
        clean_energy_before=clean_before,
        clean_energy_after=clean_after,
        clean_loss_db=clean_loss,
        params=params,
    )
    return processed, result


def unmask_stems(manifest: Manifest, alignment: Any) -> UnmaskResult:
    working = {role: stem.audio.copy() for role, stem in alignment.stems.items()}
    channels: list[UnmaskChannelResult] = []
    params_channels: list[dict[str, Any]] = []
    for relation in manifest.masking:
        target_track = manifest.track_by_role(relation.target_group)
        source_track = manifest.track_by_role(relation.source_group)
        if source_track.role not in alignment.stems:
            continue
        current = working[target_track.role]
        reference = alignment.stems[target_track.role].reference
        processed, result = _detect_and_apply_relation(
            manifest,
            relation,
            current,
            reference,
            target_track,
        )
        working[target_track.role] = processed
        channels.append(result)
        params_channels.append(result.params)
    params = {
        "channels": params_channels,
    }
    return UnmaskResult(stems=working, channels=channels, params=params)


def reapply_unmask_params(
    manifest: Manifest,
    alignment: Any,
    params: dict[str, Any],
) -> UnmaskResult:
    channels_spec = params.get("channels", [])
    relation_specs = {(item["source_group"], item["target_role"]): item for item in channels_spec}
    working = {role: stem.audio.copy() for role, stem in alignment.stems.items()}
    channels: list[UnmaskChannelResult] = []
    for relation in manifest.masking:
        target_track = manifest.track_by_role(relation.target_group)
        spec = relation_specs.get((relation.source_group, target_track.role))
        if spec is None:
            raise ValueError("快照中的解耦参数与清单的掩蔽关系不一致")
        if not spec.get("detected", False):
            channels.append(
                UnmaskChannelResult(
                    source_group=relation.source_group,
                    target_role=target_track.role,
                    target_channel=target_track.name,
                    target_group=target_track.group,
                    detected=False,
                    modified_intervals=[],
                    modified_sample_count=0,
                    before_energy=float(spec["before_energy"]),
                    after_energy=float(spec["after_energy"]),
                    attenuation_db=float(spec["attenuation_db"]),
                    clean_energy_before=float(spec["clean_energy_before"]),
                    clean_energy_after=float(spec["clean_energy_after"]),
                    clean_loss_db=float(spec["clean_loss_db"]),
                    params=spec,
                )
            )
            continue
        low, high = spec["band_hz"]
        windows = [tuple(item) for item in spec["windows_seconds"]]
        gate = window_gate(len(working[target_track.role]), manifest.sample_rate, windows, fade_seconds=0.006)
        reference = alignment.stems[target_track.role].reference
        injection = bandpass_fft(
            working[target_track.role] - reference,
            manifest.sample_rate,
            float(low),
            float(high),
        ) * gate
        working[target_track.role] = working[target_track.role] - injection * float(spec["remove_factor"])
        intervals = [tuple(item) for item in spec.get("modified_intervals", [])]
        channels.append(
            UnmaskChannelResult(
                source_group=relation.source_group,
                target_role=target_track.role,
                target_channel=target_track.name,
                target_group=target_track.group,
                detected=True,
                modified_intervals=[(int(a), int(b)) for a, b in intervals],
                modified_sample_count=int(spec["modified_sample_count"]),
                before_energy=float(spec["before_energy"]),
                after_energy=float(spec["after_energy"]),
                attenuation_db=float(spec["attenuation_db"]),
                clean_energy_before=float(spec["clean_energy_before"]),
                clean_energy_after=float(spec["clean_energy_after"]),
                clean_loss_db=float(spec["clean_loss_db"]),
                params=spec,
            )
        )
    return UnmaskResult(stems=working, channels=channels, params=params)
