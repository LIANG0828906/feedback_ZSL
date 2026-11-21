"""Deterministic local sample and baseline generator."""

from __future__ import annotations

import copy
import os
import random
from decimal import Decimal
from typing import Any

from .baseline import generate_baseline
from .engine import run_recording
from .numeric import VelocityCache
from .validation import atomic_write_json, parse_recording


def _dec_text(numerator: int, scale: int = 10_000) -> str:
    value = Decimal(numerator) / Decimal(scale)
    text = format(value, "f")
    if "." in text:
        text = text.rstrip("0").rstrip(".")
    return text or "0"


def _sample_case(
    *,
    match_id: str,
    source_version: str,
    player_count: int,
    total_steps: int,
    seed: int,
    disorder: int,
    param_decimals: int,
    accum_decimals: int,
    exact_dual: bool = False,
) -> dict[str, Any]:
    rng = random.Random(seed)
    if exact_dual:
        timestep = "0.125"
        flip_threshold = "100"
        resource_threshold = "100"
        speeds = ["0.25", "0.5", "0.75", "1"]
        resources = ["0.125", "0.25", "0.5"]
    else:
        timestep = "0.1"
        flip_threshold = "1"
        resource_threshold = "0.002"
        speeds = None
        resources = None

    entities = []
    for player in range(player_count):
        if exact_dual:
            initial_x = format(Decimal(rng.randint(-8, 8)) / Decimal(8), "f")
            initial_y = format(Decimal(rng.randint(-8, 8)) / Decimal(8), "f")
        else:
            initial_x = _dec_text(rng.randint(-1000, 1000))
            initial_y = _dec_text(rng.randint(-1000, 1000))
        entity = {
            "id": f"p{player + 1}",
            "position": {
                "x": initial_x,
                "y": initial_y,
            },
            "velocity": {"x": "0", "y": "0"},
            "heading": "0",
        }
        if source_version == "v2":
            entity["alive"] = True
        entities.append(entity)

    operations: list[dict[str, Any]] = []
    arrival_seq = 0
    op_index = 0
    per_player = max(3, total_steps // 2)
    for player in range(player_count):
        entity_id = f"p{player + 1}"
        for client_seq in range(per_player):
            effective_step = rng.randint(1, total_steps)
            if exact_dual:
                speed = speeds[(client_seq + player) % len(speeds)]
                heading = "0"
                resource_delta = resources[(client_seq + player) % len(resources)]
            else:
                speed = _dec_text(rng.randint(2_000, 5_000))
                heading = _dec_text(rng.randint(-300, 300))
                resource_delta = _dec_text(rng.randint(-900, 900))
            operations.append(
                {
                    "id": f"op-{op_index + 1:04d}",
                    "entity_id": entity_id,
                    "client_seq": client_seq,
                    "arrival_seq": arrival_seq,
                    "arrival_step": effective_step,
                    "effective_step": effective_step,
                    "speed": speed,
                    "heading": heading,
                    "resource_delta": resource_delta,
                }
            )
            arrival_seq += 1
            op_index += 1

    # Simulate network disorder by shuffling the protocol array.  The parser
    # must recover the stable (effective_step, arrival_seq, client_seq) order.
    for _ in range(disorder):
        if len(operations) > 1:
            left = rng.randrange(len(operations))
            right = rng.randrange(len(operations))
            operations[left], operations[right] = operations[right], operations[left]

    return {
        "recording_version": source_version,
        "match_id": match_id,
        "config": {
            "total_steps": total_steps,
            "timestep": timestep,
            "flip_threshold": flip_threshold,
            "resource_diff_threshold": resource_threshold,
            "coord_limit": "1000",
            "fixed_param_decimals": param_decimals,
            "fixed_accum_decimals": accum_decimals,
            "late_window_steps": 1,
            "alive_default": True,
            "tolerances": {
                "position": "0.000001",
                "velocity": "0.000001",
                "heading": "0.000001",
                "resource": "0.000001",
            },
        },
        "initial_state": {
            "room": {"resource": "0"},
            "entities": entities,
        },
        "operations": operations,
    }


def _mismatch_baseline(baseline: dict[str, Any]) -> dict[str, Any]:
    payload = copy.deepcopy(baseline)
    frames = payload["frames"]
    if frames:
        frame = frames[min(10, len(frames) - 1)]
        entities = frame["state"].get("entities", [])
        if entities:
            entities[0]["alive"] = not bool(entities[0].get("alive"))
        frame["state_hash"] = "0" * 64
        # Missing operation association must disable fork-cause inference.
        frame.pop("effective_ops", None)
        frame.pop("effective_ops_detail", None)
    return payload


def generate_samples(out_dir: str, *, seed: int = 20261122) -> dict[str, Any]:
    os.makedirs(out_dir, exist_ok=True)
    cases = [
        {
            "name": "case-01-v2-mixed",
            "match_id": "match-01-v2-mixed",
            "source_version": "v2",
            "players": 3,
            "steps": 48,
            "disorder": 24,
            "param_decimals": 3,
            "accum_decimals": 5,
            "offset": 11,
        },
        {
            "name": "case-02-v1-upgrade",
            "match_id": "match-02-v1-upgrade",
            "source_version": "v1",
            "players": 2,
            "steps": 36,
            "disorder": 12,
            "param_decimals": 4,
            "accum_decimals": 6,
            "offset": 23,
        },
        {
            "name": "case-03-v2-precision",
            "match_id": "match-03-v2-precision",
            "source_version": "v2",
            "players": 4,
            "steps": 60,
            "disorder": 40,
            "param_decimals": 2,
            "accum_decimals": 4,
            "offset": 37,
        },
        {
            "name": "case-04-exact-dual",
            "match_id": "match-04-exact-dual",
            "source_version": "v2",
            "players": 2,
            "steps": 32,
            "disorder": 12,
            "param_decimals": 4,
            "accum_decimals": 6,
            "offset": 51,
            "exact_dual": True,
        },
    ]
    manifest: dict[str, Any] = {
        "generator_version": "samples-v1",
        "seed": seed,
        "cases": [],
    }
    for case in cases:
        case_dir = os.path.join(out_dir, case["name"])
        os.makedirs(case_dir, exist_ok=True)
        raw = _sample_case(
            match_id=case["match_id"],
            source_version=case["source_version"],
            player_count=case["players"],
            total_steps=case["steps"],
            seed=seed + case["offset"],
            disorder=case["disorder"],
            param_decimals=case["param_decimals"],
            accum_decimals=case["accum_decimals"],
            exact_dual=bool(case.get("exact_dual")),
        )
        recording_path = os.path.join(case_dir, "recording.json")
        atomic_write_json(recording_path, raw)
        recording = parse_recording(raw)
        fixed = run_recording(recording, "fixed", cache=VelocityCache())
        baseline = generate_baseline(fixed)
        baseline_path = os.path.join(case_dir, "baseline.json")
        atomic_write_json(baseline_path, baseline)
        mismatch_path = os.path.join(case_dir, "baseline-mismatch.json")
        atomic_write_json(mismatch_path, _mismatch_baseline(baseline))
        manifest["cases"].append(
            {
                "name": case["name"],
                "match_id": case["match_id"],
                "source_version": case["source_version"],
                "players": case["players"],
                "total_steps": case["steps"],
                "disorder_swaps": case["disorder"],
                "fixed_param_decimals": case["param_decimals"],
                "fixed_accum_decimals": case["accum_decimals"],
                "exact_dual": bool(case.get("exact_dual")),
                "recording": os.path.relpath(recording_path, out_dir),
                "baseline": os.path.relpath(baseline_path, out_dir),
                "baseline_mismatch": os.path.relpath(mismatch_path, out_dir),
                "derived_fields": recording.derived_fields,
                "operation_count": len(recording.operations),
                "baseline_final_state_hash": fixed.final_state_hash,
                "baseline_trajectory_sha256": fixed.trajectory_sha256(),
            }
        )
    manifest_path = os.path.join(out_dir, "manifest.json")
    atomic_write_json(manifest_path, manifest)
    return manifest
