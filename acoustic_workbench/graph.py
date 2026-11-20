from __future__ import annotations

from typing import Any

from .errors import E_FACTOR_INVALID, fail
from .model import Scene, normalize_factor_name


NODE_ORDER = (
    "direct_align",
    "early_reflections",
    "band_decay",
    "late_reverb",
    "multichannel_stitch",
    "field_coefficients",
)

NODE_TITLES = {
    "direct_align": "Direct alignment",
    "early_reflections": "Early reflections",
    "band_decay": "Band decay",
    "late_reverb": "Late reverb",
    "multichannel_stitch": "5.1 stitch",
    "field_coefficients": "Field coefficients",
}

EARLIEST_NODE = {
    "material": "early_reflections",
    "source_position": "direct_align",
    "listener_position": "direct_align",
    "band_config": "early_reflections",
    "window": "early_reflections",
    "alignment": "direct_align",
    "late_config": "late_reverb",
}


def suffix_from(node: str, reason: str) -> dict[str, list[str]]:
    start = NODE_ORDER.index(node)
    return {name: [reason] for name in NODE_ORDER[start:]}


def affected_listeners(
    scene: Scene,
    factor: str,
    change: dict[str, Any],
) -> list[str]:
    if factor == "listener_position":
        listener_id = str(
            change.get("listener_id", change.get("listener", change.get("id", "")))
        )
        scene.listener(listener_id)
        return [listener_id]
    if factor == "material":
        wall_id = str(change.get("wall_id", change.get("wall", "")))
        scene.wall(wall_id)
        return [point.id for point in scene.listeners]
    return [point.id for point in scene.listeners]


def invalidation_plan(
    scene: Scene,
    changes: list[dict[str, Any]],
) -> dict[str, Any]:
    per_point: dict[str, dict[str, Any]] = {
        point.id: {"invalidated_nodes": [], "reasons": {}, "earliest": None}
        for point in scene.listeners
    }
    factor_records: list[dict[str, Any]] = []
    for change in changes:
        if not isinstance(change, dict):
            fail(E_FACTOR_INVALID, "each influence-factor change must be an object")
        factor = normalize_factor_name(
            change.get("factor", change.get("type", change.get("kind", "")))
        )
        if factor not in EARLIEST_NODE:
            fail(E_FACTOR_INVALID, f"factor {factor} has no graph rule")
        targets = affected_listeners(scene, factor, change)
        earliest = EARLIEST_NODE[factor]
        reason = f"{factor} -> {earliest}"
        factor_records.append(
            {
                "factor": factor,
                "targets": targets,
                "earliest_node": earliest,
                "reason": reason,
            }
        )
        for listener_id in targets:
            point_plan = per_point[listener_id]
            start = NODE_ORDER.index(earliest)
            for node in NODE_ORDER[start:]:
                if node not in point_plan["invalidated_nodes"]:
                    point_plan["invalidated_nodes"].append(node)
                reasons = point_plan["reasons"].setdefault(node, [])
                if reason not in reasons:
                    reasons.append(reason)
            if point_plan["earliest"] is None:
                point_plan["earliest"] = earliest
            elif NODE_ORDER.index(earliest) < NODE_ORDER.index(
                point_plan["earliest"]
            ):
                point_plan["earliest"] = earliest
    for plan in per_point.values():
        plan["invalidated_nodes"] = [
            node for node in NODE_ORDER if node in plan["invalidated_nodes"]
        ]
    return {"points": per_point, "factors": factor_records}


def merge_invalidation_plans(*plans: dict[str, Any]) -> dict[str, Any]:
    merged = {"points": {}, "factors": []}
    for plan in plans:
        merged["factors"].extend(plan.get("factors", []))
        for point_id, point_plan in plan.get("points", {}).items():
            target = merged["points"].setdefault(
                point_id,
                {"invalidated_nodes": [], "reasons": {}, "earliest": None},
            )
            for node in point_plan.get("invalidated_nodes", []):
                if node not in target["invalidated_nodes"]:
                    target["invalidated_nodes"].append(node)
            for node, reasons in point_plan.get("reasons", {}).items():
                bucket = target["reasons"].setdefault(node, [])
                for reason in reasons:
                    if reason not in bucket:
                        bucket.append(reason)
            earliest = point_plan.get("earliest")
            if earliest is not None:
                if target["earliest"] is None:
                    target["earliest"] = earliest
                elif NODE_ORDER.index(earliest) < NODE_ORDER.index(
                    target["earliest"]
                ):
                    target["earliest"] = earliest
    for target in merged["points"].values():
        target["invalidated_nodes"] = [
            node for node in NODE_ORDER if node in target["invalidated_nodes"]
        ]
    return merged

