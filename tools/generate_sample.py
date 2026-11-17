#!/usr/bin/env python3
"""Generate a deterministic local project and loop-layer WAVs."""

from __future__ import annotations

import argparse
import json
from pathlib import Path

import numpy as np
import soundfile as sf

SAMPLE_RATE = 48000


def _noise(rng: np.random.Generator, frames: int, channels: int) -> np.ndarray:
    noise = rng.standard_normal((frames, channels))
    kernel = np.ones(17, dtype=np.float64) / 17.0
    for channel in range(channels):
        noise[:, channel] = np.convolve(noise[:, channel], kernel, mode="same")
    return noise


def _layer(
    path: Path,
    frames: int,
    channels: int,
    frequency: float,
    seed: int,
    amplitude: float,
) -> None:
    rng = np.random.default_rng(seed)
    time = np.arange(frames, dtype=np.float64) / SAMPLE_RATE
    tone = np.sin(2.0 * np.pi * frequency * time)
    slow = 0.65 + 0.35 * np.sin(2.0 * np.pi * 0.071 * time + seed * 0.1)
    noise = _noise(rng, frames, channels)
    signal = slow[:, None] * (0.34 * tone[:, None] + 0.66 * noise)
    signal *= amplitude / max(float(np.max(np.abs(signal))), 1e-12)
    sf.write(path, signal, SAMPLE_RATE, subtype="FLOAT", format="WAV")


def generate(output: Path, duration: float) -> None:
    layers_dir = output / "layers"
    layers_dir.mkdir(parents=True, exist_ok=True)
    _layer(layers_dir / "forest_wind.wav", 72017, 2, 143.0, 1001, 0.55)
    _layer(layers_dir / "forest_insects.wav", 37003, 1, 2870.0, 1002, 0.25)
    _layer(layers_dir / "forest_stream.wav", 54311, 2, 73.0, 1003, 0.46)
    _layer(layers_dir / "snow_wind.wav", 91009, 2, 191.0, 2001, 0.52)
    _layer(layers_dir / "snow_crystals.wav", 110017, 1, 3490.0, 2002, 0.22)

    transition = min(4.0, duration * 0.32)
    fade_end = min(duration, transition + min(4.0, duration * 0.34))
    project = {
        "schema_version": 1,
        "sample_rate": SAMPLE_RATE,
        "duration_seconds": duration,
        "output_channels": "stereo",
        "target_loudness_lufs": -23.0,
        "biomes": [
            {
                "name": "forest",
                "weight": 1.0,
                "layers": [
                    {
                        "name": "wind",
                        "path": "layers/forest_wind.wav",
                        "loop_start": 0,
                        "loop_end": 72017,
                        "base_gain_db": 0.0,
                        "curve": [
                            {"time": 0.0, "gain_db": 0.0},
                            {"time": transition, "gain_db": -1.5},
                            {"time": fade_end, "gain_db": -9.0},
                            {"time": duration, "gain_db": -12.0},
                        ],
                    },
                    {
                        "name": "insects",
                        "path": "layers/forest_insects.wav",
                        "loop_start": 11,
                        "loop_end": 37003,
                        "base_gain_db": 1.0,
                        "curve": [
                            {"time": 0.0, "gain_db": 1.5},
                            {"time": transition, "gain_db": 0.0},
                            {"time": fade_end, "gain_db": -10.0},
                            {"time": duration, "gain_db": -14.0},
                        ],
                    },
                    {
                        "name": "stream",
                        "path": "layers/forest_stream.wav",
                        "loop_start": 31,
                        "loop_end": 54311,
                        "base_gain_db": 0.0,
                        "curve": [
                            {"time": 0.0, "gain_db": 0.0},
                            {"time": transition, "gain_db": -1.0},
                            {"time": fade_end, "gain_db": -8.0},
                            {"time": duration, "gain_db": -11.0},
                        ],
                    },
                ],
            },
            {
                "name": "snow",
                "weight": 1.0,
                "layers": [
                    {
                        "name": "snow_wind",
                        "path": "layers/snow_wind.wav",
                        "loop_start": 7,
                        "loop_end": 91009,
                        "base_gain_db": 0.0,
                        "curve": [
                            {"time": 0.0, "gain_db": -14.0},
                            {"time": transition, "gain_db": -12.0},
                            {"time": fade_end, "gain_db": -2.0},
                            {"time": duration, "gain_db": 0.0},
                        ],
                    },
                    {
                        "name": "ice",
                        "path": "layers/snow_crystals.wav",
                        "loop_start": 13,
                        "loop_end": 110017,
                        "base_gain_db": 1.0,
                        "curve": [
                            {"time": 0.0, "gain_db": -18.0},
                            {"time": transition, "gain_db": -15.0},
                            {"time": fade_end, "gain_db": -4.0},
                            {"time": duration, "gain_db": -1.0},
                        ],
                    },
                ],
            },
        ],
    }
    project_path = output / "project.json"
    project_path.write_text(
        json.dumps(project, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    print(project_path)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", required=True)
    parser.add_argument("--duration", type=float, default=12.0)
    args = parser.parse_args()
    if args.duration <= 0:
        parser.error("--duration must be positive")
    generate(Path(args.output).resolve(), args.duration)


if __name__ == "__main__":
    main()
