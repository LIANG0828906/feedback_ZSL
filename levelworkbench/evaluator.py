"""Constraint evaluation, error reports and conflict evidence."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Iterable

from .fixed import (
    build_area_model,
    build_door_model,
    build_keychain_model,
    model_result,
)
from .models import Constraint, Level, WorkbenchConfig
from .reachability import ReachabilityResult, analyze_key_chain


@dataclass
class EvaluationBundle:
    results: dict[str, dict[str, Any]]
    reachability: ReachabilityResult
    error_report: list[dict[str, Any]]
    failed_hard: list[str]
    failed_soft: list[str]
    uncertain: list[str]

    def to_dict(self) -> dict[str, Any]:
        return {
            "results": self.results,
            "error_report": self.error_report,
            "failed_hard": list(self.failed_hard),
            "failed_soft": list(self.failed_soft),
            "uncertain": list(self.uncertain),
            "hard_passed": not self.failed_hard,
            "minimal_evidence": minimal_conflict_evidence(
                [self.results[item] for item in self.failed_hard]
            ),
            "reachability": self.reachability.to_dict(),
        }


def _bool_result(
    constraint: Constraint,
    *,
    passed: bool,
    truth: bool,
    evidence: dict[str, Any],
    model_name: str,
    extra: dict[str, Any] | None = None,
) -> dict[str, Any]:
    result = {
        "constraint_id": constraint.id,
        "type": constraint.type,
        "priority": constraint.priority,
        "hard": constraint.hard,
        "passed": passed,
        "model": model_name,
        "fixed_truth": truth,
        "float_truth": truth,
        "truth_lower": truth,
        "truth_upper": truth,
        "uncertain": False,
        "attribution": None,
        "evidence": evidence,
    }
    if extra:
        result.update(extra)
    return result


def evaluate_constraints(level: Level, config: WorkbenchConfig) -> EvaluationBundle:
    keychain = config.primary_keychain()
    required_keys = keychain.keys if keychain is not None else ()
    reachability = analyze_key_chain(level, required_keys)
    room_ids = {room.id for room in level.rooms}
    results: dict[str, dict[str, Any]] = {}
    error_report: list[dict[str, Any]] = []
    failed_hard: list[str] = []
    failed_soft: list[str] = []
    uncertain_constraints: list[str] = []

    for constraint in sorted(config.constraints, key=lambda item: (-item.priority, item.id)):
        if constraint.type == "room_count":
            minimum = int(constraint.params["min"])
            maximum = int(constraint.params["max"])
            actual = len(level.rooms)
            passed = minimum <= actual <= maximum
            result = _bool_result(
                constraint,
                passed=passed,
                truth=passed,
                model_name="integer_range",
                evidence={
                    "observed": actual,
                    "min": minimum,
                    "max": maximum,
                    "facts": [f"room_count={actual}", f"allowed=[{minimum},{maximum}]"],
                },
            )
        elif constraint.type == "connectivity":
            scope = constraint.params.get("scope", "all_rooms")
            if scope == "start_to_end":
                passed = reachability.end_reachable
                missing: tuple[str, ...] = () if passed else ("end",)
                evidence = {
                    "scope": scope,
                    "start": reachability.start_room,
                    "end": reachability.end_room,
                    "missing": list(missing),
                    "path": list(reachability.path),
                    "facts": [
                        f"start={reachability.start_room}",
                        f"end={reachability.end_room}",
                        f"end_reachable={reachability.end_reachable}",
                    ],
                }
            else:
                missing = tuple(sorted(room_ids - set(reachability.reached_rooms)))
                passed = not missing
                evidence = {
                    "scope": scope,
                    "missing_rooms": list(missing),
                    "reached_rooms": list(reachability.reached_rooms),
                    "facts": [
                        f"reached_rooms={len(reachability.reached_rooms)}",
                        f"total_rooms={len(level.rooms)}",
                        f"missing_rooms={list(missing)}",
                    ],
                }
            result = _bool_result(
                constraint,
                passed=passed,
                truth=passed,
                model_name="door_reachability",
                evidence=evidence,
            )
        elif constraint.type == "key_chain":
            decimals, rounding = config.quantization_for(constraint)
            numeric = build_keychain_model(
                constraint.keys,
                constraint.coefficients,
                constraint.threshold,
                decimals,
                rounding,
            )
            numeric_result = model_result(
                numeric,
                "chain.compare",
                constraint.id,
                extra={
                    "quantization": {"decimals": decimals, "rounding": rounding},
                    "numeric_threshold": constraint.threshold,
                    "keys": list(constraint.keys),
                },
            )
            selected = numeric_result["fixed_truth"] if config.evaluation_mode == "fixed" else numeric_result["float_truth"]
            if config.uncertain_policy == "fail" and numeric_result["uncertain"]:
                selected = False
            passed = reachability.ok and selected
            numeric_result.update(
                {
                    "type": constraint.type,
                    "priority": constraint.priority,
                    "hard": constraint.hard,
                    "passed": passed,
                    "reachability_ok": reachability.ok,
                    "reachability_reason": reachability.reason,
                    "path": list(reachability.path),
                    "unlock_order": list(reachability.unlock_order),
                    "collected_keys": list(reachability.collected_keys),
                    "missing_keys": list(reachability.missing_keys),
                    "evidence": {
                        "required_keys": list(reachability.required_keys),
                        "collected_keys": list(reachability.collected_keys),
                        "missing_keys": list(reachability.missing_keys),
                        "path": list(reachability.path),
                        "unlock_order": list(reachability.unlock_order),
                        "reachability_ok": reachability.ok,
                        "reason": reachability.reason,
                        "facts": [
                            f"reachability_ok={reachability.ok}",
                            f"collected_keys={list(reachability.collected_keys)}",
                            f"missing_keys={list(reachability.missing_keys)}",
                        ],
                    },
                }
            )
            result = numeric_result
        elif constraint.type == "no_lock_unreachable_end":
            passed = not reachability.end_reachable_without_locks and not reachability.premature_end_reachable
            result = _bool_result(
                constraint,
                passed=passed,
                truth=passed,
                model_name="locked_endpoint",
                evidence={
                    "end_reachable_without_locks": reachability.end_reachable_without_locks,
                    "premature_end_reachable": reachability.premature_end_reachable,
                    "facts": [
                        "end_reachable_without_locks="
                        f"{reachability.end_reachable_without_locks}",
                        f"premature_end_reachable={reachability.premature_end_reachable}",
                    ],
                },
            )
        elif constraint.type == "area_target":
            decimals, rounding = config.quantization_for(constraint)
            model = build_area_model(
                (room.area for room in level.rooms),
                float(constraint.params["target"]),
                float(constraint.params["tolerance"]),
                float(constraint.params["coefficient"]),
                decimals,
                rounding,
            )
            actual_area = sum(room.area for room in level.rooms)
            numeric_result = model_result(
                model,
                "area.compare",
                constraint.id,
                extra={
                    "quantization": {"decimals": decimals, "rounding": rounding},
                    "observed": actual_area,
                    "target": constraint.params["target"],
                    "tolerance": constraint.params["tolerance"],
                },
            )
            selected = numeric_result["fixed_truth"] if config.evaluation_mode == "fixed" else numeric_result["float_truth"]
            if config.uncertain_policy == "fail" and numeric_result["uncertain"]:
                selected = False
            numeric_result.update(
                {
                    "type": constraint.type,
                    "priority": constraint.priority,
                    "hard": constraint.hard,
                    "passed": selected,
                    "evidence": {
                        "observed": actual_area,
                        "target": constraint.params["target"],
                        "tolerance": constraint.params["tolerance"],
                        "facts": [
                            f"area={actual_area}",
                            f"target={constraint.params['target']}",
                            f"tolerance={constraint.params['tolerance']}",
                        ],
                    },
                }
            )
            result = numeric_result
        elif constraint.type == "door_count_target":
            decimals, rounding = config.quantization_for(constraint)
            model = build_door_model(
                len(level.doors),
                float(constraint.params["target"]),
                float(constraint.params["tolerance"]),
                float(constraint.params["coefficient"]),
                decimals,
                rounding,
            )
            numeric_result = model_result(
                model,
                "doors.compare",
                constraint.id,
                extra={
                    "quantization": {"decimals": decimals, "rounding": rounding},
                    "observed": len(level.doors),
                    "target": constraint.params["target"],
                    "tolerance": constraint.params["tolerance"],
                },
            )
            selected = numeric_result["fixed_truth"] if config.evaluation_mode == "fixed" else numeric_result["float_truth"]
            if config.uncertain_policy == "fail" and numeric_result["uncertain"]:
                selected = False
            numeric_result.update(
                {
                    "type": constraint.type,
                    "priority": constraint.priority,
                    "hard": constraint.hard,
                    "passed": selected,
                    "evidence": {
                        "observed": len(level.doors),
                        "target": constraint.params["target"],
                        "tolerance": constraint.params["tolerance"],
                        "facts": [
                            f"doors={len(level.doors)}",
                            f"target={constraint.params['target']}",
                            f"tolerance={constraint.params['tolerance']}",
                        ],
                    },
                }
            )
            result = numeric_result
        else:
            continue

        results[constraint.id] = result
        if result.get("uncertain"):
            uncertain_constraints.append(constraint.id)
        if result.get("fixed_truth") != result.get("float_truth") or result.get("uncertain"):
            error_report.append(
                {
                    "constraint_id": constraint.id,
                    "type": constraint.type,
                    "quantization": result.get(
                        "quantization",
                        {
                            "decimals": config.quantization_for(constraint)[0],
                            "rounding": config.quantization_for(constraint)[1],
                        },
                    ),
                    "fixed_truth": result.get("fixed_truth"),
                    "float_truth": result.get("float_truth"),
                    "truth_lower": result.get("truth_lower"),
                    "truth_upper": result.get("truth_upper"),
                    "uncertain": result.get("uncertain"),
                    "bounds": result.get("comparison"),
                    "first_uncertain_factor": result.get("attribution"),
                }
            )
        if result["passed"]:
            continue
        if constraint.hard:
            failed_hard.append(constraint.id)
        else:
            failed_soft.append(constraint.id)

    return EvaluationBundle(
        results=results,
        reachability=reachability,
        error_report=error_report,
        failed_hard=failed_hard,
        failed_soft=failed_soft,
        uncertain=uncertain_constraints,
    )


def evaluate_types(
    level: Level,
    config: WorkbenchConfig,
    types: Iterable[str],
) -> EvaluationBundle:
    selected = set(types)
    filtered = WorkbenchConfig(
        **{
            **config.__dict__,
            "constraints": tuple(item for item in config.constraints if item.type in selected),
        }
    )
    return evaluate_constraints(level, filtered)


def minimal_conflict_evidence(failed_results: list[dict[str, Any]]) -> dict[str, Any] | None:
    if not failed_results:
        return None
    ordered = sorted(
        failed_results,
        key=lambda item: (-int(item.get("priority", 0)), str(item.get("constraint_id"))),
    )
    selected = ordered
    facts: list[dict[str, Any]] = []
    for result in selected:
        evidence = dict(result.get("evidence", {}))
        values = evidence.pop("facts", [])
        facts.append(
            {
                "constraint_id": result["constraint_id"],
                "type": result["type"],
                "observed": evidence,
                "facts": values,
            }
        )
    return {
        "constraints": [item["constraint_id"] for item in selected],
        "facts": facts,
        "sufficient": True,
        "explanation": "Each entry is the narrowest configured condition that observed a false hard predicate.",
    }
