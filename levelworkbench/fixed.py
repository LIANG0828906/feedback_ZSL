"""Quantization, interval propagation and dual fixed/float evaluation."""

from __future__ import annotations

from dataclasses import dataclass, field
from decimal import Decimal, ROUND_CEILING, ROUND_DOWN, ROUND_FLOOR, ROUND_HALF_EVEN, ROUND_HALF_UP, ROUND_UP
from typing import Any, Iterable


ROUNDING_MAP = {
    "half_up": ROUND_HALF_UP,
    "half_even": ROUND_HALF_EVEN,
    "floor": ROUND_FLOOR,
    "ceiling": ROUND_CEILING,
    "down": ROUND_DOWN,
    "up": ROUND_UP,
}


def as_decimal(value: Any) -> Decimal:
    if isinstance(value, Decimal):
        return value
    if isinstance(value, float):
        return Decimal(str(value))
    return Decimal(value)


def decimal_text(value: Decimal) -> str:
    text = format(value, "f")
    if "." in text:
        text = text.rstrip("0").rstrip(".")
    return text or "0"


@dataclass
class Quantized:
    exact: Decimal
    quantized: Decimal
    decimals: int
    rounding: str
    direction: str
    error_lower: Decimal
    error_upper: Decimal

    def to_dict(self) -> dict[str, Any]:
        return {
            "exact": decimal_text(self.exact),
            "quantized": decimal_text(self.quantized),
            "decimals": self.decimals,
            "rounding": self.rounding,
            "direction": self.direction,
            "error_lower": decimal_text(self.error_lower),
            "error_upper": decimal_text(self.error_upper),
        }


def quantize(value: Any, decimals: int, rounding: str = "half_up") -> Quantized:
    if rounding not in ROUNDING_MAP:
        raise ValueError(f"unsupported rounding mode: {rounding}")
    exact = as_decimal(value)
    quantum = Decimal(1).scaleb(-decimals)
    quantized = exact.quantize(quantum, rounding=ROUNDING_MAP[rounding])
    if quantized > exact:
        direction = "up"
    elif quantized < exact:
        direction = "down"
    else:
        direction = "none"
    error_lower = min(quantized - exact, Decimal(0))
    error_upper = max(quantized - exact, Decimal(0))
    return Quantized(
        exact=exact,
        quantized=quantized,
        decimals=decimals,
        rounding=rounding,
        direction=direction,
        error_lower=error_lower,
        error_upper=error_upper,
    )


def _interval_product(
    left_lower: Decimal,
    left_upper: Decimal,
    right_lower: Decimal,
    right_upper: Decimal,
) -> tuple[Decimal, Decimal]:
    candidates = (
        left_lower * right_lower,
        left_lower * right_upper,
        left_upper * right_lower,
        left_upper * right_upper,
    )
    return min(candidates), max(candidates)


@dataclass
class ErrorNode:
    node_id: str
    op: str
    exact: Decimal
    quantized: Decimal
    lower: Decimal
    upper: Decimal
    inputs: list[str] = field(default_factory=list)
    label: str = ""
    details: dict[str, Any] = field(default_factory=dict)

    @property
    def width(self) -> Decimal:
        return self.upper - self.lower

    @property
    def uncertain_factor(self) -> bool:
        return self.op == "leaf" and self.width != 0

    def to_dict(self) -> dict[str, Any]:
        payload = {
            "id": self.node_id,
            "op": self.op,
            "label": self.label,
            "exact": decimal_text(self.exact),
            "quantized": decimal_text(self.quantized),
            "lower": decimal_text(self.lower),
            "upper": decimal_text(self.upper),
            "width": decimal_text(self.width),
            "inputs": list(self.inputs),
        }
        if self.details:
            payload["details"] = self.details
        return payload


class ErrorModel:
    def __init__(self, name: str) -> None:
        self.name = name
        self.nodes: list[ErrorNode] = []
        self._by_id: dict[str, ErrorNode] = {}

    def add(self, node: ErrorNode) -> ErrorNode:
        if node.node_id in self._by_id:
            raise ValueError(f"duplicate error node: {node.node_id}")
        self.nodes.append(node)
        self._by_id[node.node_id] = node
        return node

    def get(self, node_id: str) -> ErrorNode:
        return self._by_id[node_id]

    def leaf(
        self,
        node_id: str,
        value: Any,
        *,
        decimals: int,
        rounding: str,
        label: str = "",
        source: str = "coefficient",
    ) -> ErrorNode:
        quantized = quantize(value, decimals, rounding)
        return self.add(
            ErrorNode(
                node_id=node_id,
                op="leaf",
                exact=quantized.exact,
                quantized=quantized.quantized,
                lower=min(quantized.exact, quantized.quantized),
                upper=max(quantized.exact, quantized.quantized),
                label=label,
                details={
                    "source": source,
                    "decimals": decimals,
                    "rounding": rounding,
                    "direction": quantized.direction,
                    "error_lower": decimal_text(quantized.error_lower),
                    "error_upper": decimal_text(quantized.error_upper),
                },
            )
        )

    def integer_leaf(self, node_id: str, value: int, label: str = "") -> ErrorNode:
        return self.add(
            ErrorNode(
                node_id=node_id,
                op="leaf",
                exact=Decimal(value),
                quantized=Decimal(value),
                lower=Decimal(value),
                upper=Decimal(value),
                label=label,
                details={"source": "integer", "decimals": 0, "rounding": "none", "direction": "none"},
            )
        )

    def sum(self, node_id: str, input_ids: Iterable[str], label: str = "") -> ErrorNode:
        inputs = [self.get(item) for item in input_ids]
        exact = sum((item.exact for item in inputs), Decimal(0))
        quantized = sum((item.quantized for item in inputs), Decimal(0))
        lower = sum((item.lower for item in inputs), Decimal(0))
        upper = sum((item.upper for item in inputs), Decimal(0))
        return self.add(
            ErrorNode(
                node_id=node_id,
                op="sum",
                exact=exact,
                quantized=quantized,
                lower=lower,
                upper=upper,
                inputs=[item.node_id for item in inputs],
                label=label,
            )
        )

    def product(self, node_id: str, left_id: str, right_id: str, label: str = "") -> ErrorNode:
        left = self.get(left_id)
        right = self.get(right_id)
        lower, upper = _interval_product(left.lower, left.upper, right.lower, right.upper)
        return self.add(
            ErrorNode(
                node_id=node_id,
                op="product",
                exact=left.exact * right.exact,
                quantized=left.quantized * right.quantized,
                lower=lower,
                upper=upper,
                inputs=[left.node_id, right.node_id],
                label=label,
            )
        )

    def subtract(self, node_id: str, left_id: str, right_id: str, label: str = "") -> ErrorNode:
        left = self.get(left_id)
        right = self.get(right_id)
        return self.add(
            ErrorNode(
                node_id=node_id,
                op="subtract",
                exact=left.exact - right.exact,
                quantized=left.quantized - right.quantized,
                lower=left.lower - right.upper,
                upper=left.upper - right.lower,
                inputs=[left.node_id, right.node_id],
                label=label,
            )
        )

    def absolute(self, node_id: str, input_id: str, label: str = "") -> ErrorNode:
        source = self.get(input_id)
        if source.lower <= 0 <= source.upper:
            lower = Decimal(0)
        else:
            lower = min(abs(source.lower), abs(source.upper))
        upper = max(abs(source.lower), abs(source.upper))
        return self.add(
            ErrorNode(
                node_id=node_id,
                op="abs",
                exact=abs(source.exact),
                quantized=abs(source.quantized),
                lower=lower,
                upper=upper,
                inputs=[source.node_id],
                label=label,
            )
        )

    def compare(
        self,
        node_id: str,
        left_id: str,
        right_id: str,
        operator: str,
        label: str = "",
    ) -> ErrorNode:
        if operator not in ("<=", ">=", "<", ">"):
            raise ValueError(f"unsupported comparison operator: {operator}")
        left = self.get(left_id)
        right = self.get(right_id)
        exact_true = _compare(left.exact, right.exact, operator)
        quantized_true = _compare(left.quantized, right.quantized, operator)
        definite_true, definite_false = comparison_bounds(left, right, operator)
        node = self.add(
            ErrorNode(
                node_id=node_id,
                op="compare",
                exact=Decimal(1 if exact_true else 0),
                quantized=Decimal(1 if quantized_true else 0),
                lower=Decimal(1 if definite_true else 0),
                upper=Decimal(0 if definite_false else 1),
                inputs=[left.node_id, right.node_id],
                label=label,
                details={
                    "operator": operator,
                    "exact_true": exact_true,
                    "fixed_true": quantized_true,
                    "definite_true": definite_true,
                    "definite_false": definite_false,
                    "uncertain": not definite_true and not definite_false,
                    "left_lower": decimal_text(left.lower),
                    "left_upper": decimal_text(left.upper),
                    "right_lower": decimal_text(right.lower),
                    "right_upper": decimal_text(right.upper),
                },
            )
        )
        return node

    def to_nodes(self) -> list[dict[str, Any]]:
        return [node.to_dict() for node in self.nodes]


def _compare(left: Decimal, right: Decimal, operator: str) -> bool:
    if operator == "<=":
        return left <= right
    if operator == ">=":
        return left >= right
    if operator == "<":
        return left < right
    return left > right


def comparison_bounds(left: ErrorNode, right: ErrorNode, operator: str) -> tuple[bool, bool]:
    if operator == "<=":
        return left.upper <= right.lower, left.lower > right.upper
    if operator == ">=":
        return left.lower >= right.upper, left.upper < right.lower
    if operator == "<":
        return left.upper < right.lower, left.lower >= right.upper
    return left.lower > right.upper, left.upper <= right.lower


def first_uncertain_factor(model: ErrorModel, comparison_id: str) -> dict[str, Any]:
    comparison = model.get(comparison_id)
    uncertain = bool(comparison.details.get("uncertain"))
    result: dict[str, Any] = {
        "uncertain": uncertain,
        "first_factor": None,
        "node_id": None,
        "quantization": None,
    }
    if not uncertain:
        return result
    for node in model.nodes:
        if node.uncertain_factor:
            result.update(
                {
                    "first_factor": node.label or node.node_id,
                    "node_id": node.node_id,
                    "source": node.details.get("source", "quantization"),
                    "quantization": {
                        "decimals": node.details.get("decimals"),
                        "rounding": node.details.get("rounding"),
                        "direction": node.details.get("direction"),
                        "exact": decimal_text(node.exact),
                        "quantized": decimal_text(node.quantized),
                    },
                }
            )
            return result
    for node in model.nodes:
        if node.width != 0:
            result.update(
                {
                    "first_factor": node.label or node.node_id,
                    "node_id": node.node_id,
                    "source": "interval_accumulation",
                }
            )
            return result
    return result


def build_area_model(
    room_areas: Iterable[int],
    target: float,
    tolerance: float,
    coefficient: float,
    decimals: int,
    rounding: str,
) -> ErrorModel:
    model = ErrorModel("area_target")
    area_ids = []
    for index, area in enumerate(room_areas):
        area_ids.append(f"area.room_{index}")
        model.integer_leaf(area_ids[-1], int(area), label=f"room_{index}.area")
    if area_ids:
        model.sum("area.sum", area_ids, label="sum(room areas)")
    else:
        model.integer_leaf("area.sum", 0, label="sum(room areas)")
    coefficient_node = model.leaf(
        "area.coefficient",
        coefficient,
        decimals=decimals,
        rounding=rounding,
        label="area coefficient",
    )
    model.product("area.score", "area.sum", coefficient_node.node_id, label="area * coefficient")
    target_node = model.leaf(
        "area.target",
        target,
        decimals=decimals,
        rounding=rounding,
        label="area target",
    )
    model.subtract("area.difference", "area.score", target_node.node_id, label="area score - target")
    model.absolute("area.distance", "area.difference", label="abs(area difference)")
    tolerance_node = model.leaf(
        "area.tolerance",
        tolerance,
        decimals=decimals,
        rounding=rounding,
        label="area tolerance",
    )
    model.compare("area.compare", "area.distance", tolerance_node.node_id, "<=", label="area within tolerance")
    return model


def build_door_model(
    door_count: int,
    target: float,
    tolerance: float,
    coefficient: float,
    decimals: int,
    rounding: str,
) -> ErrorModel:
    model = ErrorModel("door_count_target")
    door_ids = []
    for index in range(int(door_count)):
        door_ids.append(f"doors.door_{index}")
        model.integer_leaf(door_ids[-1], 1, label=f"door_{index}")
    if door_ids:
        model.sum("doors.sum", door_ids, label="count(doors)")
    else:
        model.integer_leaf("doors.sum", 0, label="count(doors)")
    coefficient_node = model.leaf(
        "doors.coefficient",
        coefficient,
        decimals=decimals,
        rounding=rounding,
        label="door-count coefficient",
    )
    model.product("doors.score", "doors.sum", coefficient_node.node_id, label="doors * coefficient")
    target_node = model.leaf(
        "doors.target",
        target,
        decimals=decimals,
        rounding=rounding,
        label="door target",
    )
    model.subtract("doors.difference", "doors.score", target_node.node_id, label="door score - target")
    model.absolute("doors.distance", "doors.difference", label="abs(door difference)")
    tolerance_node = model.leaf(
        "doors.tolerance",
        tolerance,
        decimals=decimals,
        rounding=rounding,
        label="door tolerance",
    )
    model.compare("doors.compare", "doors.distance", tolerance_node.node_id, "<=", label="doors within tolerance")
    return model


def build_keychain_model(
    keys: Iterable[str],
    coefficients: dict[str, float],
    threshold: float,
    decimals: int,
    rounding: str,
) -> ErrorModel:
    model = ErrorModel("key_chain")
    key_list = list(keys)
    if not key_list:
        one = model.integer_leaf("chain.one", 1, label="empty-chain identity")
        threshold_node = model.leaf(
            "chain.threshold",
            threshold,
            decimals=decimals,
            rounding=rounding,
            label="chain threshold",
        )
        model.compare("chain.compare", one.node_id, threshold_node.node_id, ">=", label="chain score")
        return model
    current = "chain.factor.k0"
    model.leaf(
        current,
        coefficients.get(key_list[0], 1.0),
        decimals=decimals,
        rounding=rounding,
        label=f"{key_list[0]} coefficient",
    )
    for index, key in enumerate(key_list[1:], start=1):
        leaf_id = f"chain.factor.k{index}"
        model.leaf(
            leaf_id,
            coefficients.get(key, 1.0),
            decimals=decimals,
            rounding=rounding,
            label=f"{key} coefficient",
        )
        node_id = f"chain.product.{index}"
        model.product(node_id, current, leaf_id, label=f"multiply through {key}")
        current = node_id
    threshold_node = model.leaf(
        "chain.threshold",
        threshold,
        decimals=decimals,
        rounding=rounding,
        label="chain threshold",
    )
    model.compare("chain.compare", current, threshold_node.node_id, ">=", label="chain score >= threshold")
    return model


def model_result(
    model: ErrorModel,
    comparison_id: str,
    constraint_id: str,
    *,
    extra: dict[str, Any] | None = None,
) -> dict[str, Any]:
    comparison = model.get(comparison_id)
    details = comparison.details
    fixed_truth = bool(details["fixed_true"])
    float_truth = bool(details["exact_true"])
    attribution = first_uncertain_factor(model, comparison_id)
    result = {
        "constraint_id": constraint_id,
        "model": model.name,
        "fixed_truth": fixed_truth,
        "float_truth": float_truth,
        "truth_lower": bool(details["definite_true"]),
        "truth_upper": not bool(details["definite_false"]),
        "uncertain": bool(details["uncertain"]),
        "comparison": {
            "operator": details["operator"],
            "left_lower": details["left_lower"],
            "left_upper": details["left_upper"],
            "right_lower": details["right_lower"],
            "right_upper": details["right_upper"],
        },
        "attribution": attribution,
        "nodes": model.to_nodes(),
    }
    if extra:
        result.update(extra)
    return result
