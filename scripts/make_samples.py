#!/usr/bin/env python3
"""Generate deterministic sample configs, hashes and injection baselines."""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from levelworkbench.config import load_config  # noqa: E402
from levelworkbench.errors import WorkbenchError  # noqa: E402
from levelworkbench.pipeline import generate_from_document  # noqa: E402


def constraint(
    constraint_id: str,
    kind: str,
    *,
    priority: int,
    hard: bool = True,
    **params: Any,
) -> dict[str, Any]:
    return {
        "id": constraint_id,
        "type": kind,
        "priority": priority,
        "hard": hard,
        **params,
    }


def document(
    *,
    seed: int,
    width: int,
    height: int,
    rooms: tuple[int, int],
    keys: list[str],
    coefficients: dict[str, float] | None = None,
    threshold: float = 0.5,
    area_target: float,
    area_tolerance: float,
    door_target: int,
    door_tolerance: int,
    room_max: tuple[int, int] = (6, 5),
    direction_mode: str = "bidirectional",
    max_attempts: int = 16,
) -> dict[str, Any]:
    coefficients = coefficients or {key: 1.0 for key in keys}
    constraints = [
        constraint(
            "rooms",
            "room_count",
            priority=100,
            min=rooms[0],
            max=rooms[1],
        ),
        constraint("connectivity", "connectivity", priority=100),
    ]
    if keys:
        constraints.extend(
            [
                constraint(
                    "key_chain",
                    "key_chain",
                    priority=100,
                    keys=keys,
                    coefficients=coefficients,
                    threshold=threshold,
                ),
                constraint("locked_endpoint", "no_lock_unreachable_end", priority=100),
            ]
        )
    constraints.extend(
        [
            constraint(
                "area_target",
                "area_target",
                priority=20,
                hard=False,
                target=area_target,
                tolerance=area_tolerance,
                coefficient=1.0,
                weight=1.0,
            ),
            constraint(
                "door_target",
                "door_count_target",
                priority=10,
                hard=False,
                target=door_target,
                tolerance=door_tolerance,
                coefficient=1.0,
                weight=1.0,
            ),
        ]
    )
    return {
        "version": 1,
        "seed": seed,
        "grid": {"width": width, "height": height},
        "generation": {
            "max_attempts": max_attempts,
            "max_phase_retries": 5,
            "room_min_width": 3,
            "room_min_height": 3,
            "room_max_width": room_max[0],
            "room_max_height": room_max[1],
            "room_margin": 1,
            "rewards": 1,
            "direction_mode": direction_mode,
        },
        "quantization": {"decimals": 2, "rounding": "half_up"},
        "evaluation": {"mode": "fixed", "uncertain_policy": "allow"},
        "limits": {"max_constraints": 64},
        "constraints": constraints,
    }


def quantized_probe() -> dict[str, Any]:
    return {
        "version": 1,
        "seed": 9001,
        "grid": {"width": 3, "height": 3},
        "generation": {
            "max_attempts": 4,
            "max_phase_retries": 3,
            "room_min_width": 3,
            "room_min_height": 3,
            "room_max_width": 3,
            "room_max_height": 3,
            "room_margin": 0,
            "rewards": 1,
            "direction_mode": "bidirectional",
        },
        "quantization": {"decimals": 2, "rounding": "half_up"},
        "evaluation": {"mode": "fixed", "uncertain_policy": "allow"},
        "limits": {"max_constraints": 64},
        "constraints": [
            constraint("rooms", "room_count", priority=100, min=1, max=1),
            constraint("connectivity", "connectivity", priority=100),
            constraint(
                "area_quantized",
                "area_target",
                priority=30,
                hard=False,
                target=2.997,
                tolerance=0.0,
                coefficient=0.333,
                weight=1.0,
            ),
        ],
    }


def conflict_document() -> dict[str, Any]:
    return {
        "version": 1,
        "seed": 51,
        "grid": {"width": 12, "height": 8},
        "generation": {
            "max_attempts": 2,
            "max_phase_retries": 2,
            "room_min_width": 3,
            "room_min_height": 3,
            "room_max_width": 5,
            "room_max_height": 4,
            "room_margin": 1,
            "rewards": 1,
            "direction_mode": "bidirectional",
        },
        "quantization": {"decimals": 2, "rounding": "half_up"},
        "evaluation": {"mode": "fixed", "uncertain_policy": "allow"},
        "limits": {"max_constraints": 64},
        "constraints": [
            constraint("rooms", "room_count", priority=100, min=1, max=1),
            constraint("connectivity", "connectivity", priority=100),
            constraint(
                "key_chain",
                "key_chain",
                priority=100,
                keys=["k1", "k2"],
                coefficients={"k1": 0.9, "k2": 0.9},
                threshold=0.5,
            ),
            constraint("locked_endpoint", "no_lock_unreachable_end", priority=100),
        ],
    }


SAMPLES = {
    "config.json": document(
        seed=20261121,
        width=28,
        height=20,
        rooms=(7, 10),
        keys=["k1", "k2"],
        coefficients={"k1": 0.95, "k2": 0.95},
        threshold=0.5,
        area_target=260,
        area_tolerance=100,
        door_target=9,
        door_tolerance=4,
        room_max=(6, 5),
        direction_mode="mixed",
    ),
    "small.json": document(
        seed=101,
        width=16,
        height=12,
        rooms=(4, 6),
        keys=["key_a"],
        coefficients={"key_a": 0.8},
        threshold=0.5,
        area_target=80,
        area_tolerance=40,
        door_target=4,
        door_tolerance=2,
    ),
    "medium_chain.json": document(
        seed=202,
        width=26,
        height=18,
        rooms=(8, 11),
        keys=["key_alpha", "key_beta", "key_gamma"],
        coefficients={
            "key_alpha": 0.9,
            "key_beta": 0.9,
            "key_gamma": 0.9,
        },
        threshold=0.6,
        area_target=260,
        area_tolerance=80,
        door_target=10,
        door_tolerance=4,
        direction_mode="mixed",
    ),
    "tight_grid.json": document(
        seed=303,
        width=18,
        height=14,
        rooms=(5, 6),
        keys=["bronze", "silver"],
        coefficients={"bronze": 0.85, "silver": 0.85},
        threshold=0.7,
        area_target=105,
        area_tolerance=25,
        door_target=7,
        door_tolerance=2,
        room_max=(5, 4),
        max_attempts=24,
    ),
    "quantized_probe.json": quantized_probe(),
    "conflict_rooms_vs_chain.json": conflict_document(),
}


def write_json(path: Path, payload: Any) -> None:
    path.write_text(
        json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def main() -> int:
    samples_dir = ROOT / "samples"
    samples_dir.mkdir(parents=True, exist_ok=True)
    for name, payload in SAMPLES.items():
        write_json(samples_dir / name, payload)

    baselines: dict[str, Any] = {
        "trace_version": "LEVEL_TRACE_V1",
        "samples": {},
    }
    for name, payload in SAMPLES.items():
        if name.startswith("conflict_"):
            try:
                generate_from_document(payload)
                baselines["samples"][name] = {"unexpected_success": True}
            except WorkbenchError as exc:
                baselines["samples"][name] = {
                    "expected_error": exc.code,
                    "stage": exc.stage,
                    "violated_constraints": exc.details.get("violated_constraints"),
                    "minimal_evidence": exc.details.get("minimal_evidence"),
                }
            continue
        result = generate_from_document(payload)
        baselines["samples"][name] = {
            "seed": payload["seed"],
            "layout_hash": result.layout_hash,
            "trace_hash": result.trace_hash,
            "seed_derivation_hash": result.seed_derivation_hash,
            "attempts": result.attempts,
            "rooms": len(result.level.rooms),
            "doors": len(result.level.doors),
            "tags": len(result.level.tags),
            "unlock_order": list(result.evaluation.reachability.unlock_order),
            "hard_passed": not result.evaluation.failed_hard,
            "failed_soft": list(result.evaluation.failed_soft),
            "uncertain": list(result.evaluation.uncertain),
            "error_report": result.evaluation.error_report,
        }
    write_json(samples_dir / "baselines.json", baselines)

    injection_base = SAMPLES["small.json"]
    write_json(samples_dir / "injection_base.json", injection_base)
    injection_cases = {
        "base": "injection_base.json",
        "cases": [
            {
                "mode": "room_bounds",
                "expected_code": "E_INJECT_ROOM_BOUNDS",
                "location": "/input_layout/rooms/0",
            },
            {
                "mode": "dangling_door",
                "expected_code": "E_INJECT_DANGLING_DOOR",
                "location": "/input_layout/doors/0",
            },
            {
                "mode": "key_cycle",
                "expected_code": "E_INJECT_KEY_CYCLE",
                "location": "/input_layout/key_dependencies",
            },
            {
                "mode": "constraint_limit",
                "expected_code": "E_INJECT_CONSTRAINT_LIMIT",
                "location": "/constraints",
            },
            {
                "mode": "truncated_input",
                "expected_code": "E_INJECT_TRUNCATED_INPUT",
                "location": "$",
            },
        ],
    }
    write_json(samples_dir / "injection_cases.json", injection_cases)

    injection_results: dict[str, Any] = {
        "base": "injection_base.json",
        "cases": {},
    }
    for case in injection_cases["cases"]:
        mode = case["mode"]
        try:
            load_config(samples_dir / "injection_base.json", injection=mode)
            injection_results["cases"][mode] = {"unexpected_success": True}
        except WorkbenchError as exc:
            injection_results["cases"][mode] = {
                "code": exc.code,
                "expected_code": case["expected_code"],
                "blocked_before_output": True,
                "injection_point": exc.injection_point,
            }
    write_json(samples_dir / "injection_baselines.json", injection_results)
    print(json.dumps(baselines, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
