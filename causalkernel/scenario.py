"""Scenario syntax, graph construction and whole-graph validation."""

from __future__ import annotations

import math
from dataclasses import dataclass
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .canonical import hash_object, parse_json_bytes
from .errors import E_SCENARIO_INVALID, KernelError
from .precision import Precision, float64


MAX_EXPR_DEPTH = 32
MAX_EXPR_NODES = 512
MAX_GRAPH_DEPTH = 64

CONDITION_OPS = {">", ">=", "<", "<=", "==", "!="}


def _invalid(message: str, location: dict[str, Any] | None = None) -> KernelError:
    return KernelError(E_SCENARIO_INVALID, message, location or {})


def _decimal_text(value: str, location: dict[str, Any]) -> Decimal:
    try:
        number = Decimal(value)
    except (InvalidOperation, ValueError) as exc:
        raise _invalid("coefficient is not a decimal number", location) from exc
    if not number.is_finite():
        raise _invalid("coefficient must be finite", location)
    return number


@dataclass(frozen=True)
class Coefficient:
    kind: str
    text: str
    scale: int | None = None

    @classmethod
    def from_dict(cls, value: Any, location: dict[str, Any]) -> "Coefficient":
        if not isinstance(value, dict):
            raise _invalid("coefficient must be a tagged object", location)
        kind = value.get("kind")
        if kind not in {"fixed", "float"}:
            raise _invalid("coefficient kind must be 'fixed' or 'float'", location)
        text = value.get("text")
        if not isinstance(text, str) or not text:
            raise _invalid("coefficient text must be a non-empty string", location)
        raw = _decimal_text(text, location)
        scale = value.get("scale")
        if kind == "fixed":
            if not isinstance(scale, int) or isinstance(scale, bool) or not 0 <= scale <= 18:
                raise _invalid("fixed coefficient requires scale in [0, 18]", location)
            exponent = raw.as_tuple().exponent
            fractional_digits = max(0, -exponent)
            if fractional_digits > scale:
                raise _invalid(
                    "fixed coefficient has more fractional digits than its scale",
                    location,
                )
        else:
            if scale is not None:
                raise _invalid("float coefficient must not declare a scale", location)
            float64(raw)
        return cls(kind=kind, text=text, scale=scale)

    @property
    def decimal(self) -> Decimal:
        return Decimal(self.text)

    def to_dict(self) -> dict[str, Any]:
        value: dict[str, Any] = {"kind": self.kind, "text": self.text}
        if self.scale is not None:
            value["scale"] = self.scale
        return value


@dataclass(frozen=True)
class Condition:
    id: str
    expr: dict[str, Any]
    op: str
    threshold: Coefficient


@dataclass(frozen=True)
class Branch:
    id: str
    to: str
    priority: int
    condition: str | None
    index: int


@dataclass(frozen=True)
class Effect:
    id: str
    op: str
    path: str
    coef: Coefficient | None = None
    value: Any = None


@dataclass(frozen=True)
class Event:
    id: str
    index: int
    conditions: tuple[Condition, ...]
    preconditions: tuple[str, ...]
    effects: tuple[Effect, ...]
    branches: tuple[Branch, ...]

    def condition_map(self) -> dict[str, Condition]:
        return {condition.id: condition for condition in self.conditions}


@dataclass(frozen=True)
class ScalarSpec:
    kind: str
    value: Coefficient | bool | str


@dataclass
class Scenario:
    document: dict[str, Any]
    scenario_id: str
    start_event: str
    events: dict[str, Event]
    initial_values: dict[str, ScalarSpec]
    precision: Precision
    scenario_hash: str
    max_graph_depth: int

    def event(self, event_id: str) -> Event:
        event = self.events.get(event_id)
        if event is None:
            raise _invalid("event does not exist", {"event_id": event_id})
        return event


def _flatten_initial(
    value: Any,
    prefix: str,
    output: dict[str, ScalarSpec],
    location: dict[str, Any],
) -> None:
    if isinstance(value, dict):
        if "kind" in value:
            coef = Coefficient.from_dict(value, location)
            output[prefix] = ScalarSpec("number", coef)
            return
        if not value:
            raise _invalid("empty state object", location)
        for key, child in value.items():
            if not isinstance(key, str) or not key:
                raise _invalid("state keys must be non-empty strings", location)
            child_path = f"{prefix}.{key}" if prefix else key
            _flatten_initial(child, child_path, output, {**location, "path": child_path})
        return
    if isinstance(value, bool):
        output[prefix] = ScalarSpec("bool", value)
        return
    if isinstance(value, str):
        output[prefix] = ScalarSpec("string", value)
        return
    if isinstance(value, int) and not isinstance(value, bool):
        output[prefix] = ScalarSpec(
            "number",
            Coefficient("fixed", str(value), 0),
        )
        return
    if isinstance(value, float):
        if not math.isfinite(value):
            raise _invalid("initial numeric value must be finite", location)
        text = repr(value)
        output[prefix] = ScalarSpec("number", Coefficient("float", text))
        return
    raise _invalid("unsupported initial state value", location)


def _validate_expr(
    expr: Any,
    numeric_paths: set[str],
    location: dict[str, Any],
    depth: int = 0,
    counter: list[int] | None = None,
) -> None:
    if counter is None:
        counter = [0]
    counter[0] += 1
    if counter[0] > MAX_EXPR_NODES:
        raise _invalid("expression exceeds node limit", location)
    if depth > MAX_EXPR_DEPTH:
        raise _invalid("expression exceeds depth limit", location)
    if not isinstance(expr, dict):
        raise _invalid("expression node must be an object", location)
    op = expr.get("op")
    if op == "const":
        Coefficient.from_dict(expr.get("coef"), {**location, "field": "coef"})
        return
    if op == "var":
        path = expr.get("path")
        if not isinstance(path, str) or path not in numeric_paths:
            raise _invalid(
                "variable path is missing or not numeric",
                {**location, "path": path},
            )
        return
    if op in {"add", "sub", "mul"}:
        for side in ("left", "right"):
            if side not in expr:
                raise _invalid(f"binary expression missing {side}", location)
            _validate_expr(
                expr[side],
                numeric_paths,
                {**location, "side": side},
                depth + 1,
                counter,
            )
        return
    if op == "neg":
        if "arg" not in expr:
            raise _invalid("neg expression missing arg", location)
        _validate_expr(expr["arg"], numeric_paths, {**location, "side": "arg"}, depth + 1, counter)
        return
    if op == "pow":
        if "base" not in expr:
            raise _invalid("pow expression missing base", location)
        exponent = expr.get("exponent")
        if not isinstance(exponent, int) or isinstance(exponent, bool) or not 0 <= exponent <= 8:
            raise _invalid("pow exponent must be an integer in [0, 8]", location)
        _validate_expr(expr["base"], numeric_paths, {**location, "side": "base"}, depth + 1, counter)
        return
    raise _invalid("unsupported expression operator", {**location, "op": op})


def _parse_condition(value: Any, location: dict[str, Any], numeric_paths: set[str]) -> Condition:
    if not isinstance(value, dict):
        raise _invalid("condition must be an object", location)
    condition_id = value.get("id")
    if not isinstance(condition_id, str) or not condition_id:
        raise _invalid("condition id must be a non-empty string", location)
    op = value.get("op")
    if op not in CONDITION_OPS:
        raise _invalid("unsupported condition comparison", {**location, "op": op})
    if "expr" not in value:
        raise _invalid("condition missing expression", location)
    expr = value["expr"]
    _validate_expr(expr, numeric_paths, {**location, "field": "expr"})
    threshold = Coefficient.from_dict(
        value.get("threshold"),
        {**location, "field": "threshold"},
    )
    return Condition(condition_id, expr, op, threshold)


def _parse_effect(
    value: Any,
    location: dict[str, Any],
    initial_kinds: dict[str, str],
    fallback_id: str,
) -> Effect:
    if not isinstance(value, dict):
        raise _invalid("effect must be an object", location)
    effect_id = value.get("id", fallback_id)
    if not isinstance(effect_id, str) or not effect_id:
        raise _invalid("effect id must be a non-empty string", location)
    op = value.get("op")
    if op not in {"add", "sub", "mul", "set", "toggle"}:
        raise _invalid("unsupported effect operator", {**location, "op": op})
    path = value.get("path")
    if not isinstance(path, str) or path not in initial_kinds:
        raise _invalid("effect path is missing from initial state", {**location, "path": path})
    kind = initial_kinds[path]
    effect_location = {**location, "path": path}
    if op in {"add", "sub", "mul"}:
        if kind != "number":
            raise _invalid("arithmetic effect requires a numeric path", effect_location)
        coef = Coefficient.from_dict(value.get("coef"), {**effect_location, "field": "coef"})
        return Effect(effect_id, op, path, coef=coef)
    if op == "toggle":
        if kind != "bool":
            raise _invalid("toggle effect requires a boolean path", effect_location)
        return Effect(effect_id, op, path)
    if "coef" in value:
        if kind != "number":
            raise _invalid("numeric set requires a numeric path", effect_location)
        coef = Coefficient.from_dict(value.get("coef"), {**effect_location, "field": "coef"})
        return Effect(effect_id, op, path, coef=coef)
    if "value" not in value:
        raise _invalid("set effect requires value or coef", effect_location)
    raw_value = value["value"]
    if kind == "bool":
        if not isinstance(raw_value, bool):
            raise _invalid("boolean state requires a boolean value", effect_location)
    elif kind == "string":
        if not isinstance(raw_value, str):
            raise _invalid("string state requires a string value", effect_location)
    else:
        raise _invalid("numeric state requires a tagged coefficient", effect_location)
    return Effect(effect_id, op, path, value=raw_value)


def _parse_event(
    value: Any,
    index: int,
    numeric_paths: set[str],
    initial_kinds: dict[str, str],
) -> Event:
    if not isinstance(value, dict):
        raise _invalid("event must be an object", {"event_index": index})
    event_id = value.get("id")
    if not isinstance(event_id, str) or not event_id:
        raise _invalid("event id must be a non-empty string", {"event_index": index})
    location = {"event_id": event_id}
    raw_conditions = value.get("conditions", [])
    raw_preconditions = value.get("preconditions", [])
    raw_effects = value.get("effects", [])
    raw_branches = value.get("branches", [])
    if not isinstance(raw_conditions, list):
        raise _invalid("event conditions must be a list", location)
    if not isinstance(raw_effects, list):
        raise _invalid("event effects must be a list", location)
    if not isinstance(raw_branches, list):
        raise _invalid("event branches must be a list", location)
    if not isinstance(raw_preconditions, list):
        raise _invalid("event preconditions must be a list", location)

    conditions: list[Condition] = []
    condition_ids: set[str] = set()
    for condition_index, raw_condition in enumerate(raw_conditions):
        condition = _parse_condition(
            raw_condition,
            {**location, "condition_index": condition_index},
            numeric_paths,
        )
        if condition.id in condition_ids:
            raise _invalid(
                "duplicate condition id",
                {**location, "condition_id": condition.id},
            )
        condition_ids.add(condition.id)
        conditions.append(condition)

    preconditions: list[str] = []
    for precondition_index, condition_id in enumerate(raw_preconditions):
        if not isinstance(condition_id, str) or condition_id not in condition_ids:
            raise _invalid(
                "event precondition references an unknown condition",
                {
                    **location,
                    "precondition_index": precondition_index,
                    "condition": condition_id,
                },
            )
        if condition_id in preconditions:
            raise _invalid(
                "duplicate event precondition",
                {**location, "condition": condition_id},
            )
        preconditions.append(condition_id)

    effects: list[Effect] = []
    effect_ids: set[str] = set()
    for effect_index, raw_effect in enumerate(raw_effects):
        effect = _parse_effect(
            raw_effect,
            {**location, "effect_index": effect_index},
            initial_kinds,
            f"effect_{effect_index}",
        )
        if effect.id in effect_ids:
            raise _invalid("duplicate effect id", {**location, "effect_id": effect.id})
        effect_ids.add(effect.id)
        effects.append(effect)

    branches: list[Branch] = []
    branch_ids: set[str] = set()
    for branch_index, raw_branch in enumerate(raw_branches):
        branch_location = {**location, "branch_index": branch_index}
        if not isinstance(raw_branch, dict):
            raise _invalid("branch must be an object", branch_location)
        branch_id = raw_branch.get("id", f"branch_{branch_index}")
        target = raw_branch.get("to")
        priority = raw_branch.get("priority", 0)
        condition_id = raw_branch.get("condition")
        if not isinstance(branch_id, str) or not branch_id:
            raise _invalid("branch id must be a non-empty string", branch_location)
        if branch_id in branch_ids:
            raise _invalid("duplicate branch id", {**branch_location, "branch_id": branch_id})
        if not isinstance(target, str) or not target:
            raise _invalid("branch target must be a non-empty string", branch_location)
        if not isinstance(priority, int) or isinstance(priority, bool):
            raise _invalid("branch priority must be an integer", branch_location)
        if condition_id is not None:
            if not isinstance(condition_id, str) or condition_id not in condition_ids:
                raise _invalid(
                    "branch references an unknown condition",
                    {**branch_location, "condition": condition_id},
                )
        branch_ids.add(branch_id)
        branches.append(
            Branch(branch_id, target, priority, condition_id, branch_index)
        )
    return Event(
        event_id,
        index,
        tuple(conditions),
        tuple(preconditions),
        tuple(effects),
        tuple(branches),
    )


def _graph_checks(events: dict[str, Event], start_event: str) -> int:
    adjacency = {
        event_id: [branch.to for branch in event.branches]
        for event_id, event in events.items()
    }
    for event_id, targets in adjacency.items():
        for target in targets:
            if target not in events:
                raise _invalid(
                    "dangling branch target",
                    {"event_id": event_id, "target": target},
                )

    state: dict[str, int] = {}
    stack: list[tuple[str, Iterable[str]]] = []
    path: list[str] = []
    for root in events:
        if state.get(root, 0) != 0:
            continue
        state[root] = 1
        path.append(root)
        stack.append((root, iter(adjacency[root])))
        while stack:
            node, iterator = stack[-1]
            try:
                child = next(iterator)
            except StopIteration:
                state[node] = 2
                stack.pop()
                path.pop()
                continue
            if state.get(child, 0) == 1:
                cycle_start = path.index(child)
                cycle = path[cycle_start:] + [child]
                raise _invalid(
                    "event graph contains a cycle",
                    {"cycle": cycle, "event_id": child},
                )
            if state.get(child, 0) == 0:
                state[child] = 1
                path.append(child)
                stack.append((child, iter(adjacency[child])))

    maximum_depth = 0

    def visit(node: str, depth: int) -> None:
        nonlocal maximum_depth
        maximum_depth = max(maximum_depth, depth)
        if depth > MAX_GRAPH_DEPTH:
            raise _invalid(
                "branch chain exceeds maximum graph depth",
                {"event_id": node, "depth": depth, "limit": MAX_GRAPH_DEPTH},
            )
        for child in adjacency[node]:
            visit(child, depth + 1)

    visit(start_event, 1)
    return maximum_depth


def parse_scenario(document: dict[str, Any]) -> Scenario:
    if not isinstance(document, dict):
        raise _invalid("scenario root must be an object")
    if document.get("format") not in {None, "causal-story/1"}:
        raise _invalid("unsupported scenario format", {"format": document.get("format")})
    scenario_id = document.get("scenario_id")
    if not isinstance(scenario_id, str) or not scenario_id:
        raise _invalid("scenario_id must be a non-empty string")
    start_event = document.get("start_event")
    if not isinstance(start_event, str) or not start_event:
        raise _invalid("start_event must be a non-empty string")
    raw_initial = document.get("initial_state")
    if not isinstance(raw_initial, dict) or not raw_initial:
        raise _invalid("initial_state must be a non-empty object")
    initial_values: dict[str, ScalarSpec] = {}
    _flatten_initial(raw_initial, "", initial_values, {"field": "initial_state"})
    if "" in initial_values:
        raise _invalid("initial_state must be a nested object", {"field": "initial_state"})
    initial_kinds = {path: spec.kind for path, spec in initial_values.items()}
    numeric_paths = {path for path, kind in initial_kinds.items() if kind == "number"}

    raw_events = document.get("events")
    if not isinstance(raw_events, list) or not raw_events:
        raise _invalid("events must be a non-empty list")
    events: dict[str, Event] = {}
    for event_index, raw_event in enumerate(raw_events):
        event = _parse_event(raw_event, event_index, numeric_paths, initial_kinds)
        if event.id in events:
            raise _invalid("duplicate event id", {"event_id": event.id})
        events[event.id] = event
    if start_event not in events:
        raise _invalid("start_event does not exist", {"event_id": start_event})
    maximum_depth = _graph_checks(events, start_event)

    raw_precision = document.get("precision", {"mode": "fixed", "decimal_places": 6})
    precision = Precision.from_dict(raw_precision)
    return Scenario(
        document=document,
        scenario_id=scenario_id,
        start_event=start_event,
        events=events,
        initial_values=initial_values,
        precision=precision,
        scenario_hash=hash_object(document),
        max_graph_depth=maximum_depth,
    )


def load_scenario(
    path: str | Path,
    *,
    injection: str | None = None,
) -> Scenario:
    """Load, optionally inject a fault, then validate the entire scenario.

    Injection is intentionally placed only at this entry point. The evaluator
    and advancement implementation never branch on injection metadata.
    """

    from .injection import load_document_with_injection

    document = load_document_with_injection(path, injection)
    return parse_scenario(document)
