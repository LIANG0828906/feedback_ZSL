"""Deterministic local validation scene factory."""

from __future__ import annotations

from typing import Any

import numpy as np

from .density import generate_density


def scene_spec(*, embedded_density: bool = False) -> dict[str, Any]:
    dimensions = (40, 24, 56)
    origin = np.zeros(3, dtype=np.float64)
    voxel_size = np.full(3, 0.5, dtype=np.float64)
    density_range = [0.0, 1.0]
    noise: dict[str, Any] = {
        "octaves": 5,
        "base_frequency": 0.105,
        "lacunarity": 2.03,
        "gain": 0.53,
        "floor_thickness": 0.34,
        "top_thickness": 0.17,
        "strata_frequency": 5.0,
    }
    volume: dict[str, Any] = {
        "dimensions": list(dimensions),
        "origin": [0.0, 0.0, 0.0],
        "voxel_size": [0.5, 0.5, 0.5],
        "density_range": density_range,
        "noise": noise,
    }
    if embedded_density:
        density = generate_density(
            seed=212,
            dimensions=dimensions,
            origin=origin,
            voxel_size=voxel_size,
            density_range=(density_range[0], density_range[1]),
            noise=noise,
        )
        volume["density"] = [
            round(float(value), 9) for value in density.reshape(-1)
        ]

    return {
        "schema_version": 1,
        "scene_id": "fogport_validation",
        "seed": 212,
        "volume": volume,
        "camera": {
            "position": [27.0, 7.2, -8.0],
            "target": [10.0, 4.8, 14.0],
            "up": [0.0, 1.0, 0.0],
            "fov_deg": 52.0,
            "width": 160,
            "height": 120,
            "aspect": 160.0 / 120.0,
        },
        "lights": [
            {
                "id": "floor_amber",
                "kind": "floor",
                "position": [8.0, -1.4, 7.0],
                "direction": [0.15, 0.88, 0.45],
                "color": [1.0, 0.47, 0.16],
                "intensity": 240.0,
                "inner_cone_deg": 4.0,
                "outer_cone_deg": 13.0,
                "range": 18.0,
                "falloff": 1.7,
            },
            {
                "id": "side_cyan",
                "kind": "side",
                "position": [24.0, 5.6, 12.0],
                "direction": [-0.92, -0.08, -0.38],
                "color": [0.22, 0.72, 1.0],
                "intensity": 180.0,
                "inner_cone_deg": 3.0,
                "outer_cone_deg": 11.0,
                "range": 17.0,
                "falloff": 1.6,
            },
        ],
        "render": {
            "step": 1,
            "substeps": 4,
            "max_samples": 1024,
            "chunk_size": [20, 15],
            "absorption": 0.035,
            "scattering": 0.26,
            "phase_g": 0.08,
            "light_step": 0.8,
            "light_max_samples": 96,
            "tonemap": "aces",
            "exposure": 1.4,
            "gamma": 2.2,
            "channel_max": 1.0,
            "convergence_tolerance": 0.06,
            "ambient": [0.0015, 0.0035, 0.007],
        },
    }

