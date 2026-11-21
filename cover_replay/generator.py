"""Deterministic local match, baseline, and trajectory generation."""

from __future__ import annotations

import hashlib
import json
import math
import random
from pathlib import Path
from typing import Any

from .baseline import build_baseline
from .config import parse_config
from .engine import ReplayEngine, ReplayRun
from .model import parse_record
from .util import atomic_write_bytes, atomic_write_json, canonical_json, from_units, sha256_obj

PROFILE_SPECS: dict[str, dict[str, Any]] = {
    "small": {
        "covers": 5,
        "players": 2,
        "shots": 80,
        "physics_steps": 16,
        "disorder": 0.20,
        "max_workers": 2,
        "memory_budget_per_step": 6,
        "full_scan_cost_limit": 8192,
        "crack_propagation_damage": "1.5",
    },
    "medium": {
        "covers": 12,
        "players": 4,
        "shots": 300,
        "physics_steps": 24,
        "disorder": 0.55,
        "max_workers": 3,
        "memory_budget_per_step": 9,
        "full_scan_cost_limit": 8192,
        "crack_propagation_damage": "1.5",
    },
    "stress": {
        "covers": 24,
        "players": 6,
        "shots": 900,
        "physics_steps": 30,
        "disorder": 0.85,
        "max_workers": 4,
        "memory_budget_per_step": 14,
        "full_scan_cost_limit": 8192,
        "crack_propagation_damage": "1.5",
    },
    "conflict": {
        "covers": 4,
        "players": 2,
        "shots": 60,
        "physics_steps": 12,
        "disorder": 0.35,
        "max_workers": 2,
        "memory_budget_per_step": 5,
        "full_scan_cost_limit": 8192,
        "crack_propagation_damage": "1.5",
        "expected_error": "E_CREDIT_CONFLICT",
    },
}


def _profile_rng(seed: int, profile: str) -> random.Random:
    digest = hashlib.sha256(f"{seed}:{profile}".encode("utf-8")).digest()
    return random.Random(int.from_bytes(digest[:8], "big"))


def _config_payload(profile: str, spec: dict[str, Any]) -> dict[str, Any]:
    return {
        "physics_steps": spec["physics_steps"],
        "fractional_digits": 3,
        "max_workers": spec["max_workers"],
        "memory_budget_per_step": spec["memory_budget_per_step"],
        "memory_units_per_block": 1,
        "damage_scale": 100,
        "spread_damage_percent": 25,
        "segment_softening_percent": 10,
        "crack_threshold_segments": 2,
        "crack_propagation_damage": spec["crack_propagation_damage"],
        "max_crack_iterations": 16,
        "strategy": "adaptive",
        "full_scan_cost_limit": spec["full_scan_cost_limit"],
        "model_version": "cover-damage-v1",
        "baseline_version": "baseline-v1",
        "snapshot_version": 1,
    }


def _covers_payload(rng: random.Random, count: int) -> list[dict[str, Any]]:
    columns = max(1, math.ceil(math.sqrt(count)))
    rows = math.ceil(count / columns)
    width = 80.0
    height = 50.0
    gap = 35.0
    origin_x = 100.0
    origin_y = 80.0
    covers: list[dict[str, Any]] = []
    for index in range(count):
        column = index % columns
        row = index // columns
        x = origin_x + column * (width + gap)
        y = origin_y + row * (height + gap)
        covers.append(
            {
                "block_id": index + 1,
                "x": round(x, 3),
                "y": round(y, 3),
                "width": width,
                "height": height,
                "material_hardness": float(rng.randint(75, 135)),
                "durability_max": float(rng.randint(900, 1800)),
                "segment_damage_threshold": float(rng.randint(120, 240)),
                "spread_radius": float(rng.randint(55, 125)),
                "memory_units": rng.choice((1, 1, 2)),
            }
        )
    return covers


def _shots_payload(
    rng: random.Random,
    covers: list[dict[str, Any]],
    players: int,
    shot_count: int,
    physics_steps: int,
    disorder: float,
) -> list[dict[str, Any]]:
    sequences = {f"P{index + 1:02d}": 0 for index in range(players)}
    logical: list[dict[str, Any]] = []
    for ordinal in range(shot_count):
        player_id = f"P{rng.randint(1, players):02d}"
        cover = rng.choice(covers)
        client_seq = sequences[player_id]
        sequences[player_id] += 1
        hit_x = round(
            float(cover["x"]) + rng.uniform(4.0, float(cover["width"]) - 4.0), 3
        )
        hit_y = round(
            float(cover["y"]) + rng.uniform(4.0, float(cover["height"]) - 4.0), 3
        )
        logical.append(
            {
                "player_id": player_id,
                "client_seq": client_seq,
                "local_timestamp": f"t+{ordinal * 37:08d}",
                "arrival_seq": 0,
                "block_id": cover["block_id"],
                "hit_position": {"x": hit_x, "y": hit_y},
                "incidence_angle": round(rng.uniform(0.0, 72.0), 3),
                "momentum": round(rng.uniform(24.0, 62.0), 3),
                "declared_step": 1,
            }
        )

    arrival_order = list(range(shot_count))
    if disorder > 0:
        swapped = 0
        swaps = int(shot_count * disorder)
        for _ in range(swaps):
            left = rng.randrange(shot_count)
            right = rng.randrange(shot_count)
            arrival_order[left], arrival_order[right] = (
                arrival_order[right],
                arrival_order[left],
            )
            swapped += 1
    for arrival_seq, logical_index in enumerate(arrival_order):
        step = min(
            physics_steps,
            max(1, math.ceil((arrival_seq + 1) * physics_steps / shot_count)),
        )
        logical[logical_index]["arrival_seq"] = arrival_seq
        logical[logical_index]["declared_step"] = step
    logical.sort(key=lambda shot: (shot["player_id"], shot["client_seq"]))
    return logical


def _record_payload(profile: str, spec: dict[str, Any], seed: int) -> dict[str, Any]:
    rng = _profile_rng(seed, profile)
    covers = _covers_payload(rng, int(spec["covers"]))
    shots = _shots_payload(
        rng,
        covers,
        int(spec["players"]),
        int(spec["shots"]),
        int(spec["physics_steps"]),
        float(spec["disorder"]),
    )
    return {
        "record_version": 1,
        "match_id": f"local-{profile}-{seed}",
        "model_version": "cover-damage-v1",
        "baseline_version": "baseline-v1",
        "covers": covers,
        "shots": shots,
    }


def _inject_conflict(
    record_payload: dict[str, Any],
    config_payload: dict[str, Any],
    clean_run: ReplayRun,
) -> None:
    config = parse_config(config_payload)
    for frame in clean_run.frames:
        for credit in frame.get("credit_entries", []):
            if credit["status"] != "effective" or credit["authoritative_damage"] == "0.000":
                continue
            key = (credit["player_id"], credit["client_sequence"])
            for shot in record_payload["shots"]:
                if (shot["player_id"], shot["client_seq"]) != key:
                    continue
                authoritative = int(
                    round(float(credit["authoritative_damage"]) * config.fixed_scale)
                )
                shot["predicted_damage"] = from_units(
                    authoritative + config.fixed_scale // 2,
                    config.fractional_digits,
                )
                return
    raise RuntimeError("could not locate an effective shot for conflict injection")


def _trajectory_lines(run: ReplayRun) -> bytes:
    lines: list[str] = []
    for frame in run.frames:
        budget = frame["budget_usage"]
        lines.append(
            canonical_json(
                {
                    "step": frame["step"],
                    "effective_client_sequences": frame["effective_client_sequences"],
                    "effective_shots": frame["effective_shots"],
                    "state_hash": frame["state_hash"],
                    "queue_watermark": budget["queue_watermark"],
                    "peak_memory_used": budget["peak_memory_used"],
                    "crack_events": frame["crack_events"],
                }
            )
        )
    return ("\n".join(lines) + ("\n" if lines else "")).encode("utf-8")


def generate_profile(
    out_dir: str | Path,
    profile: str,
    *,
    seed: int = 20261122,
) -> dict[str, Any]:
    if profile not in PROFILE_SPECS:
        raise ValueError(f"unknown profile {profile}")
    spec = PROFILE_SPECS[profile]
    root = Path(out_dir) / profile
    root.mkdir(parents=True, exist_ok=True)
    config_payload = _config_payload(profile, spec)
    record_payload = _record_payload(profile, spec, seed)
    config = parse_config(config_payload)
    record = parse_record(record_payload, config)
    clean_engine = ReplayEngine(record, config, allow_conflicts=False)
    clean_run = clean_engine.run()
    expected_error = spec.get("expected_error")
    conflict_run: ReplayRun | None = None
    if expected_error:
        _inject_conflict(record_payload, config_payload, clean_run)
        conflict_record = parse_record(record_payload, config)
        conflict_run = ReplayEngine(
            conflict_record, config, allow_conflicts=True
        ).run()
        if not any(
            conflict.code == expected_error for conflict in conflict_run.conflicts
        ):
            raise RuntimeError("conflict sample did not produce the expected error")

    record_bytes = (canonical_json(record_payload) + "\n").encode("utf-8")
    config_bytes = (canonical_json(config_payload) + "\n").encode("utf-8")
    trajectory = _trajectory_lines(clean_run)
    atomic_write_bytes(root / "match.json", record_bytes)
    atomic_write_bytes(root / "config.json", config_bytes)
    atomic_write_bytes(root / "trajectory.jsonl", trajectory)
    baseline_meta: dict[str, Any] | None = None
    if expected_error is None:
        baseline = build_baseline(clean_run)
        atomic_write_json(root / "baseline.json", baseline)
        baseline_meta = {
            "path": "baseline.json",
            "digest": baseline["baseline_digest"],
        }
    expected = {
        "profile": profile,
        "seed": seed,
        "record_sha256": hashlib.sha256(record_bytes).hexdigest(),
        "config_sha256": hashlib.sha256(config_bytes).hexdigest(),
        "trajectory_sha256": hashlib.sha256(trajectory).hexdigest(),
        "baseline": baseline_meta,
        "final_state_hash": clean_run.final_state_hash,
        "strategy": clean_run.strategy,
        "frame_count": len(clean_run.frames),
        "expected_error": expected_error,
        "conflict": (
            conflict_run.conflicts[0].to_dict()
            if conflict_run is not None and conflict_run.conflicts
            else None
        ),
    }
    atomic_write_json(root / "expected.json", expected)
    return expected


def generate_samples(
    out_dir: str | Path,
    *,
    seed: int = 20261122,
    profiles: tuple[str, ...] | list[str] | None = None,
) -> dict[str, Any]:
    chosen = tuple(profiles) if profiles is not None else tuple(PROFILE_SPECS)
    results = {
        profile: generate_profile(out_dir, profile, seed=seed)
        for profile in chosen
    }
    manifest = {
        "seed": seed,
        "profiles": list(chosen),
        "entries": results,
    }
    atomic_write_json(Path(out_dir) / "manifest.json", manifest)
    return manifest
