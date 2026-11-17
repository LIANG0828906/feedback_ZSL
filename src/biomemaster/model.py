"""Validated project and runtime data models."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import numpy as np


@dataclass(frozen=True)
class Curve:
    times_seconds: np.ndarray
    gains_db: np.ndarray


@dataclass(frozen=True)
class LayerSpec:
    name: str
    path: Path
    relative_path: str
    loop_start: int
    loop_end: int
    base_gain_db: float
    curve: Curve
    source_channels: int
    source_frames: int
    source_sha256: str

    @property
    def loop_samples(self) -> int:
        return self.loop_end - self.loop_start


@dataclass(frozen=True)
class Layer:
    spec: LayerSpec
    loop_audio: np.ndarray


@dataclass(frozen=True)
class Biome:
    name: str
    weight: float
    layers: tuple[Layer, ...]


@dataclass(frozen=True)
class Project:
    source_path: Path
    project_sha256: str
    content_sha256: str
    sample_rate: int
    duration_seconds: float
    total_samples: int
    channels: int
    target_loudness_lufs: float
    biomes: tuple[Biome, ...]

    def layer_count_by_biome(self) -> dict[str, int]:
        return {biome.name: len(biome.layers) for biome in self.biomes}
