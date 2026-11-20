"""Deterministic local scenario generation."""

from __future__ import annotations

import random
from pathlib import Path
from typing import Any

from .canonical import atomic_write_json, hash_object


def _fixed(text: str, scale: int) -> dict[str, Any]:
    return {"kind": "fixed", "text": text, "scale": scale}


def _float(text: str) -> dict[str, Any]:
    return {"kind": "float", "text": text}


def _condition(
    condition_id: str,
    expr: dict[str, Any],
    op: str,
    threshold: dict[str, Any],
) -> dict[str, Any]:
    return {"id": condition_id, "expr": expr, "op": op, "threshold": threshold}


def branch_flip_scenario() -> dict[str, Any]:
    """Scenario where fixed-point quantization reverses a branch outcome."""

    ratio_expr = {
        "op": "pow",
        "base": {"op": "const", "coef": _float("0.6667")},
        "exponent": 3,
    }
    return {
        "format": "causal-story/1",
        "scenario_id": "branch_flip",
        "description": "Fixed quantization raises 0.6667 to 0.67 and flips the edge branch.",
        "start_event": "start",
        "precision": {
            "mode": "fixed",
            "decimal_places": 3,
            "rounding": "half_even",
        },
        "initial_state": {
            "characters": {
                "hero": {
                    "trust": _fixed("1.000", 3),
                    "resolve": _fixed("0.500", 3),
                }
            },
            "resources": {"energy": _fixed("10", 0)},
            "flags": {"gate_open": False, "edge_reached": False},
        },
        "events": [
            {
                "id": "start",
                "preconditions": ["trust_positive"],
                "conditions": [
                    _condition(
                        "trust_positive",
                        {"op": "var", "path": "characters.hero.trust"},
                        ">",
                        _fixed("0", 0),
                    ),
                    _condition(
                        "ratio_ok",
                        ratio_expr,
                        ">=",
                        _fixed("0.2968", 4),
                    ),
                    _condition(
                        "always",
                        {"op": "const", "coef": _fixed("1", 0)},
                        ">=",
                        _fixed("0", 0),
                    ),
                ],
                "effects": [
                    {
                        "id": "open_gate",
                        "op": "set",
                        "path": "flags.gate_open",
                        "value": True,
                    }
                ],
                "branches": [
                    {
                        "id": "b_edge",
                        "to": "edge",
                        "priority": 20,
                        "condition": "ratio_ok",
                    },
                    {
                        "id": "b_fallback",
                        "to": "fallback",
                        "priority": 0,
                        "condition": "always",
                    },
                ],
            },
            {
                "id": "edge",
                "preconditions": ["ratio_ok", "always"],
                "conditions": [
                    _condition(
                        "ratio_ok",
                        ratio_expr,
                        ">=",
                        _fixed("0.2968", 4),
                    ),
                    _condition(
                        "always",
                        {"op": "const", "coef": _fixed("1", 0)},
                        ">=",
                        _fixed("0", 0),
                    ),
                ],
                "effects": [
                    {
                        "id": "edge_trust",
                        "op": "add",
                        "path": "characters.hero.trust",
                        "coef": _fixed("0.050", 3),
                    },
                    {
                        "id": "edge_mark",
                        "op": "set",
                        "path": "flags.edge_reached",
                        "value": True,
                    },
                ],
                "branches": [],
            },
            {
                "id": "fallback",
                "preconditions": ["always"],
                "conditions": [
                    _condition(
                        "always",
                        {"op": "const", "coef": _fixed("1", 0)},
                        ">=",
                        _fixed("0", 0),
                    )
                ],
                "effects": [
                    {
                        "id": "fallback_trust",
                        "op": "sub",
                        "path": "characters.hero.trust",
                        "coef": _fixed("0.100", 3),
                    }
                ],
                "branches": [],
            },
        ],
    }


def deterministic_profile(
    scenario_id: str,
    seed: int,
    event_count: int,
    branch_width: int,
    decimal_places: int,
) -> dict[str, Any]:
    rng = random.Random(seed)
    events: list[dict[str, Any]] = []
    for index in range(event_count):
        event_id = f"e{index:03d}"
        conditions = [
            _condition(
                "entry_guard",
                {"op": "var", "path": "characters.hero.trust"},
                ">=",
                _fixed("-100", 0),
            )
        ]
        branches: list[dict[str, Any]] = []
        if index < event_count - 1:
            conditions.append(
                _condition(
                    "proceed",
                    {
                        "op": "add",
                        "left": {"op": "var", "path": "characters.hero.trust"},
                        "right": {"op": "const", "coef": _fixed("0.000", decimal_places)},
                    },
                    ">=",
                    _fixed("-100", 0),
                )
            )
            conditions.append(
                _condition(
                    "surge",
                    {
                        "op": "pow",
                        "base": {"op": "const", "coef": _fixed("0.800", decimal_places)},
                        "exponent": 2,
                    },
                    ">=",
                    _fixed("0.640", decimal_places),
                )
            )
            branches.append(
                {
                    "id": f"b{index:03d}_primary",
                    "to": f"e{index + 1:03d}",
                    "priority": 100,
                    "condition": "proceed",
                }
            )
            max_width = min(branch_width, event_count - index - 1)
            for offset in range(2, max_width + 1):
                branches.append(
                    {
                        "id": f"b{index:03d}_span{offset}",
                        "to": f"e{index + offset:03d}",
                        "priority": 100 - offset,
                        "condition": "surge",
                    }
                )
        delta = rng.randint(1, 19)
        effects: list[dict[str, Any]] = [
            {
                "id": f"fx{index:03d}_trust",
                "op": "add",
                "path": "characters.hero.trust",
                "coef": _fixed(f"0.{delta:03d}", decimal_places),
            },
            {
                "id": f"fx{index:03d}_steps",
                "op": "add",
                "path": "resources.steps",
                "coef": _fixed("1", 0),
            },
        ]
        if index == 0:
            effects.append(
                {
                    "id": "start_mark",
                    "op": "set",
                    "path": "flags.started",
                    "value": True,
                }
            )
        events.append(
            {
                "id": event_id,
                "preconditions": ["entry_guard"],
                "conditions": conditions,
                "effects": effects,
                "branches": branches,
            }
        )
    return {
        "format": "causal-story/1",
        "scenario_id": scenario_id,
        "description": "Deterministic generated narrative profile.",
        "start_event": "e000",
        "precision": {
            "mode": "fixed",
            "decimal_places": decimal_places,
            "rounding": "half_even",
        },
        "initial_state": {
            "characters": {"hero": {"trust": _fixed("10.000", decimal_places)}},
            "resources": {"steps": _fixed("0", 0)},
            "flags": {"started": False},
        },
        "events": events,
    }


def all_samples(seed: int = 20261121) -> dict[str, dict[str, Any]]:
    return {
        "branch_flip.json": branch_flip_scenario(),
        "deterministic_small.json": deterministic_profile(
            "deterministic_small", seed + 1, 8, 2, 4
        ),
        "deterministic_medium.json": deterministic_profile(
            "deterministic_medium", seed + 2, 18, 3, 6
        ),
        "deterministic_deep.json": deterministic_profile(
            "deterministic_deep", seed + 3, 36, 2, 5
        ),
    }


def generate_samples(out_dir: str | Path, seed: int = 20261121) -> dict[str, Any]:
    output = Path(out_dir)
    output.mkdir(parents=True, exist_ok=True)
    (output.parent / "reports").mkdir(parents=True, exist_ok=True)
    manifest: dict[str, Any] = {
        "format": "causal-samples/1",
        "seed": seed,
        "profiles": {},
    }
    for name, document in all_samples(seed).items():
        path = output / name
        atomic_write_json(path, document)
        manifest["profiles"][name] = {
            "path": str(path),
            "scenario_id": document["scenario_id"],
            "event_count": len(document["events"]),
            "scenario_hash": hash_object(document),
            "max_branch_width": max(
                (len(event.get("branches", [])) for event in document["events"]),
                default=0,
            ),
            "decimal_places": document["precision"]["decimal_places"],
        }
    operations = {
        "branch_flip": ["b_edge"],
        "deterministic_small": ["b000_primary"],
    }
    atomic_write_json(output / "operations.json", operations)
    manifest["operations"] = str(output / "operations.json")
    manifest["manifest_hash"] = hash_object(manifest)
    atomic_write_json(output / "manifest.json", manifest)
    return manifest
