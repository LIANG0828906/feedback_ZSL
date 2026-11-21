from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from cover_replay.loader import load_scenario
from cover_replay.snapshot import atomic_write_bytes

DEFAULT_CONFIG: dict[str, Any] = {
    "schema_version": 1,
    "physical_steps": 4,
    "fixed_point_decimals": 3,
    "parallelism": 2,
    "memory_budget_bytes": 65536,
    "degradation_sequence": ["1.0", "0.75", "0.5", "0.25"],
    "crack_max_iterations": 16,
    "crack_propagation_ratio": "1.0",
    "revocation_window_steps": 2,
    "damage_model_version": "cover-damage-v1",
    "baseline_version": "baseline-v1",
    "quantization_mode": "half_even",
}


def cover(
    cover_id: str = "C01",
    *,
    min_x: str = "0",
    min_y: str = "0",
    max_x: str = "10",
    max_y: str = "10",
    hardness: str = "1.0",
    durability: str = "100",
    threshold: str = "20",
    radius: str = "20",
    neighbors: list[dict[str, str]] | None = None,
    buffer_bytes: int = 2048,
) -> dict[str, Any]:
    return {
        "id": cover_id,
        "bounds": {
            "min_x": min_x,
            "min_y": min_y,
            "max_x": max_x,
            "max_y": max_y,
        },
        "material_hardness": hardness,
        "max_durability": durability,
        "segment_damage_threshold": threshold,
        "attenuation_radius": radius,
        "neighbors": neighbors or [],
        "temporary_buffer_bytes": buffer_bytes,
    }


def shot(
    player_id: str = "P1",
    client_seq: int = 0,
    *,
    arrival_seq: int = 1,
    x: str = "5",
    y: str = "5",
    angle: str = "0",
    momentum: str = "5",
    step: int = 0,
    cover_id: str | None = "C01",
) -> dict[str, Any]:
    result: dict[str, Any] = {
        "player_id": player_id,
        "client_seq": client_seq,
        "local_timestamp": step * 1000 + arrival_seq,
        "arrival_seq": arrival_seq,
        "hit_position": {"x": x, "y": y},
        "incidence_angle": angle,
        "momentum": momentum,
        "effective_step": step,
    }
    if cover_id is not None:
        result["cover_id"] = cover_id
    return result


def revoke(
    player_id: str = "P1",
    client_seq: int = 0,
    *,
    arrival_seq: int = 100,
    step: int = 1,
) -> dict[str, Any]:
    return {
        "player_id": player_id,
        "client_seq": client_seq,
        "arrival_seq": arrival_seq,
        "effective_step": step,
    }


def write_json(path: Path, value: Any) -> None:
    atomic_write_bytes(
        path,
        (
            json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2, allow_nan=False)
            + "\n"
        ).encode("utf-8"),
    )


def make_scenario(
    path: Path,
    *,
    covers: list[dict[str, Any]],
    shots: list[dict[str, Any]],
    revokes: list[dict[str, Any]] | None = None,
    config: dict[str, Any] | None = None,
    clean: bool = True,
) -> Path:
    path.mkdir(parents=True, exist_ok=True)
    if clean:
        for child in path.iterdir():
            if child.is_file():
                child.unlink()
    merged_config = dict(DEFAULT_CONFIG)
    if config:
        merged_config.update(config)
    write_json(path / "config.json", merged_config)
    write_json(path / "covers.json", covers)
    write_json(
        path / "match.json",
        {
            "schema_version": 1,
            "config_file": "config.json",
            "covers_file": "covers.json",
            "shots": shots,
            "revokes": revokes or [],
        },
    )
    return path


def load(path: Path):
    return load_scenario(path)

