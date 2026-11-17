"""Deterministic demo scene: cloister -> crypt transition soundscape."""

from __future__ import annotations

import json
from pathlib import Path


def build_demo_scene(seed: int = 20261118) -> dict:
    duration = 8.0
    sample_rate = 24000
    frame_rate = 30.0
    path = []
    steps = 41
    for index in range(steps):
        t = duration * index / (steps - 1)
        fraction = index / (steps - 1)
        x = -10.0 + 20.0 * fraction
        # Lateral sway makes the listener cross column/bar sight lines.
        y = 0.55 * __import__("math").sin(2.0 * 3.141592653589793 * 0.5 * t)
        yaw = 0.6 * __import__("math").sin(2.0 * 3.141592653589793 * 0.25 * t)
        path.append({"t": round(t, 6), "position": [round(x, 6), round(y, 6), 1.7],
                     "yaw": round(yaw, 6)})
    return {
        "project": {
            "sample_rate": sample_rate,
            "frame_rate": frame_rate,
            "block_samples": 12000,
            "duration": duration,
            "seed": seed,
            "distance_ref": 5.0,
            "distance_gain_exponent": 1.0,
            "diffraction_db_per_m": 2.5,
            "max_rt60_rate_s": 1.5,
            "crossfade_width": 0.75,
            "tail_seconds": 1.0,
        },
        "sources": [
            {"id": "organ", "kind": "organ", "position": [-13.0, 0.0, 3.2],
             "file": "dry/organ.wav", "gain": 0.0},
            {"id": "drip", "kind": "drip", "position": [12.0, -1.4, 2.6],
             "file": "dry/drip.wav", "gain": -2.0},
            {"id": "footstep", "kind": "footstep", "position": [-1.0, 1.2, 0.3],
             "file": "dry/footstep.wav", "gain": -4.0},
        ],
        "path": path,
        "layers": [
            {
                "id": "colonnade",
                "type": "colonnade",
                "plane": -3.5,
                "transmission": 0.55,
                "reverb": {"rt60": 2.6, "wet": 0.32},
                "geometry": {"column_count": 6, "column_radius": 0.42, "jitter": 0.04},
            },
            {
                "id": "arch",
                "type": "arch",
                "plane": 0.0,
                "transmission": 0.7,
                "reverb": {"rt60": 1.4, "wet": 0.24},
                "geometry": {
                    "opening_width": 1.8,
                    "opening_height": 2.6,
                    "pillar_width": 0.9,
                    "jitter": 0.02,
                },
            },
            {
                "id": "fence",
                "type": "fence",
                "plane": 3.5,
                "transmission": 0.35,
                "reverb": {"rt60": 0.55, "wet": 0.14},
                "geometry": {"bar_count": 9, "bar_width": 0.16, "jitter": 0.015},
            },
        ],
    }


def write_demo_scene(target: str | Path) -> Path:
    target = Path(target)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps(build_demo_scene(), indent=2), encoding="utf-8")
    return target

