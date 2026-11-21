"""Strongly typed scenario records."""

from __future__ import annotations

from dataclasses import dataclass, field
from decimal import Decimal
from pathlib import Path
from typing import Any


@dataclass(frozen=True)
class ReplayConfig:
    physical_steps: int
    fixed_point_decimals: int
    parallelism: int
    memory_budget_bytes: int
    degradation_sequence: tuple[Decimal, ...]
    crack_max_iterations: int
    crack_propagation_ratio: Decimal
    revocation_window_steps: int
    damage_model_version: str
    baseline_version: str
    quantization_mode: str = "half_even"


@dataclass(frozen=True)
class Bounds:
    min_x: Decimal
    min_y: Decimal
    max_x: Decimal
    max_y: Decimal

    def contains(self, x: Decimal, y: Decimal) -> bool:
        return self.min_x <= x <= self.max_x and self.min_y <= y <= self.max_y


@dataclass(frozen=True)
class Neighbor:
    cover_id: str
    distance: Decimal


@dataclass(frozen=True)
class CoverSpec:
    cover_id: str
    record_index: int
    bounds: Bounds
    material_hardness: Decimal
    max_durability: Decimal
    segment_damage_threshold: Decimal
    attenuation_radius: Decimal
    neighbors: tuple[Neighbor, ...]
    temporary_buffer_bytes: int


@dataclass(frozen=True)
class Shot:
    index: int
    player_id: str
    client_seq: int
    local_timestamp: Decimal
    arrival_seq: int
    hit_x: Decimal
    hit_y: Decimal
    incidence_angle: Decimal
    momentum: Decimal
    effective_step: int
    cover_id: str

    @property
    def key(self) -> str:
        return f"{self.player_id}:{self.client_seq}"


@dataclass(frozen=True)
class Revoke:
    index: int
    player_id: str
    client_seq: int
    arrival_seq: int
    effective_step: int
    source: str = "match"

    @property
    def key(self) -> str:
        return f"{self.player_id}:{self.client_seq}"


@dataclass(frozen=True)
class Scenario:
    config: ReplayConfig
    covers: tuple[CoverSpec, ...]
    shots: tuple[Shot, ...]
    revokes: tuple[Revoke, ...]
    source_dir: Path
    source_files: dict[str, Path] = field(default_factory=dict)
    baseline_path: Path | None = None

    @property
    def cover_by_id(self) -> dict[str, CoverSpec]:
        return {cover.cover_id: cover for cover in self.covers}

    @property
    def shot_by_key(self) -> dict[str, Shot]:
        return {shot.key: shot for shot in self.shots}

    def normalized(self) -> dict[str, Any]:
        config = self.config
        return {
            "config": {
                "physical_steps": config.physical_steps,
                "fixed_point_decimals": config.fixed_point_decimals,
                "parallelism": config.parallelism,
                "memory_budget_bytes": config.memory_budget_bytes,
                "degradation_sequence": [str(value) for value in config.degradation_sequence],
                "crack_max_iterations": config.crack_max_iterations,
                "crack_propagation_ratio": str(config.crack_propagation_ratio),
                "revocation_window_steps": config.revocation_window_steps,
                "damage_model_version": config.damage_model_version,
                "baseline_version": config.baseline_version,
                "quantization_mode": config.quantization_mode,
            },
            "covers": [
                {
                    "id": cover.cover_id,
                    "bounds": {
                        "min_x": str(cover.bounds.min_x),
                        "min_y": str(cover.bounds.min_y),
                        "max_x": str(cover.bounds.max_x),
                        "max_y": str(cover.bounds.max_y),
                    },
                    "material_hardness": str(cover.material_hardness),
                    "max_durability": str(cover.max_durability),
                    "segment_damage_threshold": str(cover.segment_damage_threshold),
                    "attenuation_radius": str(cover.attenuation_radius),
                    "neighbors": [
                        {"cover_id": neighbor.cover_id, "distance": str(neighbor.distance)}
                        for neighbor in cover.neighbors
                    ],
                    "temporary_buffer_bytes": cover.temporary_buffer_bytes,
                }
                for cover in self.covers
            ],
            "shots": [
                {
                    "player_id": shot.player_id,
                    "client_seq": shot.client_seq,
                    "local_timestamp": str(shot.local_timestamp),
                    "arrival_seq": shot.arrival_seq,
                    "hit_position": {"x": str(shot.hit_x), "y": str(shot.hit_y)},
                    "incidence_angle": str(shot.incidence_angle),
                    "momentum": str(shot.momentum),
                    "effective_step": shot.effective_step,
                }
                for shot in self.shots
            ],
            "revokes": [
                {
                    "player_id": revoke.player_id,
                    "client_seq": revoke.client_seq,
                    "arrival_seq": revoke.arrival_seq,
                    "effective_step": revoke.effective_step,
                }
                for revoke in self.revokes
            ],
        }

