"""Deterministic causal advancement, attribution, tracing and snapshots."""

from __future__ import annotations

import json
import os
import re
from dataclasses import dataclass, field
from decimal import Decimal
from pathlib import Path
from typing import Any

import numpy as np

from .canonical import (
    Number,
    atomic_write_json,
    canonical_bytes,
    decode_number,
    encode_number,
    hash_object,
    parse_json_bytes,
    verify_seal,
)
from .errors import (
    E_ATTRIBUTION_FAILED,
    E_EVENT_NOT_TRIGGERED,
    E_RECOVERY_INVALID,
    E_SCENARIO_INVALID,
    E_TRACE_INVALID,
    KernelError,
)
from .precision import (
    FLOAT64_UNIT_ROUNDOFF,
    Precision,
    float64,
    float64_local_error,
)
from .scenario import (
    Branch,
    Coefficient,
    Condition,
    Event,
    Scenario,
    load_scenario,
)


TRACE_FORMAT = "causal-trace/1"
SNAPSHOT_FORMAT = "causal-snapshot/1"
SNAPSHOT_VERSION = 1
GENESIS_HASH = "GENESIS"
SNAPSHOT_PATTERN = re.compile(r"^snapshot-(\d{6})\.json$")


@dataclass
class ScalarCell:
    kind: str
    value: Any
    lower: Any = None
    upper: Any = None

    def to_full_dict(self) -> dict[str, Any]:
        if self.kind == "number":
            return {
                "kind": "number",
                "value": encode_number(self.value),
                "lower": encode_number(self.lower),
                "upper": encode_number(self.upper),
            }
        return {"kind": self.kind, "value": self.value}

    def to_point_dict(self) -> dict[str, Any]:
        if self.kind == "number":
            return {"kind": "number", "value": encode_number(self.value)}
        return {"kind": self.kind, "value": self.value}

    @classmethod
    def from_full_dict(cls, value: dict[str, Any]) -> "ScalarCell":
        kind = value["kind"]
        if kind == "number":
            return cls(
                kind,
                decode_number(value["value"], kind),
                decode_number(value["lower"], kind),
                decode_number(value["upper"], kind),
            )
        return cls(kind, value["value"])


@dataclass
class StateModel:
    cells: dict[str, ScalarCell] = field(default_factory=dict)

    @classmethod
    def from_scenario(cls, scenario: Scenario, precision: Precision) -> "StateModel":
        cells: dict[str, ScalarCell] = {}
        for path, spec in scenario.initial_values.items():
            if spec.kind == "number":
                coefficient = spec.value
                assert isinstance(coefficient, Coefficient)
                raw = coefficient.decimal
                if precision.mode == "fixed":
                    quantized = precision.quantize_decimal(raw)
                    lower = min(quantized.value, raw)
                    upper = max(quantized.value, raw)
                    cells[path] = ScalarCell("number", quantized.value, lower, upper)
                else:
                    value = float64(raw)
                    source_error = np.float64(
                        abs(Decimal.from_float(float(value)) - raw)
                    )
                    cells[path] = ScalarCell(
                        "number",
                        value,
                        np.float64(value - source_error),
                        np.float64(value + source_error),
                    )
            else:
                cells[path] = ScalarCell(spec.kind, spec.value)
        return cls(cells)

    def numeric(self, path: str, location: dict[str, Any]) -> ScalarCell:
        cell = self.cells.get(path)
        if cell is None or cell.kind != "number":
            raise KernelError(
                E_SCENARIO_INVALID,
                "numeric state path does not exist",
                {**location, "path": path},
            )
        return cell

    def point_state(self) -> dict[str, Any]:
        return {
            path: self.cells[path].to_point_dict()
            for path in sorted(self.cells)
        }

    def full_state(self) -> dict[str, Any]:
        return {
            path: self.cells[path].to_full_dict()
            for path in sorted(self.cells)
        }

    def point_hash(self) -> str:
        return hash_object(self.point_state())

    def full_hash(self) -> str:
        return hash_object(self.full_state())

    @classmethod
    def from_full_state(cls, value: dict[str, Any]) -> "StateModel":
        return cls(
            {
                path: ScalarCell.from_full_dict(cell)
                for path, cell in sorted(value.items())
            }
        )


@dataclass
class EvalNode:
    node_id: str
    op: str
    point: Number
    lower: Number
    upper: Number
    local_error: Number
    local_kind: str
    rounding_direction: str
    children: list[str] = field(default_factory=list)
    detail: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "node_id": self.node_id,
            "op": self.op,
            "children": list(self.children),
            "point": encode_number(self.point),
            "lower": encode_number(self.lower),
            "upper": encode_number(self.upper),
            "local_error": encode_number(self.local_error),
            "local_kind": self.local_kind,
            "rounding_direction": self.rounding_direction,
            "detail": self.detail,
        }


def _max_number(values: list[Number]) -> Number:
    return max(values)  # type: ignore[return-value]


def _min_number(values: list[Number]) -> Number:
    return min(values)  # type: ignore[return-value]


def _abs_number(value: Number) -> Number:
    return abs(value)


def _as_float(value: Number) -> float:
    return float(value)


class Evaluator:
    def __init__(self, precision: Precision, state: StateModel, prefix: str):
        self.precision = precision
        self.state = state
        self.prefix = prefix
        self.nodes: list[EvalNode] = []

    def _quantize(self, raw: Decimal) -> tuple[Number, str, Number]:
        if self.precision.mode == "fixed":
            quantized = self.precision.quantize_decimal(raw)
            return quantized.value, quantized.direction, quantized.error
        value = float64(raw)
        converted = Decimal.from_float(float(value))
        error = np.float64(abs(converted - raw))
        if converted > raw:
            direction = "up"
        elif converted < raw:
            direction = "down"
        else:
            direction = "none"
        return value, direction, error

    def _append(self, node: EvalNode) -> EvalNode:
        self.nodes.append(node)
        return node

    def constant(self, coefficient: Coefficient, node_id: str) -> EvalNode:
        raw = coefficient.decimal
        point, direction, error = self._quantize(raw)
        if self.precision.mode == "fixed":
            lower = min(raw, point)
            upper = max(raw, point)
        else:
            lower = np.float64(point - error)
            upper = np.float64(point + error)
        return self._append(
            EvalNode(
                node_id=node_id,
                op="const",
                point=point,
                lower=lower,
                upper=upper,
                local_error=error,
                local_kind="coefficient_quantization",
                rounding_direction=direction,
                detail={"coefficient": coefficient.to_dict()},
            )
        )

    def variable(self, path: str, node_id: str) -> EvalNode:
        cell = self.state.numeric(path, {"node_id": node_id})
        lower = cell.lower
        upper = cell.upper
        point = cell.value
        error = max(point - lower, upper - point)
        return self._append(
            EvalNode(
                node_id=node_id,
                op="var",
                point=point,
                lower=lower,
                upper=upper,
                local_error=error,
                local_kind="state_accumulation",
                rounding_direction="none",
                detail={"path": path},
            )
        )

    def _finish_operation(
        self,
        node_id: str,
        op: str,
        point: Number,
        exact_lower: Number,
        exact_upper: Number,
        operand_scale: Number,
        children: list[EvalNode],
        detail: dict[str, Any] | None = None,
    ) -> EvalNode:
        if self.precision.mode == "fixed":
            quantized = self.precision.quantize_decimal(point)  # type: ignore[arg-type]
            output = quantized.value
            local_error = quantized.error
            direction = quantized.direction
        else:
            output = float64(point)
            local_error = float64_local_error(output, float(_abs_number(operand_scale)))
            direction = "nearest_even"
        lower = min(exact_lower, output) - local_error
        upper = max(exact_upper, output) + local_error
        return self._append(
            EvalNode(
                node_id=node_id,
                op=op,
                point=output,
                lower=lower,
                upper=upper,
                local_error=local_error,
                local_kind="intermediate_quantization",
                rounding_direction=direction,
                children=[child.node_id for child in children],
                detail=detail or {},
            )
        )

    def binary(self, op: str, left: EvalNode, right: EvalNode, node_id: str) -> EvalNode:
        if op == "add":
            point = left.point + right.point
            exact_lower = left.lower + right.lower
            exact_upper = left.upper + right.upper
            scale = _abs_number(left.point) + _abs_number(right.point)
        elif op == "sub":
            point = left.point - right.point
            exact_lower = left.lower - right.upper
            exact_upper = left.upper - right.lower
            scale = _abs_number(left.point) + _abs_number(right.point)
        elif op == "mul":
            point = left.point * right.point
            products = [
                left.lower * right.lower,
                left.lower * right.upper,
                left.upper * right.lower,
                left.upper * right.upper,
            ]
            exact_lower = _min_number(products)
            exact_upper = _max_number(products)
            scale = max(_abs_number(value) for value in products)
        else:
            raise RuntimeError(f"unknown binary op {op}")
        return self._finish_operation(
            node_id,
            op,
            point,
            exact_lower,
            exact_upper,
            scale,
            [left, right],
        )

    def negate(self, child: EvalNode, node_id: str) -> EvalNode:
        exact_lower = -child.upper
        exact_upper = -child.lower
        point = -child.point
        return self._finish_operation(
            node_id,
            "neg",
            point,
            exact_lower,
            exact_upper,
            _abs_number(point),
            [child],
        )

    def evaluate_expr(self, expr: dict[str, Any], node_id: str) -> EvalNode:
        op = expr["op"]
        if op == "const":
            return self.constant(
                Coefficient.from_dict(expr["coef"], {"node_id": node_id}),
                node_id,
            )
        if op == "var":
            return self.variable(expr["path"], node_id)
        if op in {"add", "sub", "mul"}:
            left = self.evaluate_expr(expr["left"], f"{node_id}.left")
            right = self.evaluate_expr(expr["right"], f"{node_id}.right")
            return self.binary(op, left, right, node_id)
        if op == "neg":
            child = self.evaluate_expr(expr["arg"], f"{node_id}.arg")
            return self.negate(child, node_id)
        if op == "pow":
            base = self.evaluate_expr(expr["base"], f"{node_id}.base")
            exponent = expr["exponent"]
            if exponent == 0:
                return self.constant(Coefficient("fixed", "1", 0), node_id)
            current = base
            for step in range(1, exponent):
                current = self.binary(
                    "mul",
                    current,
                    base,
                    f"{node_id}.pow_step_{step}",
                )
                current.detail = {"pow": True, "exponent": exponent, "step": step}
            return current
        raise RuntimeError(f"unknown expression op {op}")

    def threshold(self, coefficient: Coefficient, node_id: str) -> dict[str, Any]:
        raw = coefficient.decimal
        point, direction, error = self._quantize(raw)
        if self.precision.mode == "fixed":
            lower = min(raw, point)
            upper = max(raw, point)
        else:
            lower = np.float64(point - error)
            upper = np.float64(point + error)
        return {
            "coefficient": coefficient.to_dict(),
            "value": encode_number(point),
            "lower": encode_number(lower),
            "upper": encode_number(upper),
            "rounding_direction": direction,
            "quantization_error": encode_number(error),
        }


def _compare(op: str, left: Number, right: Number) -> bool:
    if op == ">":
        return bool(left > right)
    if op == ">=":
        return bool(left >= right)
    if op == "<":
        return bool(left < right)
    if op == "<=":
        return bool(left <= right)
    if op == "==":
        return bool(left == right)
    if op == "!=":
        return bool(left != right)
    raise RuntimeError(f"unknown comparison {op}")


def _condition_truth(
    op: str,
    point: Number,
    lower: Number,
    upper: Number,
    threshold: Number,
) -> tuple[bool, bool, bool, bool]:
    point_truth = _compare(op, point, threshold)
    if op in {"==", "!="}:
        contains = bool(lower <= threshold and threshold <= upper)
        if op == "==":
            lower_truth = False
            upper_truth = True
            certain = (lower == upper == threshold) or not contains
            if lower == upper == threshold:
                lower_truth = upper_truth = True
            elif not contains:
                lower_truth = upper_truth = False
        else:
            lower_truth = False
            upper_truth = True
            certain = not contains
            if not contains:
                lower_truth = upper_truth = True
        return point_truth, lower_truth, upper_truth, certain
    lower_truth = _compare(op, lower, threshold)
    upper_truth = _compare(op, upper, threshold)
    return point_truth, lower_truth, upper_truth, bool(lower_truth == upper_truth)


def _attribute_uncertainty(
    nodes: list[EvalNode],
    root: EvalNode,
    threshold: Number,
    event_id: str,
    condition_id: str,
) -> dict[str, Any]:
    node_map = {node.node_id: node for node in nodes}
    sensitivity: dict[str, float] = {root.node_id: 1.0}
    for node in reversed(nodes):
        parent_sensitivity = sensitivity.get(node.node_id, 0.0)
        if parent_sensitivity == 0.0:
            continue
        if node.op in {"add", "sub"}:
            for child_id in node.children:
                sensitivity[child_id] = sensitivity.get(child_id, 0.0) + parent_sensitivity
        elif node.op in {"mul", "pow_step"} or (node.op == "mul" and node.detail.get("pow")):
            left = node_map[node.children[0]]
            right = node_map[node.children[1]]
            sensitivity[left.node_id] = (
                sensitivity.get(left.node_id, 0.0)
                + parent_sensitivity * abs(_as_float(right.point))
            )
            sensitivity[right.node_id] = (
                sensitivity.get(right.node_id, 0.0)
                + parent_sensitivity * abs(_as_float(left.point))
            )
        elif node.op == "neg":
            child_id = node.children[0]
            sensitivity[child_id] = sensitivity.get(child_id, 0.0) + parent_sensitivity

    factors: list[dict[str, Any]] = []
    for index, node in enumerate(nodes):
        sensitivity_value = sensitivity.get(node.node_id, 0.0)
        if node.local_error == 0 or sensitivity_value == 0.0:
            continue
        raw_contribution = _as_float(node.local_error) * sensitivity_value
        factors.append(
            {
                "chain_index": index,
                "node_id": node.node_id,
                "op": node.op,
                "factor_kind": node.local_kind,
                "local_error": encode_number(node.local_error),
                "sensitivity": sensitivity_value,
                "first_order_contribution": raw_contribution,
            }
        )

    margin = abs(_as_float(root.point) - _as_float(threshold))
    half_width = max(
        abs(_as_float(root.point) - _as_float(root.lower)),
        abs(_as_float(root.upper) - _as_float(root.point)),
    )
    raw_total = sum(float(factor["first_order_contribution"]) for factor in factors)
    if raw_total <= 0.0:
        raise KernelError(
            E_ATTRIBUTION_FAILED,
            "condition is uncertain but no causal error factor is available",
            {
                "event_id": event_id,
                "condition_id": condition_id,
                "lower": encode_number(root.lower),
                "upper": encode_number(root.upper),
                "threshold": encode_number(threshold),
            },
        )
    scale = max(1.0, half_width / raw_total)
    cumulative = 0.0
    attributed: dict[str, Any] | None = None
    for factor in factors:
        contribution = float(factor["first_order_contribution"]) * scale
        previous = cumulative
        cumulative += contribution
        factor["scaled_contribution"] = contribution
        factor["cumulative_error"] = cumulative
        if attributed is None and cumulative >= margin:
            attributed = {
                **factor,
                "previous_cumulative_error": previous,
                "margin": margin,
                "error_bound": half_width,
                "scale": scale,
                "reason": "threshold crossed by cumulative propagated error",
            }
    if attributed is None:
        raise KernelError(
            E_ATTRIBUTION_FAILED,
            "could not locate the earliest factor crossing the condition threshold",
            {
                "event_id": event_id,
                "condition_id": condition_id,
                "margin": margin,
                "half_width": half_width,
                "first_order_total": raw_total,
            },
        )
    return {
        "status": "attributed",
        "event_id": event_id,
        "condition_id": condition_id,
        "error_bound": half_width,
        "first_order_error_bound": raw_total,
        "factors": factors,
        "earliest_factor": attributed,
    }


def evaluate_condition(
    scenario: Scenario,
    state: StateModel,
    precision: Precision,
    event: Event,
    condition: Condition,
    purpose: str,
    branch: Branch | None = None,
) -> dict[str, Any]:
    prefix = f"{event.id}.{condition.id}.{purpose}"
    evaluator = Evaluator(precision, state, prefix)
    root = evaluator.evaluate_expr(condition.expr, f"{prefix}.expr")
    threshold = evaluator.threshold(condition.threshold, f"{prefix}.threshold")
    threshold_value = decode_number(threshold["value"], precision.mode)
    point_truth, lower_truth, upper_truth, certain = _condition_truth(
        condition.op,
        root.point,
        root.lower,
        root.upper,
        threshold_value,
    )
    error_bound = max(root.point - root.lower, root.upper - root.point)
    attribution = None
    if not certain:
        attribution = _attribute_uncertainty(
            evaluator.nodes,
            root,
            threshold_value,
            event.id,
            condition.id,
        )
    return {
        "event_id": event.id,
        "condition_id": condition.id,
        "branch_id": branch.id if branch is not None else None,
        "purpose": purpose,
        "expression": condition.expr,
        "operator": condition.op,
        "threshold": threshold,
        "point_value": encode_number(root.point),
        "lower_bound": encode_number(root.lower),
        "upper_bound": encode_number(root.upper),
        "cumulative_error_bound": encode_number(error_bound),
        "truth": point_truth,
        "truth_lower_bound": lower_truth,
        "truth_upper_bound": upper_truth,
        "certain": certain,
        "uncertain_branch": not certain,
        "attribution": attribution,
        "evaluation_chain": [node.to_dict() for node in evaluator.nodes],
    }


class Engine:
    def __init__(
        self,
        scenario: Scenario,
        precision: Precision,
        *,
        operations: list[Any] | None = None,
        max_steps: int = 64,
        snapshot_dir: str | Path | None = None,
        crash_after: int | None = None,
    ):
        self.scenario = scenario
        self.precision = precision
        self.operations = list(operations or [])
        self.max_steps = max_steps
        self.snapshot_dir = Path(snapshot_dir) if snapshot_dir is not None else None
        self.crash_after = crash_after
        self.state = StateModel.from_scenario(scenario, precision)
        self.steps: list[dict[str, Any]] = []
        self.current_event = scenario.start_event
        self.terminated_reason: str | None = None
        self._transition_cursor = 0

    @classmethod
    def recover(
        cls,
        scenario: Scenario,
        precision: Precision,
        snapshot_dir: str | Path,
        *,
        operations: list[Any] | None = None,
        max_steps: int = 64,
    ) -> "Engine":
        directory = Path(snapshot_dir)
        candidates: list[tuple[int, Path]] = []
        if directory.exists():
            for path in directory.iterdir():
                match = SNAPSHOT_PATTERN.match(path.name)
                if match:
                    candidates.append((int(match.group(1)), path))
        if not candidates:
            raise KernelError(
                E_RECOVERY_INVALID,
                "no snapshots are available",
                {"snapshot_dir": str(directory)},
            )
        candidates.sort(reverse=True)
        snapshot_path = candidates[0][1]
        try:
            snapshot = parse_json_bytes(snapshot_path.read_bytes())
        except Exception as exc:
            raise KernelError(
                E_RECOVERY_INVALID,
                "latest snapshot is unreadable or truncated",
                {"path": str(snapshot_path)},
            ) from exc
        if not isinstance(snapshot, dict):
            raise KernelError(
                E_RECOVERY_INVALID,
                "snapshot root must be an object",
                {"path": str(snapshot_path)},
            )
        verify_seal(snapshot, "snapshot_hash", E_RECOVERY_INVALID, str(snapshot_path))
        if snapshot.get("format") != SNAPSHOT_FORMAT:
            raise KernelError(
                E_RECOVERY_INVALID,
                "snapshot format is invalid",
                {"path": str(snapshot_path), "format": snapshot.get("format")},
            )
        if snapshot.get("version") != SNAPSHOT_VERSION:
            raise KernelError(
                E_RECOVERY_INVALID,
                "snapshot version does not match",
                {
                    "path": str(snapshot_path),
                    "expected": SNAPSHOT_VERSION,
                    "actual": snapshot.get("version"),
                },
            )
        if snapshot.get("scenario_hash") != scenario.scenario_hash:
            raise KernelError(
                E_RECOVERY_INVALID,
                "snapshot belongs to a different scenario",
                {"path": str(snapshot_path)},
            )
        if snapshot.get("precision") != precision.to_dict():
            raise KernelError(
                E_RECOVERY_INVALID,
                "snapshot precision does not match the requested mode",
                {"path": str(snapshot_path)},
            )
        if snapshot.get("operations") != list(operations or []):
            raise KernelError(
                E_RECOVERY_INVALID,
                "snapshot operations do not match the requested sequence",
                {"path": str(snapshot_path)},
            )
        if snapshot.get("max_steps") != max_steps:
            raise KernelError(
                E_RECOVERY_INVALID,
                "snapshot max_steps does not match",
                {"path": str(snapshot_path)},
            )
        steps = snapshot.get("steps")
        if not isinstance(steps, list) or len(steps) != snapshot.get("step_count"):
            raise KernelError(
                E_RECOVERY_INVALID,
                "snapshot step count is inconsistent",
                {"path": str(snapshot_path)},
            )
        previous = GENESIS_HASH
        for index, step in enumerate(steps):
            if not isinstance(step, dict):
                raise KernelError(
                    E_RECOVERY_INVALID,
                    "snapshot contains a malformed step",
                    {"path": str(snapshot_path), "step_index": index},
                )
            if step.get("prev_hash") != previous:
                raise KernelError(
                    E_RECOVERY_INVALID,
                    "snapshot causal chain is broken",
                    {"path": str(snapshot_path), "step_index": index},
                )
            body = dict(step)
            expected = body.pop("record_hash", None)
            actual = hash_object(body)
            if expected != actual:
                raise KernelError(
                    E_RECOVERY_INVALID,
                    "snapshot step hash mismatch",
                    {
                        "path": str(snapshot_path),
                        "step_index": index,
                        "expected": expected,
                        "actual": actual,
                    },
                )
            previous = expected
        if snapshot.get("chain_hash") != previous:
            raise KernelError(
                E_RECOVERY_INVALID,
                "snapshot chain hash mismatch",
                {"path": str(snapshot_path)},
            )
        try:
            state = StateModel.from_full_state(snapshot["state"])
        except Exception as exc:
            raise KernelError(
                E_RECOVERY_INVALID,
                "snapshot state cannot be decoded",
                {"path": str(snapshot_path)},
            ) from exc
        if state.full_hash() != snapshot.get("state_hash"):
            raise KernelError(
                E_RECOVERY_INVALID,
                "snapshot state hash mismatch",
                {"path": str(snapshot_path)},
            )
        engine = cls(
            scenario,
            precision,
            operations=operations,
            max_steps=max_steps,
            snapshot_dir=snapshot_dir,
        )
        engine.state = state
        engine.steps = list(steps)
        engine.current_event = snapshot["current_event"]
        engine._transition_cursor = sum(
            1 for step in engine.steps if step.get("kind") == "transition"
        )
        return engine

    def _seal_step(self, payload: dict[str, Any]) -> dict[str, Any]:
        previous = self.steps[-1]["record_hash"] if self.steps else GENESIS_HASH
        payload["prev_hash"] = previous
        payload["record_hash"] = hash_object(payload)
        self.steps.append(payload)
        return payload

    def _snapshot(self) -> None:
        if self.snapshot_dir is None:
            return
        body = {
            "format": SNAPSHOT_FORMAT,
            "version": SNAPSHOT_VERSION,
            "scenario_id": self.scenario.scenario_id,
            "scenario_hash": self.scenario.scenario_hash,
            "precision": self.precision.to_dict(),
            "operations": self.operations,
            "max_steps": self.max_steps,
            "step_count": len(self.steps),
            "current_event": self.current_event,
            "state": self.state.full_state(),
            "state_hash": self.state.full_hash(),
            "steps": self.steps,
            "chain_hash": self.steps[-1]["record_hash"] if self.steps else GENESIS_HASH,
        }
        body["snapshot_hash"] = hash_object(body)
        path = self.snapshot_dir / f"snapshot-{len(self.steps) - 1:06d}.json"
        atomic_write_json(path, body)
        if self.crash_after is not None and len(self.steps) >= self.crash_after:
            os._exit(17)

    def _event_conditions(
        self,
        event: Event,
        purpose: str,
    ) -> list[dict[str, Any]]:
        condition_map = event.condition_map()
        return [
            evaluate_condition(
                self.scenario,
                self.state,
                self.precision,
                event,
                condition_map[condition_id],
                purpose,
            )
            for condition_id in event.preconditions
        ]

    def _branch_condition(self, event: Event, branch: Branch) -> dict[str, Any]:
        if branch.condition is None:
            synthetic = Condition(
                id="<always>",
                expr={"op": "const", "coef": {"kind": "fixed", "text": "1", "scale": 0}},
                op=">=",
                threshold=Coefficient("fixed", "0", 0),
            )
            return evaluate_condition(
                self.scenario,
                self.state,
                self.precision,
                event,
                synthetic,
                "branch",
                branch,
            )
        condition = event.condition_map()[branch.condition]
        return evaluate_condition(
            self.scenario,
            self.state,
            self.precision,
            event,
            condition,
            "branch",
            branch,
        )

    def _apply_effect(self, effect: Any, step_index: int, effect_index: int) -> dict[str, Any]:
        cell = self.state.cells[effect.path]
        before = cell.to_point_dict()
        prefix = f"step{step_index}.effect{effect_index}"
        evaluator = Evaluator(self.precision, self.state, prefix)
        chain: list[EvalNode] = []
        rounding: dict[str, Any] = {}
        if effect.op in {"add", "sub", "mul"}:
            left = evaluator.variable(effect.path, f"{prefix}.before")
            right = evaluator.constant(effect.coef, f"{prefix}.coefficient")
            result = evaluator.binary(effect.op, left, right, f"{prefix}.result")
            cell.value = result.point
            cell.lower = result.lower
            cell.upper = result.upper
            chain = evaluator.nodes
            rounding = {
                "result_direction": result.rounding_direction,
                "local_error": encode_number(result.local_error),
            }
        elif effect.op == "set":
            if effect.coef is not None:
                result = evaluator.constant(effect.coef, f"{prefix}.coefficient")
                cell.value = result.point
                cell.lower = result.lower
                cell.upper = result.upper
                chain = evaluator.nodes
                rounding = {
                    "result_direction": result.rounding_direction,
                    "local_error": encode_number(result.local_error),
                }
            else:
                cell.value = effect.value
        elif effect.op == "toggle":
            cell.value = not bool(cell.value)
        else:
            raise KernelError(
                E_SCENARIO_INVALID,
                "unsupported effect operator",
                {"effect_id": effect.id, "op": effect.op},
            )
        return {
            "effect_id": effect.id,
            "op": effect.op,
            "path": effect.path,
            "before": before,
            "after": cell.to_point_dict(),
            "coefficient": effect.coef.to_dict() if effect.coef is not None else None,
            "rounding": rounding,
            "evaluation_chain": [node.to_dict() for node in chain],
        }

    def _apply_event_effects(self, event: Event, step_index: int) -> list[dict[str, Any]]:
        return [
            self._apply_effect(effect, step_index, effect_index)
            for effect_index, effect in enumerate(event.effects)
        ]

    def _entry_step(self) -> None:
        event = self.scenario.event(self.scenario.start_event)
        step_index = 0
        before = self.state.point_state()
        before_hash = self.state.point_hash()
        conditions = self._event_conditions(event, "entry_precondition")
        if any(not condition["truth"] for condition in conditions):
            raise KernelError(
                E_EVENT_NOT_TRIGGERED,
                "start event preconditions are false",
                {"event_id": event.id},
            )
        effects = self._apply_event_effects(event, step_index)
        payload = {
            "index": step_index,
            "kind": "entry",
            "source_event": None,
            "selected_event": event.id,
            "selected_branch": None,
            "precision": self.precision.to_dict(),
            "evaluated_conditions": conditions,
            "selection": {
                "eligible_branches": [],
                "priority_order": [],
                "selected_uncertain": any(
                    condition["uncertain_branch"] for condition in conditions
                ),
            },
            "state_before": before,
            "state_before_hash": before_hash,
            "effects": effects,
            "state_after": self.state.point_state(),
            "state_after_hash": self.state.point_hash(),
        }
        self._seal_step(payload)
        self.current_event = event.id
        self._snapshot()

    def _select_branch(
        self,
        branches: list[Branch],
        records: dict[str, dict[str, Any]],
        eligible: list[Branch],
        operations: Any | None,
    ) -> tuple[Branch | None, dict[str, Any] | None]:
        if not eligible:
            return None, None
        selected = eligible[0]
        operation = operations
        matched = True
        requested_id: str | None = None
        if operation is not None:
            if isinstance(operation, str):
                requested_id = operation
            elif isinstance(operation, dict):
                requested_id = operation.get("branch_id")
            else:
                raise KernelError(
                    E_SCENARIO_INVALID,
                    "operation entry must be a branch id or object",
                    {"operation_index": self._transition_cursor},
                )
            if not isinstance(requested_id, str):
                raise KernelError(
                    E_SCENARIO_INVALID,
                    "operation is missing branch_id",
                    {"operation_index": self._transition_cursor},
                )
            known = {branch.id for branch in branches}
            if requested_id not in known:
                raise KernelError(
                    E_SCENARIO_INVALID,
                    "operation references an unknown branch",
                    {
                        "operation_index": self._transition_cursor,
                        "branch_id": requested_id,
                    },
                )
            eligible_ids = {branch.id for branch in eligible}
            if requested_id in eligible_ids:
                selected = next(branch for branch in eligible if branch.id == requested_id)
            else:
                matched = False
        return selected, {
            "operation_index": self._transition_cursor,
            "requested_branch": requested_id,
            "matched": matched,
            "selected_branch": selected.id,
        }

    def _transition_step(self) -> bool:
        event = self.scenario.event(self.current_event)
        if not event.branches:
            self.terminated_reason = "terminal_event"
            return False
        before = self.state.point_state()
        before_hash = self.state.point_hash()

        branch_records = {
            branch.id: self._branch_condition(event, branch)
            for branch in event.branches
        }
        target_preconditions: dict[str, list[dict[str, Any]]] = {}
        for branch in event.branches:
            if branch.to not in target_preconditions:
                target = self.scenario.event(branch.to)
                target_preconditions[branch.to] = self._event_conditions(
                    target, "target_precondition"
                )

        eligible: list[Branch] = []
        branch_summaries: list[dict[str, Any]] = []
        for branch in event.branches:
            branch_record = branch_records[branch.id]
            conditions_true = all(
                record["truth"] for record in target_preconditions[branch.to]
            )
            is_eligible = branch_record["truth"] and conditions_true
            if is_eligible:
                eligible.append(branch)
            branch_summaries.append(
                {
                    "branch_id": branch.id,
                    "target": branch.to,
                    "priority": branch.priority,
                    "condition_truth": branch_record["truth"],
                    "condition_uncertain": branch_record["uncertain_branch"],
                    "target_preconditions_true": conditions_true,
                    "eligible": is_eligible,
                    "declaration_index": branch.index,
                }
            )
        eligible.sort(key=lambda branch: (-branch.priority, branch.index))
        operation = (
            self.operations[self._transition_cursor]
            if self._transition_cursor < len(self.operations)
            else None
        )
        selected, operation_record = self._select_branch(
            list(event.branches), branch_records, eligible, operation
        )
        if selected is None:
            self.terminated_reason = "no_eligible_branch"
            return False

        step_index = len(self.steps)
        effects = self._apply_event_effects(self.scenario.event(selected.to), step_index)
        evaluated_conditions = list(branch_records.values())
        for records in target_preconditions.values():
            evaluated_conditions.extend(records)
        selected_record = branch_records[selected.id]
        payload = {
            "index": step_index,
            "kind": "transition",
            "source_event": event.id,
            "selected_event": selected.to,
            "selected_branch": selected.id,
            "operation": operation_record,
            "precision": self.precision.to_dict(),
            "evaluated_conditions": evaluated_conditions,
            "selection": {
                "eligible_branches": [branch.id for branch in eligible],
                "priority_order": [branch.id for branch in eligible],
                "branches": branch_summaries,
                "selected_uncertain": selected_record["uncertain_branch"],
            },
            "state_before": before,
            "state_before_hash": before_hash,
            "effects": effects,
            "state_after": self.state.point_state(),
            "state_after_hash": self.state.point_hash(),
        }
        self._seal_step(payload)
        self.current_event = selected.to
        self._transition_cursor += 1
        self._snapshot()
        return True

    def run(self) -> "RunResult":
        if not self.steps:
            self._entry_step()
        while len(self.steps) < self.max_steps:
            if not self._transition_step():
                break
        if self.terminated_reason is None:
            self.terminated_reason = "max_steps"
        return RunResult(self)

    def build_trace(self) -> dict[str, Any]:
        body = {
            "format": TRACE_FORMAT,
            "scenario_id": self.scenario.scenario_id,
            "scenario_hash": self.scenario.scenario_hash,
            "precision": self.precision.to_dict(),
            "operations": self.operations,
            "max_steps": self.max_steps,
            "initial_state": (
                StateModel.from_scenario(self.scenario, self.precision).point_state()
            ),
            "initial_state_hash": (
                StateModel.from_scenario(self.scenario, self.precision).point_hash()
            ),
            "steps": self.steps,
            "final_state": self.state.point_state(),
            "final_state_hash": self.state.point_hash(),
            "causal_chain_hash": self.steps[-1]["record_hash"] if self.steps else GENESIS_HASH,
            "terminated_reason": self.terminated_reason,
        }
        body["trace_hash"] = hash_object(body)
        return body

    def write_trace(self, path: str | Path) -> dict[str, Any]:
        trace = self.build_trace()
        atomic_write_json(path, trace)
        return trace


@dataclass
class RunResult:
    engine: Engine

    @property
    def steps(self) -> list[dict[str, Any]]:
        return self.engine.steps

    @property
    def final_state(self) -> dict[str, Any]:
        return self.engine.state.point_state()

    @property
    def final_state_hash(self) -> str:
        return self.engine.state.point_hash()

    @property
    def causal_chain_hash(self) -> str:
        return self.steps[-1]["record_hash"] if self.steps else GENESIS_HASH

    @property
    def terminated_reason(self) -> str | None:
        return self.engine.terminated_reason

    @property
    def precision(self) -> Precision:
        return self.engine.precision

    def build_trace(self) -> dict[str, Any]:
        return self.engine.build_trace()

    def write_trace(self, path: str | Path) -> dict[str, Any]:
        return self.engine.write_trace(path)


def verify_trace_steps(steps: list[dict[str, Any]], code: str, path: str) -> str:
    previous = GENESIS_HASH
    for index, step in enumerate(steps):
        if not isinstance(step, dict):
            raise KernelError(code, "trace contains a malformed step", {"path": path, "step_index": index})
        if step.get("prev_hash") != previous:
            raise KernelError(
                code,
                "trace causal hash chain is broken",
                {"path": path, "step_index": index},
            )
        body = dict(step)
        expected = body.pop("record_hash", None)
        actual = hash_object(body)
        if expected != actual:
            raise KernelError(
                code,
                "trace step content hash mismatch",
                {
                    "path": path,
                    "step_index": index,
                    "expected": expected,
                    "actual": actual,
                },
            )
        previous = expected
    return previous


def replay_trace(
    scenario_path: str | Path,
    trace_path: str | Path,
) -> RunResult:
    trace_file = Path(trace_path)
    try:
        trace = parse_json_bytes(trace_file.read_bytes())
    except Exception as exc:
        raise KernelError(
            E_TRACE_INVALID,
            "trace is truncated or not valid JSON",
            {"path": str(trace_file)},
        ) from exc
    if not isinstance(trace, dict):
        raise KernelError(E_TRACE_INVALID, "trace root must be an object", {"path": str(trace_file)})
    verify_seal(trace, "trace_hash", E_TRACE_INVALID, str(trace_file))
    if trace.get("format") != TRACE_FORMAT:
        raise KernelError(
            E_TRACE_INVALID,
            "trace format is invalid",
            {"path": str(trace_file), "format": trace.get("format")},
        )
    steps = trace.get("steps")
    if not isinstance(steps, list):
        raise KernelError(E_TRACE_INVALID, "trace steps must be a list", {"path": str(trace_file)})
    chain_hash = verify_trace_steps(steps, E_TRACE_INVALID, str(trace_file))
    if chain_hash != trace.get("causal_chain_hash"):
        raise KernelError(
            E_TRACE_INVALID,
            "trace causal chain hash mismatch",
            {"path": str(trace_file)},
        )
    scenario = load_scenario(scenario_path)
    if scenario.scenario_hash != trace.get("scenario_hash"):
        raise KernelError(
            E_TRACE_INVALID,
            "trace scenario hash mismatch",
            {"path": str(trace_file)},
        )
    try:
        precision = Precision.from_dict(trace["precision"])
        operations = trace.get("operations", [])
        max_steps = int(trace["max_steps"])
    except Exception as exc:
        raise KernelError(
            E_TRACE_INVALID,
            "trace metadata cannot be decoded",
            {"path": str(trace_file)},
        ) from exc
    engine = Engine(
        scenario,
        precision,
        operations=operations,
        max_steps=max_steps,
    )
    result = engine.run()
    if engine.steps != steps:
        for index, (expected, actual) in enumerate(zip(steps, engine.steps)):
            if expected != actual:
                raise KernelError(
                    E_TRACE_INVALID,
                    "replayed causal record differs from the trace",
                    {
                        "path": str(trace_file),
                        "step_index": index,
                        "expected_hash": expected.get("record_hash"),
                        "actual_hash": actual.get("record_hash"),
                    },
                )
        raise KernelError(
            E_TRACE_INVALID,
            "replayed step count differs from the trace",
            {"path": str(trace_file), "expected": len(steps), "actual": len(engine.steps)},
        )
    if result.final_state_hash != trace.get("final_state_hash"):
        raise KernelError(
            E_TRACE_INVALID,
            "replayed terminal state hash differs",
            {
                "path": str(trace_file),
                "expected": trace.get("final_state_hash"),
                "actual": result.final_state_hash,
            },
        )
    if result.final_state != trace.get("final_state"):
        raise KernelError(
            E_TRACE_INVALID,
            "replayed terminal state differs",
            {"path": str(trace_file)},
        )
    return result
