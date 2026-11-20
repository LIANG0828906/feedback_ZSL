from __future__ import annotations

import json
from pathlib import Path
from typing import Any


def sample_payloads() -> dict[str, dict[str, Any]]:
    """Deterministic local fixtures covering overlap and all envelopes."""
    return {
        "single_short": {
            "name": "single_short",
            "description": "one short rectangular pulse",
            "sample_rate": 1000,
            "device": {
                "min": -1.0,
                "max": 1.0,
                "limiter": "hard",
                "soft_knee_ratio": 0.8,
            },
            "motor_map": {
                "left_ratio": 0.9,
                "right_ratio": 0.7,
                "left_polarity": 1,
                "right_polarity": 1,
            },
            "segments": [
                {
                    "start_ms": 0,
                    "duration_ms": 40,
                    "frequency_hz": 35,
                    "amplitude": 0.55,
                    "envelope": "rectangular",
                }
            ],
        },
        "three_overlap": {
            "name": "three_overlap",
            "description": "three overlapping segments with hard limiting",
            "sample_rate": 1000,
            "device": {
                "min": -1.0,
                "max": 1.0,
                "limiter": "hard",
                "soft_knee_ratio": 0.8,
            },
            "motor_map": {
                "left_ratio": 0.8,
                "right_ratio": 0.8,
                "left_polarity": 1,
                "right_polarity": 1,
            },
            "segments": [
                {
                    "start_ms": 0,
                    "duration_ms": 180,
                    "frequency_hz": 24,
                    "amplitude": 0.85,
                    "envelope": "sine",
                },
                {
                    "start_ms": 70,
                    "duration_ms": 180,
                    "frequency_hz": 41,
                    "amplitude": 0.7,
                    "envelope": "linear_decay",
                },
                {
                    "start_ms": 140,
                    "duration_ms": 160,
                    "frequency_hz": 17,
                    "amplitude": 0.6,
                    "envelope": "rectangular",
                    "left_polarity": -1,
                },
            ],
        },
        "mixed_envelopes": {
            "name": "mixed_envelopes",
            "description": "five segments, three envelopes, asymmetric motors",
            "sample_rate": 2000,
            "device": {
                "min": -1.0,
                "max": 1.0,
                "limiter": "hard",
                "soft_knee_ratio": 0.8,
            },
            "motor_map": {
                "left_ratio": 0.9,
                "right_ratio": 0.65,
                "left_polarity": 1,
                "right_polarity": 1,
            },
            "segments": [
                {
                    "start_ms": 0,
                    "duration_ms": 220,
                    "frequency_hz": 55,
                    "amplitude": 0.65,
                    "envelope": "sine",
                },
                {
                    "start_ms": 60,
                    "duration_ms": 260,
                    "frequency_hz": 72,
                    "amplitude": 0.8,
                    "envelope": "rectangular",
                    "left_ratio": 0.7,
                    "right_ratio": 1.0,
                },
                {
                    "start_ms": 170,
                    "duration_ms": 240,
                    "frequency_hz": 31,
                    "amplitude": 0.55,
                    "envelope": "linear_decay",
                    "right_polarity": -1,
                },
                {
                    "start_ms": 300,
                    "duration_ms": 180,
                    "frequency_hz": 110,
                    "amplitude": 0.45,
                    "envelope": "sine",
                    "left_ratio": 1.0,
                    "right_ratio": 0.35,
                },
                {
                    "start_ms": 390,
                    "duration_ms": 210,
                    "frequency_hz": 18,
                    "amplitude": 0.9,
                    "envelope": "rectangular",
                    "left_polarity": -1,
                },
            ],
        },
        "dense_soft": {
            "name": "dense_soft",
            "description": "eight heavily overlapping segments with soft limiting",
            "sample_rate": 2000,
            "device": {
                "min": -1.0,
                "max": 1.0,
                "limiter": "soft",
                "soft_knee_ratio": 0.7,
            },
            "motor_map": {
                "left_ratio": 0.85,
                "right_ratio": 0.75,
                "left_polarity": 1,
                "right_polarity": 1,
            },
            "segments": [
                {
                    "start_ms": 0,
                    "duration_ms": 180,
                    "frequency_hz": 20,
                    "amplitude": 0.8,
                    "envelope": "sine",
                },
                {
                    "start_ms": 35,
                    "duration_ms": 200,
                    "frequency_hz": 33,
                    "amplitude": 0.75,
                    "envelope": "linear_decay",
                },
                {
                    "start_ms": 80,
                    "duration_ms": 190,
                    "frequency_hz": 47,
                    "amplitude": 0.7,
                    "envelope": "rectangular",
                    "right_polarity": -1,
                },
                {
                    "start_ms": 115,
                    "duration_ms": 170,
                    "frequency_hz": 62,
                    "amplitude": 0.65,
                    "envelope": "sine",
                },
                {
                    "start_ms": 145,
                    "duration_ms": 180,
                    "frequency_hz": 29,
                    "amplitude": 0.9,
                    "envelope": "linear_decay",
                },
                {
                    "start_ms": 190,
                    "duration_ms": 210,
                    "frequency_hz": 81,
                    "amplitude": 0.55,
                    "envelope": "rectangular",
                },
                {
                    "start_ms": 230,
                    "duration_ms": 160,
                    "frequency_hz": 14,
                    "amplitude": 0.85,
                    "envelope": "sine",
                    "left_polarity": -1,
                },
                {
                    "start_ms": 275,
                    "duration_ms": 185,
                    "frequency_hz": 53,
                    "amplitude": 0.7,
                    "envelope": "linear_decay",
                    "left_ratio": 1.1,
                    "right_ratio": 0.45,
                },
            ],
        },
        "cancel_long": {
            "name": "cancel_long",
            "description": "long high-rate fixture for cooperative cancellation",
            "sample_rate": 8000,
            "device": {
                "min": -1.0,
                "max": 1.0,
                "limiter": "hard",
                "soft_knee_ratio": 0.8,
            },
            "motor_map": {
                "left_ratio": 0.8,
                "right_ratio": 0.8,
                "left_polarity": 1,
                "right_polarity": 1,
            },
            "segments": [
                {
                    "start_ms": 0,
                    "duration_ms": 12000,
                    "frequency_hz": 180,
                    "amplitude": 0.7,
                    "envelope": "sine",
                },
                {
                    "start_ms": 2500,
                    "duration_ms": 9000,
                    "frequency_hz": 240,
                    "amplitude": 0.65,
                    "envelope": "linear_decay",
                },
                {
                    "start_ms": 5000,
                    "duration_ms": 6000,
                    "frequency_hz": 310,
                    "amplitude": 0.55,
                    "envelope": "rectangular",
                },
            ],
        },
    }


def write_samples(outdir: str | Path) -> list[Path]:
    target = Path(outdir)
    target.mkdir(parents=True, exist_ok=True)
    paths: list[Path] = []
    for name, payload in sorted(sample_payloads().items()):
        path = target / f"{name}.json"
        path.write_text(
            json.dumps(payload, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        paths.append(path)
    return paths


def write_manifest(outdir: str | Path) -> Path:
    target = Path(outdir)
    payload = sample_payloads()
    manifest = {
        "schema": "haptic-samples-v1",
        "samples": [
            {
                "name": name,
                "file": f"{name}.json",
                "segments": len(payload[name]["segments"]),
                "sample_rate": payload[name]["sample_rate"],
                "limiter": payload[name]["device"]["limiter"],
                "envelopes": sorted(
                    {segment["envelope"] for segment in payload[name]["segments"]}
                ),
            }
            for name in sorted(payload)
        ],
    }
    path = target / "manifest.json"
    path.write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    return path
