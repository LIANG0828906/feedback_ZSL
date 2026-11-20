"""Fault injection at the scenario loading boundary.

All injected faults are detected here, before graph validation or execution.
No evaluator or advancement code knows about injected-fault metadata.
"""

from __future__ import annotations

import json
from decimal import Decimal
from pathlib import Path
from typing import Any

from .canonical import parse_json_bytes
from .errors import (
    E_INJECT_CYCLE,
    E_INJECT_DEPTH_EXCEEDED,
    E_INJECT_NONFINITE_COEFFICIENT,
    E_INJECT_PRECISION_CONFLICT,
    E_INJECT_TRUNCATED,
    E_SCENARIO_INVALID,
    KernelError,
)
from .scenario import MAX_GRAPH_DEPTH


INJECTIONS = {
    "cycle",
    "precision-conflict",
    "deep-chain",
    "nonfinite",
    "truncated",
}


def _first_event(document: dict[str, Any]) -> dict[str, Any] | None:
    events = document.get("events")
    if isinstance(events, list) and events and isinstance(events[0], dict):
        return events[0]
    return None


def _first_condition(document: dict[str, Any]) -> tuple[dict[str, Any], dict[str, Any]] | None:
    events = document.get("events")
    if not isinstance(events, list):
        return None
    for event in events:
        if not isinstance(event, dict):
            continue
        conditions = event.get("conditions")
        if isinstance(conditions, list):
            for condition in conditions:
                if isinstance(condition, dict):
                    return event, condition
    return None


def _cycle_path(document: dict[str, Any]) -> list[str] | None:
    events = document.get("events")
    if not isinstance(events, list):
        return None
    adjacency: dict[str, list[str]] = {}
    for event in events:
        if not isinstance(event, dict) or not isinstance(event.get("id"), str):
            continue
        targets: list[str] = []
        branches = event.get("branches", [])
        if isinstance(branches, list):
            for branch in branches:
                if isinstance(branch, dict) and isinstance(branch.get("to"), str):
                    targets.append(branch["to"])
        adjacency[event["id"]] = targets
    visiting: set[str] = set()
    visited: set[str] = set()
    stack: list[str] = []

    def visit(node: str) -> list[str] | None:
        visiting.add(node)
        stack.append(node)
        for child in adjacency.get(node, []):
            if child in visiting:
                start = stack.index(child)
                return stack[start:] + [child]
            if child not in visited:
                found = visit(child)
                if found:
                    return found
        stack.pop()
        visiting.remove(node)
        visited.add(node)
        return None

    for node in adjacency:
        if node not in visited:
            found = visit(node)
            if found:
                return found
    return None


def _graph_depth(document: dict[str, Any], start_event: str) -> int:
    events = document.get("events")
    if not isinstance(events, list):
        return 0
    adjacency: dict[str, list[str]] = {}
    for event in events:
        if isinstance(event, dict) and isinstance(event.get("id"), str):
            branches = event.get("branches", [])
            adjacency[event["id"]] = [
                branch["to"]
                for branch in branches
                if isinstance(branch, dict) and isinstance(branch.get("to"), str)
            ]
    memo: dict[str, int] = {}

    def depth(node: str, active: set[str]) -> int:
        if node in memo:
            return memo[node]
        if node in active:
            return 10**9
        active.add(node)
        child_depth = 0
        for child in adjacency.get(node, []):
            child_depth = max(child_depth, depth(child, active))
        active.remove(node)
        memo[node] = 1 + child_depth
        return memo[node]

    return depth(start_event, set()) if start_event in adjacency else 0


def _apply_cycle(document: dict[str, Any]) -> None:
    event = _first_event(document)
    if event is None:
        raise KernelError(
            E_INJECT_CYCLE,
            "injection could not find an event",
            {"injection": "cycle", "json_path": "$.events[0]"},
        )
    branches = event.setdefault("branches", [])
    if not isinstance(branches, list):
        branches = []
        event["branches"] = branches
    target = document.get("start_event", event.get("id"))
    branches.append(
        {
            "id": "__injected_cycle__",
            "to": target,
            "priority": 999999,
            "condition": None,
        }
    )
    cycle = _cycle_path(document)
    if not cycle:
        raise KernelError(
            E_INJECT_CYCLE,
            "cycle injection failed to create a cycle",
            {"injection": "cycle"},
        )
    raise KernelError(
        E_INJECT_CYCLE,
        "injected cyclic event dependency",
        {
            "injection": "cycle",
            "json_path": f"$.events[{event.get('id')}].branches[-1].to",
            "event_id": event.get("id"),
            "cycle": cycle,
        },
    )


def _apply_precision_conflict(document: dict[str, Any]) -> None:
    found = _first_condition(document)
    if found is None:
        raise KernelError(
            E_INJECT_PRECISION_CONFLICT,
            "injection could not find a condition",
            {"injection": "precision-conflict"},
        )
    event, condition = found
    document["precision"] = {
        "mode": "fixed",
        "decimal_places": 2,
        "rounding": "half_even",
    }
    condition["threshold"] = {"kind": "fixed", "text": "0.123456789", "scale": 9}
    precision = document["precision"]
    threshold = condition["threshold"]
    threshold_text = threshold["text"]
    fractional = max(0, -Decimal(threshold_text).as_tuple().exponent)
    if fractional > precision["decimal_places"]:
        raise KernelError(
            E_INJECT_PRECISION_CONFLICT,
            "condition threshold needs more fractional digits than the fixed mode provides",
            {
                "injection": "precision-conflict",
                "json_path": f"$.events[{event.get('id')}].conditions[{condition.get('id')}].threshold",
                "event_id": event.get("id"),
                "condition_id": condition.get("id"),
                "threshold_scale": fractional,
                "decimal_places": precision["decimal_places"],
            },
        )


def _apply_deep_chain(document: dict[str, Any]) -> None:
    events = document.get("events")
    if not isinstance(events, list) or not events:
        raise KernelError(
            E_INJECT_DEPTH_EXCEEDED,
            "injection could not find an event list",
            {"injection": "deep-chain"},
        )
    anchor = events[0] if isinstance(events[0], dict) else {}
    anchor_id = anchor.get("id")
    if not isinstance(anchor_id, str):
        raise KernelError(
            E_INJECT_DEPTH_EXCEEDED,
            "injection could not find an anchor event",
            {"injection": "deep-chain", "json_path": "$.events[0]"},
        )
    previous = anchor_id
    for index in range(MAX_GRAPH_DEPTH + 12):
        event_id = f"__injected_deep_{index:03d}__"
        target = (
            f"__injected_deep_{index + 1:03d}__"
            if index < MAX_GRAPH_DEPTH + 11
            else None
        )
        branches = []
        if target:
            branches.append(
                {
                    "id": f"deep_branch_{index}",
                    "to": target,
                    "priority": 0,
                    "condition": None,
                }
            )
        events.append(
            {
                "id": event_id,
                "conditions": [],
                "effects": [],
                "branches": branches,
            }
        )
    branches = anchor.setdefault("branches", [])
    if not isinstance(branches, list):
        branches = []
        anchor["branches"] = branches
    branches.append(
        {
            "id": "__injected_deep_entry__",
            "to": "__injected_deep_000__",
            "priority": -999999,
            "condition": None,
        }
    )
    depth = _graph_depth(document, document.get("start_event", anchor_id))
    if depth > MAX_GRAPH_DEPTH:
        raise KernelError(
            E_INJECT_DEPTH_EXCEEDED,
            "injected branch chain exceeds the graph depth limit",
            {
                "injection": "deep-chain",
                "json_path": f"$.events[{anchor_id}].branches[-1].to",
                "event_id": anchor_id,
                "depth": depth,
                "limit": MAX_GRAPH_DEPTH,
            },
        )


def _apply_nonfinite(document: dict[str, Any]) -> None:
    events = document.get("events")
    location: dict[str, Any] = {"injection": "nonfinite"}
    if isinstance(events, list):
        for event in events:
            if not isinstance(event, dict):
                continue
            effects = event.get("effects")
            if isinstance(effects, list) and effects and isinstance(effects[0], dict):
                effects[0]["coef"] = {"kind": "fixed", "text": "NaN", "scale": 3}
                location.update(
                    {
                        "json_path": f"$.events[{event.get('id')}].effects[0].coef",
                        "event_id": event.get("id"),
                    }
                )
                break
    if "json_path" not in location:
        anchor = _first_event(document)
        if anchor is not None:
            anchor.setdefault("effects", []).append(
                {
                    "id": "__injected_nonfinite__",
                    "op": "add",
                    "path": "flags.__injected_nonfinite__",
                    "coef": {"kind": "fixed", "text": "Infinity", "scale": 3},
                }
            )
            location.update(
                {
                    "json_path": f"$.events[{anchor.get('id')}].effects[-1].coef",
                    "event_id": anchor.get("id"),
                }
            )
    raise KernelError(
        E_INJECT_NONFINITE_COEFFICIENT,
        "injected non-finite coefficient",
        location,
    )


def load_document_with_injection(path: str | Path, injection: str | None) -> dict[str, Any]:
    source = Path(path)
    raw = source.read_bytes()
    if injection is None:
        try:
            document = parse_json_bytes(raw)
        except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
            raise KernelError(
                E_SCENARIO_INVALID,
                "scenario is not valid JSON",
                {"path": str(source), "offset": getattr(exc, "pos", None)},
            ) from exc
        if not isinstance(document, dict):
            raise KernelError(
                E_SCENARIO_INVALID,
                "scenario root must be a JSON object",
                {"path": str(source)},
            )
        return document

    if injection not in INJECTIONS:
        raise KernelError(
            E_SCENARIO_INVALID,
            "unknown injection type",
            {"injection": injection, "supported": sorted(INJECTIONS)},
        )

    if injection == "truncated":
        cut = max(1, len(raw) // 2)
        truncated = raw[:cut]
        try:
            parse_json_bytes(truncated)
        except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
            raise KernelError(
                E_INJECT_TRUNCATED,
                "injected truncated scenario JSON",
                {
                    "injection": "truncated",
                    "json_path": "$",
                    "byte_offset": getattr(exc, "pos", cut),
                    "original_bytes": len(raw),
                    "truncated_bytes": cut,
                },
            ) from exc

    try:
        document = parse_json_bytes(raw)
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
        raise KernelError(
            E_SCENARIO_INVALID,
            "injected scenario source is not valid JSON",
            {"path": str(source), "offset": getattr(exc, "pos", None)},
        ) from exc
    if not isinstance(document, dict):
        raise KernelError(E_SCENARIO_INVALID, "scenario root must be an object", {})

    if injection == "cycle":
        _apply_cycle(document)
    if injection == "precision-conflict":
        _apply_precision_conflict(document)
    if injection == "deep-chain":
        _apply_deep_chain(document)
    if injection == "nonfinite":
        _apply_nonfinite(document)
    raise RuntimeError("unreachable injection dispatch")
