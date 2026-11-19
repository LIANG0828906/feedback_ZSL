from __future__ import annotations

import math
from typing import Any, Iterable

from .sql_parser import AggregateSpec
from .util import ts_iso


def new_acc() -> dict[str, Any]:
    return {"n": 0, "a": {}}


def add_event(acc: dict[str, Any], aggregates: list[AggregateSpec], row: dict[str, Any]) -> None:
    acc["n"] = int(acc.get("n", 0)) + 1
    states = acc.setdefault("a", {})
    for agg in aggregates:
        if agg.func == "count_star":
            states[agg.alias] = int(states.get(agg.alias, 0)) + 1
            continue
        value = row.get(agg.column) if agg.column is not None else None
        if value is None:
            continue
        if agg.func == "count":
            if agg.distinct:
                values = states.setdefault(agg.alias, [])
                if value not in values:
                    values.append(value)
            else:
                states[agg.alias] = int(states.get(agg.alias, 0)) + 1
        elif agg.func == "sum":
            if agg.distinct:
                values = states.setdefault(agg.alias, [])
                if value not in values:
                    values.append(value)
            else:
                st = states.setdefault(agg.alias, {"sum": 0, "n": 0})
                st["sum"] = st.get("sum", 0) + value
                st["n"] = int(st.get("n", 0)) + 1
        elif agg.func == "avg":
            if agg.distinct:
                values = states.setdefault(agg.alias, [])
                if value not in values:
                    values.append(value)
            else:
                st = states.setdefault(agg.alias, {"sum": 0.0, "n": 0})
                st["sum"] = st.get("sum", 0.0) + value
                st["n"] = int(st.get("n", 0)) + 1
        elif agg.func == "min":
            if agg.alias not in states or value < states[agg.alias]:
                states[agg.alias] = value
        elif agg.func == "max":
            if agg.alias not in states or value > states[agg.alias]:
                states[agg.alias] = value


def merge_accs(
    parts: Iterable[dict[str, Any]],
    aggregates: list[AggregateSpec],
) -> dict[str, Any]:
    parts = list(parts)
    n = sum(int(p.get("n", 0)) for p in parts)
    merged_states: dict[str, Any] = {}
    for agg in aggregates:
        alias = agg.alias
        if agg.func == "count_star":
            merged_states[alias] = n
            continue
        vals = [p.get("a", {}).get(alias) for p in parts if alias in p.get("a", {})]
        if agg.func == "count":
            if agg.distinct:
                union: list[Any] = []
                seen = set()
                for v in vals:
                    for item in v or []:
                        key = _hashable(item)
                        if key not in seen:
                            seen.add(key)
                            union.append(item)
                merged_states[alias] = len(union)
            else:
                merged_states[alias] = sum(int(v or 0) for v in vals)
        elif agg.func == "sum":
            if agg.distinct:
                union = _union_values(vals)
                merged_states[alias] = _round(math.fsum(union)) if union else None
            else:
                sums = [v for v in vals if isinstance(v, dict) and int(v.get("n", 0)) > 0]
                merged_states[alias] = (
                    _round(math.fsum(float(v["sum"]) for v in sums)) if sums else None
                )
        elif agg.func == "avg":
            if agg.distinct:
                union = _union_values(vals)
                merged_states[alias] = (
                    _round(math.fsum(union) / len(union)) if union else None
                )
            else:
                nums = [v for v in vals if isinstance(v, dict) and int(v.get("n", 0)) > 0]
                total = math.fsum(float(v["sum"]) for v in nums)
                count = sum(int(v["n"]) for v in nums)
                merged_states[alias] = _round(total / count) if count else None
        elif agg.func == "min":
            merged_states[alias] = min(vals) if vals else None
        elif agg.func == "max":
            merged_states[alias] = max(vals) if vals else None
    return {"n": n, "a": merged_states}


def project_result(
    acc: dict[str, Any],
    aggregates: list[AggregateSpec],
    vehicle_id: str,
    window_start: int,
    window_end: int,
) -> dict[str, Any]:
    result: dict[str, Any] = {
        "vehicle_id": vehicle_id,
        "window_start": ts_iso(window_start),
        "window_end": ts_iso(window_end),
    }
    states = acc.get("a", {})
    for agg in aggregates:
        result[agg.alias] = states.get(agg.alias)
    return result


def _union_values(vals: list[Any]) -> list[float]:
    out: list[float] = []
    seen = set()
    for v in vals:
        for item in v or []:
            key = _hashable(item)
            if key not in seen:
                seen.add(key)
                try:
                    out.append(float(item))
                except (TypeError, ValueError):
                    continue
    return out


def _hashable(value: Any) -> Any:
    try:
        hash(value)
        return (type(value).__name__, value)
    except TypeError:
        return (type(value).__name__, str(value))


def _round(value: float) -> float:
    # Six decimals is well beyond the source precision and makes the batch and
    # stream merge paths byte-for-byte stable in the resulting JSON.
    return round(float(value), 6)

