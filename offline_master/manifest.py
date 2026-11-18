"""Project manifest loading and exhaustive validation."""

from __future__ import annotations

import json
import os
from typing import Any

import soundfile as sf

from .codes import BusinessError, ErrorCode
from .dsp import canonical_json_bytes

VALID_TYPES = ("dialogue", "footstep", "cloth", "ambient")
VALID_SUBTYPES = {16: "PCM_16", 24: "PCM_24", 32: "PCM_32"}


def _warn(warnings: list[dict], file_name: str, reason: str, detail: Any = None) -> None:
    warnings.append(
        {"file": file_name, "reason": reason, "detail": detail}
    )


def load_manifest(path: str) -> dict:
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def manifest_digest(manifest: dict) -> str:
    """Stable digest of the manifest contents (path independent)."""
    import hashlib

    return hashlib.sha256(canonical_json_bytes(manifest)).hexdigest()


def _require(
    manifest: dict,
    key: str,
    expected_type: type | tuple[type, ...],
    warnings: list[dict],
    predicate=lambda value: True,
) -> Any:
    if key not in manifest:
        _warn(warnings, "<manifest>", "missing_field", key)
        return None
    value = manifest[key]
    if not isinstance(value, expected_type) or not predicate(value):
        _warn(warnings, "<manifest>", "invalid_field", {"field": key, "value": value})
        return None
    return value


def validate_manifest(manifest: dict, base_dir: str) -> dict:
    """Validate every rule and every channel without short-circuiting.

    Raises :class:`BusinessError` with code ``E_MANIFEST_INVALID`` carrying the
    full structured warning list when any rule fails. Validated/normalized
    values are returned on success.
    """
    warnings: list[dict] = []
    sample_rate = _require(
        manifest, "sample_rate", int, warnings, lambda v: v > 0
    )
    bit_depth = _require(
        manifest, "bit_depth", int, warnings, lambda v: v in VALID_SUBTYPES
    )
    channels = _require(
        manifest, "channels", int, warnings, lambda v: v >= 1
    )
    duration_s = _require(
        manifest, "duration_s", (int, float), warnings, lambda v: v > 0
    )
    frame_s = _require(
        manifest, "frame_s", (int, float), warnings, lambda v: v > 0
    )
    block_frames = _require(
        manifest, "block_frames", int, warnings, lambda v: v > 0
    )
    stability_db = manifest.get("stability_threshold_db", 0.5)
    if not isinstance(stability_db, (int, float)) or stability_db <= 0:
        _warn(
            warnings,
            "<manifest>",
            "invalid_field",
            {"field": "stability_threshold_db", "value": stability_db},
        )
    limits = manifest.get("mastering", {})
    target_lufs = limits.get("integrated_lufs", -16.0)
    true_peak_db = limits.get("true_peak_db", -1.0)
    if not isinstance(target_lufs, (int, float)) or target_lufs >= 0:
        _warn(warnings, "<manifest>", "invalid_field", "mastering.integrated_lufs")
    if not isinstance(true_peak_db, (int, float)) or true_peak_db > 0:
        _warn(warnings, "<manifest>", "invalid_field", "mastering.true_peak_db")

    turntable_angles = manifest.get("turntable_angles_deg")
    expected_angles = list(range(0, 360, 10))
    if (
        not isinstance(turntable_angles, list)
        or turntable_angles != expected_angles
        or any(b <= a for a, b in zip(turntable_angles, turntable_angles[1:]))
    ):
        _warn(
            warnings,
            "<manifest>",
            "angle_sequence_invalid",
            turntable_angles,
        )

    lip_points = manifest.get("lip_sync_points_s")
    if (
        not isinstance(lip_points, list)
        or not lip_points
        or any(not isinstance(p, (int, float)) for p in lip_points)
        or any(b <= a for a, b in zip(lip_points, lip_points[1:]))
    ):
        _warn(warnings, "<manifest>", "lip_points_not_ascending", lip_points)
    elif duration_s is not None and lip_points[-1] >= duration_s:
        _warn(warnings, "<manifest>", "lip_points_outside_duration", lip_points)

    bounds = manifest.get("local_coordinate_bounds", {})
    x_range = bounds.get("x", [-1.0, 1.0])
    y_range = bounds.get("y", [0.0, 2.0])
    z_range = bounds.get("z", [-1.0, 1.0])
    ranges_ok = all(
        isinstance(r, list) and len(r) == 2 and r[0] <= r[1]
        for r in (x_range, y_range, z_range)
    )
    if not ranges_ok:
        _warn(warnings, "<manifest>", "invalid_coordinate_bounds", bounds)

    tracks = manifest.get("tracks")
    if not isinstance(tracks, list) or not tracks:
        _warn(warnings, "<manifest>", "missing_field", "tracks")
        tracks = []

    seen_files: set[str] = set()
    seen_types: set[str] = set()
    allow_duplicate_types = bool(manifest.get("allow_duplicate_types", False))
    validated_tracks = []
    for index, track in enumerate(tracks):
        name = track.get("file", f"<track[{index}]>") if isinstance(track, dict) else f"<track[{index}]>"
        if not isinstance(track, dict):
            _warn(warnings, name, "track_not_object", track)
            continue
        file_path = track.get("file")
        if not isinstance(file_path, str) or not file_path:
            _warn(warnings, name, "missing_field", "file")
            continue
        if file_path in seen_files:
            _warn(warnings, file_path, "duplicate_file_path")
        seen_files.add(file_path)

        material = track.get("material_type")
        if material not in VALID_TYPES:
            _warn(warnings, file_path, "unknown_material_type", material)
        elif material in seen_types and not allow_duplicate_types:
            _warn(warnings, file_path, "duplicate_material_type", material)
        else:
            seen_types.add(material)

        anchor = track.get("anchor")
        anchor_ok = (
            isinstance(anchor, list)
            and len(anchor) == 3
            and all(isinstance(c, (int, float)) for c in anchor)
        )
        if anchor_ok and ranges_ok:
            x, y, z = anchor
            if not (
                x_range[0] <= x <= x_range[1]
                and y_range[0] <= y <= y_range[1]
                and z_range[0] <= z <= z_range[1]
            ):
                anchor_ok = False
        if not anchor_ok:
            _warn(warnings, file_path, "anchor_out_of_bounds", anchor)

        if not isinstance(track.get("seed"), int):
            _warn(warnings, file_path, "invalid_field", "seed")
        if not isinstance(track.get("start_offset_s"), (int, float)) or track.get("start_offset_s", 0) < 0:
            _warn(warnings, file_path, "invalid_field", "start_offset_s")

        absolute = os.path.join(base_dir, file_path)
        if not os.path.isfile(absolute):
            _warn(warnings, file_path, "file_not_found")
        elif sample_rate and bit_depth and channels:
            try:
                info = sf.info(absolute)
                if info.samplerate != sample_rate:
                    _warn(
                        warnings,
                        file_path,
                        "sample_rate_mismatch",
                        {"expected": sample_rate, "actual": info.samplerate},
                    )
                if info.channels != channels:
                    _warn(
                        warnings,
                        file_path,
                        "channel_count_mismatch",
                        {"expected": channels, "actual": info.channels},
                    )
                expected_subtype = VALID_SUBTYPES.get(bit_depth)
                if info.subtype != expected_subtype:
                    _warn(
                        warnings,
                        file_path,
                        "bit_depth_mismatch",
                        {"expected": expected_subtype, "actual": info.subtype},
                    )
            except Exception as exc:  # pragma: no cover - backend failure
                _warn(warnings, file_path, "unreadable_audio", str(exc))
        validated_tracks.append(track)

    if warnings:
        raise BusinessError(
            ErrorCode.E_MANIFEST_INVALID,
            warnings,
            f"manifest validation failed with {len(warnings)} warning(s)",
        )

    return {
        "sample_rate": sample_rate,
        "bit_depth": bit_depth,
        "channels": channels,
        "duration_s": float(duration_s),
        "frame_s": float(frame_s),
        "block_frames": block_frames,
        "stability_threshold_db": float(stability_db),
        "target_lufs": float(target_lufs),
        "true_peak_db": float(true_peak_db),
        "angles": list(turntable_angles),
        "lip_points": [float(p) for p in lip_points],
        "bounds": {"x": x_range, "y": y_range, "z": z_range},
        "tracks": validated_tracks,
        "digest": manifest_digest(manifest),
    }
