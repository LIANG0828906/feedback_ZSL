#!/usr/bin/env python3
"""Create the deterministic example project used by the acceptance tests."""

from __future__ import annotations

import json
import math
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
PROJECT = ROOT / "examples" / "project.json"


def quat_yaw(degrees: float) -> list[float]:
    angle = math.radians(degrees) / 2.0
    return [math.cos(angle), 0.0, 0.0, math.sin(angle)]


def rotate_inverse(quaternion: list[float], vector: list[float]) -> list[float]:
    w, x, y, z = quaternion
    qv = (x, y, z)
    v = vector
    cross = (
        qv[1] * v[2] - qv[2] * v[1],
        qv[2] * v[0] - qv[0] * v[2],
        qv[0] * v[1] - qv[1] * v[0],
    )
    cross2 = (
        qv[1] * cross[2] - qv[2] * cross[1],
        qv[2] * cross[0] - qv[0] * cross[2],
        qv[0] * cross[1] - qv[1] * cross[0],
    )
    return [
        v[index] - 2.0 * (w * cross[index] + cross2[index])
        for index in range(3)
    ]


def head_pose(time_s: float) -> tuple[list[float], list[float]]:
    position = [
        0.10 * math.sin(0.30 * time_s),
        -0.20 + 0.08 * math.sin(0.40 * time_s),
        1.60,
    ]
    yaw = 12.0 * math.sin(0.50 * time_s)
    return position, quat_yaw(yaw)


def azimuth(anchor: list[float], position: list[float], quaternion: list[float]) -> float:
    relative = [anchor[index] - position[index] for index in range(3)]
    local = rotate_inverse(quaternion, relative)
    return math.degrees(math.atan2(local[0], local[1]))


def main() -> None:
    roles = [
        ("actor_01", "actor", [2.60, 1.40, 1.55]),
        ("actor_02", "actor", [-2.40, 1.00, 1.50]),
        ("actor_03", "actor", [1.90, -2.20, 1.60]),
        ("actor_04", "actor", [-2.10, -1.70, 1.45]),
        ("actor_05", "actor", [0.00, 2.80, 1.60]),
        ("stage_ambience", "environment", [-4.00, 0.00, 2.10]),
        ("audience_ambience", "audience", [0.00, -4.00, 1.80]),
        ("speaker_reference", "speaker_reference", None),
    ]
    tracks = []
    for name, role, anchor in roles:
        track = {
            "name": name,
            "path": f"audio/{name}.wav",
            "sample_rate": 48000,
            "channels": 1,
            "bit_depth": 24,
            "source_type": role,
        }
        if anchor is not None:
            track["anchor"] = {"x": anchor[0], "y": anchor[1], "z": anchor[2]}
        tracks.append(track)

    samples = []
    for index in range(61):
        time_s = round(index * 0.1, 10)
        position, quaternion = head_pose(time_s)
        samples.append(
            {
                "t": time_s,
                "position": [round(value, 12) for value in position],
                "orientation_quaternion": [round(value, 12) for value in quaternion],
            }
        )

    check_times = [0.75, 1.50, 2.25, 3.00, 3.75, 4.50, 5.25]
    checks = []
    for time_s in check_times:
        position, quaternion = head_pose(time_s)
        checks.append(
            {
                "time_s": time_s,
                "channel": "actor_01",
                "expected_azimuth_deg": round(
                    azimuth([2.60, 1.40, 1.55], position, quaternion), 9
                ),
            }
        )

    project = {
        "schema_version": 1,
        "project_id": "theatre-closed-rehearsal-uid233",
        "seed": 233,
        "duration_seconds": 6.0,
        "sample_rate": 48000,
        "tracks": tracks,
        "reference_channel": "speaker_reference",
        "leakage": {
            "reference_channel": "speaker_reference",
            "injection_channels": ["actor_01", "actor_03", "actor_05"],
            "gain_db": -10.0,
            "channel_gain_db": {
                "actor_01": -10.0,
                "actor_03": -11.0,
                "actor_05": -9.0,
            },
            "injection_delay_ms": 9.0,
            "max_delay_ms": 25.0,
            "lowpass_hz": 120.0,
        },
        "head_tracking": {
            "quaternion_order": "wxyz",
            "orientation_convention": "local_to_world",
            "quaternion_norm_tolerance": 0.0001,
            "max_ild_rate_db_per_s": 600.0,
            "max_itd_rate_us_per_s": 30000.0,
            "samples": samples,
        },
        "head_model": {
            "head_radius_m": 0.0875,
            "speed_of_sound_m_s": 343.0,
            "max_ild_db": 12.0,
        },
        "binaural": {
            "downmix": {
                "actor_01": {"left": 1.0, "right": 1.0},
                "actor_02": {"left": 1.0, "right": 1.0},
                "actor_03": {"left": 1.0, "right": 1.0},
                "actor_04": {"left": 1.0, "right": 1.0},
                "actor_05": {"left": 1.0, "right": 1.0},
                "stage_ambience": {"left": 0.45, "right": 0.45},
                "audience_ambience": {"left": 0.35, "right": 0.35},
            }
        },
        "interpolation": {
            "azimuth_tolerance_deg": 2.0,
            "max_ild_rate_db_per_s": 600.0,
            "max_itd_rate_us_per_s": 30000.0,
            "checks": checks,
        },
        "targets": {
            "loudness_lufs": -23.0,
            "true_peak_dbtp": -1.0,
        },
        "render": {
            "block_frames": 2400,
            "master_bit_depth": 24,
            "limiter_lookahead_ms": 5.0,
            "limiter_release_ms": 80.0,
        },
    }
    PROJECT.parent.mkdir(parents=True, exist_ok=True)
    PROJECT.write_text(
        json.dumps(project, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()

