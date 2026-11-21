"""Deterministic projectile and cover damage calculations."""

from __future__ import annotations

import math
from collections.abc import Mapping
from dataclasses import dataclass

from .model import MICRO, InitialState, Projectile


@dataclass(frozen=True)
class DamageResult:
    direct_cover_id: str | None
    raw_damage: Mapping[str, int]
    causes: Mapping[str, str]


def locate_hit(initial: InitialState, projectile: Projectile) -> str | None:
    for cover_id in initial.cover_ids:
        cover = initial.covers[cover_id]
        if cover.contains(projectile.hit_x, projectile.hit_y):
            return cover_id
    return None


def compute_damage(initial: InitialState, projectile: Projectile) -> DamageResult:
    """Compute direct and chain damage without consulting mutable runtime state.

    Incident angle is measured from the surface normal. The raw damage is a
    fixed-point quantity. Neighbouring covers receive attenuated damage and may
    propagate a further chain hop only when the spread exceeds their segment
    threshold.
    """

    direct_cover_id = locate_hit(initial, projectile)
    if direct_cover_id is None:
        return DamageResult(None, {}, {})

    cover = initial.covers[direct_cover_id]
    cosine = max(0.0, math.cos(math.radians(projectile.incident_angle_deg)))
    direct = round(projectile.momentum * cosine / cover.hardness * MICRO)
    raw: dict[str, int] = {}
    causes: dict[str, str] = {}
    if direct > 0:
        raw[direct_cover_id] = direct
        causes[direct_cover_id] = "direct"

    if direct <= 0 or initial.spread_radius <= 0:
        return DamageResult(direct_cover_id, raw, causes)

    frontier: list[tuple[str, int, int]] = [(direct_cover_id, direct, 0)]
    visited = {direct_cover_id}
    while frontier:
        source_id, source_damage, depth = frontier.pop(0)
        if depth >= initial.max_chain_depth:
            continue
        source_center = initial.covers[source_id].center
        neighbours: list[tuple[float, str]] = []
        for neighbour_id in initial.cover_ids:
            if neighbour_id == source_id:
                continue
            centre = initial.covers[neighbour_id].center
            distance = math.hypot(centre[0] - source_center[0], centre[1] - source_center[1])
            if distance <= initial.spread_radius:
                neighbours.append((distance, neighbour_id))
        for distance, neighbour_id in sorted(neighbours):
            if neighbour_id in visited:
                continue
            attenuation = initial.chain_decay * max(
                0.0, 1.0 - distance / initial.spread_radius
            )
            spread = round(source_damage * attenuation)
            if spread <= 0:
                continue
            visited.add(neighbour_id)
            raw[neighbour_id] = spread
            causes[neighbour_id] = "chain"
            if spread >= initial.covers[neighbour_id].segment_threshold:
                frontier.append((neighbour_id, spread, depth + 1))
    return DamageResult(direct_cover_id, raw, causes)


def prediction_from_initial(initial: InitialState, projectile: Projectile) -> dict[str, int]:
    """A deterministic client-side prediction built from the initial snapshot."""

    result = compute_damage(initial, projectile)
    return dict(result.raw_damage)

