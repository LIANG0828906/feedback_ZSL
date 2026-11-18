"""Decision policy defaults, loading, and validation."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

from .errors import HandoverError


SEVERITIES: tuple[str, str, str, str] = ("P1", "P2", "P3", "P4")
SEVERITY_RANK = {grade: index + 1 for index, grade in enumerate(SEVERITIES)}


DEFAULT_POLICY: dict[str, Any] = {
    "schema_version": "1.0",
    "policy_version": "2026.11.1",
    "windows": {
        "short_minutes": 30,
        "long_minutes": 360,
    },
    "burn": {
        "short_trigger": 2.0,
        "long_trigger": 1.0,
        "burst_ratio": 2.0,
    },
    "severity": {
        "budget_remaining_pct": {"P1": 10.0, "P2": 25.0, "P3": 50.0},
        "short_burn": {"P1": 14.4, "P2": 6.0, "P3": 2.0},
        "long_burn": {"P1": 6.0, "P2": 3.0, "P3": 1.0},
        "affected_services": {"P1": 3, "P2": 2, "P3": 1},
        "change_risk": {
            "critical": "P1",
            "high": "P2",
            "medium": "P3",
            "low": "P4",
            "none": "P4",
        },
        "trigger_priority": [
            "short_burn_rate",
            "long_burn_rate",
            "error_budget_remaining_pct",
            "affected_services",
            "change_risk",
        ],
        "combination_rules": [
            {
                "id": "multi_service_high_change",
                "when": {
                    "affected_services_gte": 2,
                    "change_risk_at_least": "high",
                },
                "minimum_grade": "P2",
            },
            {
                "id": "low_budget_burst",
                "when": {
                    "budget_remaining_pct_lte": 25.0,
                    "burn_state": "burst",
                },
                "minimum_grade": "P2",
            },
            {
                "id": "wide_impact_critical_burn",
                "when": {
                    "affected_services_gte": 3,
                    "short_burn_gte": 6.0,
                },
                "minimum_grade": "P1",
            },
        ],
    },
    "silence": {
        "min_remaining_minutes": {"P1": 180, "P2": 120, "P3": 60, "P4": 30},
        "overbroad_service_count": 3,
        "change_risk_active_minutes": 240,
    },
    "replay": {
        "max_points": 20000,
    },
}


RISK_ORDER = {"none": 0, "low": 1, "medium": 2, "high": 3, "critical": 4}


def _deep_merge(base: dict[str, Any], override: dict[str, Any]) -> dict[str, Any]:
    result = copy.deepcopy(base)
    for key, value in override.items():
        if isinstance(value, dict) and isinstance(result.get(key), dict):
            result[key] = _deep_merge(result[key], value)
        else:
            result[key] = copy.deepcopy(value)
    return result


def load_policy(path: str | Path | None = None) -> dict[str, Any]:
    policy = copy.deepcopy(DEFAULT_POLICY)
    if path:
        try:
            raw = json.loads(Path(path).read_text(encoding="utf-8"))
        except FileNotFoundError as exc:
            raise HandoverError(
                "POLICY_NOT_FOUND",
                "The policy file was not found.",
                details={"file": Path(path).name},
            ) from exc
        except (OSError, json.JSONDecodeError) as exc:
            raise HandoverError(
                "INVALID_POLICY",
                "The policy file is not valid JSON.",
                details={"file": Path(path).name},
            ) from exc
        if not isinstance(raw, dict):
            raise HandoverError("INVALID_POLICY", "The policy must be a JSON object.")
        policy = _deep_merge(policy, raw)
    validate_policy(policy)
    return policy


def validate_policy(policy: dict[str, Any]) -> None:
    try:
        short = int(policy["windows"]["short_minutes"])
        long = int(policy["windows"]["long_minutes"])
        if short <= 0 or long <= 0 or short > long:
            raise ValueError
        if float(policy["burn"]["burst_ratio"]) < 1:
            raise ValueError
        if float(policy["burn"]["short_trigger"]) < 0:
            raise ValueError
        if float(policy["burn"]["long_trigger"]) < 0:
            raise ValueError
        for dimension in (
            "short_burn",
            "long_burn",
            "budget_remaining_pct",
            "affected_services",
        ):
            for grade in ("P1", "P2", "P3"):
                float(policy["severity"][dimension][grade])
        risk_mapping = policy["severity"]["change_risk"]
        for risk in ("critical", "high", "medium", "low", "none"):
            if risk_mapping[risk] not in SEVERITIES:
                raise ValueError
        if not isinstance(policy["severity"].get("trigger_priority"), list):
            raise ValueError
        if not isinstance(policy["severity"].get("combination_rules"), list):
            raise ValueError
        for rule in policy["severity"]["combination_rules"]:
            if not isinstance(rule, dict):
                raise ValueError
            if not isinstance(rule.get("id"), str) or not rule["id"]:
                raise ValueError
            if not isinstance(rule.get("when"), dict):
                raise ValueError
            if rule.get("minimum_grade") not in SEVERITIES:
                raise ValueError
        for grade in ("P1", "P2", "P3", "P4"):
            if int(policy["silence"]["min_remaining_minutes"][grade]) < 0:
                raise ValueError
        if int(policy["silence"]["overbroad_service_count"]) < 1:
            raise ValueError
        if int(policy["silence"]["change_risk_active_minutes"]) <= 0:
            raise ValueError
        if int(policy["replay"]["max_points"]) <= 0:
            raise ValueError
    except (KeyError, TypeError, ValueError) as exc:
        raise HandoverError("INVALID_POLICY", "The policy is incomplete or invalid.") from exc


def risk_at_least(value: str, threshold: str) -> bool:
    return RISK_ORDER.get(value, 0) >= RISK_ORDER.get(threshold, 0)
