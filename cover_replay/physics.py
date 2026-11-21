"""Fixed-point damage model and cover runtime state."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from .config import ReplayConfig
from .model import CoverSpec, ShotSpec
from .util import cosine_units, nearest_integer_ratio


@dataclass(slots=True)
class CoverState:
    block_id: int
    durability_max: int
    accumulated_damage: int = 0
    cracked: bool = False
    cracked_step: int | None = None

    @property
    def remaining(self) -> int:
        return max(0, self.durability_max - self.accumulated_damage)

    def to_snapshot(self) -> dict[str, Any]:
        return {
            "block_id": self.block_id,
            "durability_max": self.durability_max,
            "accumulated_damage": self.accumulated_damage,
            "cracked": self.cracked,
            "cracked_step": self.cracked_step,
        }

    @classmethod
    def from_snapshot(cls, payload: dict[str, Any]) -> "CoverState":
        return cls(
            block_id=int(payload["block_id"]),
            durability_max=int(payload["durability_max"]),
            accumulated_damage=int(payload["accumulated_damage"]),
            cracked=bool(payload["cracked"]),
            cracked_step=(
                None if payload["cracked_step"] is None else int(payload["cracked_step"])
            ),
        )


def direct_damage(
    config: ReplayConfig,
    shot: ShotSpec,
    cover: CoverSpec,
    accumulated_damage: int,
) -> int:
    cosine = cosine_units(shot.incidence_angle, config.fractional_digits)
    segments = accumulated_damage // cover.segment_threshold
    softening = 100 + config.segment_softening_percent * segments
    numerator = shot.momentum * cosine * config.damage_scale * softening
    denominator = cover.hardness * 100
    return max(0, nearest_integer_ratio(numerator, denominator))


def spread_damage(config: ReplayConfig, primary_damage: int) -> int:
    return nearest_integer_ratio(
        primary_damage * config.spread_damage_percent,
        100,
    )


def neighbors(
    source: CoverSpec, covers: tuple[CoverSpec, ...]
) -> tuple[CoverSpec, ...]:
    radius_squared = source.spread_radius * source.spread_radius
    return tuple(
        cover
        for cover in covers
        if cover.block_id != source.block_id
        and source.rectangle_distance_squared(cover) <= radius_squared
    )


def apply_damage(state: CoverState, amount: int) -> int:
    effective = max(0, min(amount, state.remaining))
    state.accumulated_damage += effective
    return effective
