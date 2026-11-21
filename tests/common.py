from __future__ import annotations

from typing import Any

from cover_replay.config import ReplayConfig, parse_config
from cover_replay.model import ReplayRecord, parse_record


def config_payload(**overrides: Any) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "physics_steps": 4,
        "fractional_digits": 3,
        "max_workers": 2,
        "memory_budget_per_step": 4,
        "memory_units_per_block": 1,
        "damage_scale": 100,
        "spread_damage_percent": 25,
        "segment_softening_percent": 0,
        "crack_threshold_segments": 1,
        "crack_propagation_damage": "0",
        "max_crack_iterations": 8,
        "strategy": "adaptive",
        "full_scan_cost_limit": 10000,
        "model_version": "cover-damage-v1",
        "baseline_version": "baseline-v1",
        "snapshot_version": 1,
    }
    payload.update(overrides)
    return payload


def cover(
    block_id: int,
    *,
    x: float | None = None,
    y: float = 0.0,
    width: float = 80.0,
    height: float = 60.0,
    hardness: float = 100.0,
    durability: float = 600.0,
    threshold: float = 150.0,
    spread_radius: float = 50.0,
    memory_units: int = 1,
) -> dict[str, Any]:
    return {
        "block_id": block_id,
        "x": float((block_id - 1) * 100 if x is None else x),
        "y": y,
        "width": width,
        "height": height,
        "material_hardness": hardness,
        "durability_max": durability,
        "segment_damage_threshold": threshold,
        "spread_radius": spread_radius,
        "memory_units": memory_units,
    }


def shot(
    shot_index: int,
    *,
    player_id: str = "P01",
    client_seq: int = 0,
    arrival_seq: int = 0,
    block_id: int = 1,
    x: float = 10.0,
    y: float = 10.0,
    angle: float = 0.0,
    momentum: float = 50.0,
    step: int = 1,
    predicted: float | None = None,
) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "player_id": player_id,
        "client_seq": client_seq,
        "local_timestamp": f"t+{shot_index:06d}",
        "arrival_seq": arrival_seq,
        "block_id": block_id,
        "hit_position": {"x": x, "y": y},
        "incidence_angle": angle,
        "momentum": momentum,
        "declared_step": step,
    }
    if predicted is not None:
        payload["predicted_damage"] = predicted
    return payload


def record_payload(
    covers: list[dict[str, Any]] | None = None,
    shots: list[dict[str, Any]] | None = None,
    **overrides: Any,
) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "record_version": 1,
        "match_id": "unit-match",
        "model_version": "cover-damage-v1",
        "baseline_version": "baseline-v1",
        "covers": covers if covers is not None else [cover(1)],
        "shots": shots if shots is not None else [shot(0)],
    }
    payload.update(overrides)
    return payload


def config(**overrides: Any) -> ReplayConfig:
    return parse_config(config_payload(**overrides))


def record(
    config_value: ReplayConfig,
    *,
    covers: list[dict[str, Any]] | None = None,
    shots: list[dict[str, Any]] | None = None,
) -> ReplayRecord:
    return parse_record(record_payload(covers=covers, shots=shots), config_value)
