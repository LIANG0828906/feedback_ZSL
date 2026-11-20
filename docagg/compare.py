from __future__ import annotations

import json
import math
from decimal import Decimal, localcontext
from typing import Any

from .errors import BusinessIssue, ErrorCode
from .io_utils import digest_payload


def _key_id(key: dict[str, Any]) -> str:
    return json.dumps(
        key,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    )


def refresh_summary(report: dict[str, Any]) -> None:
    errors = report.get("errors", [])
    report["summary"] = {
        "group_count": len(report.get("groups", [])),
        "error_count": len(errors),
        "precision_error_count": sum(
            error.get("code") == ErrorCode.PRECISION_BOUND.value for error in errors
        ),
        "deterministic_digest": digest_payload(
            {
                "groups": report.get("groups", []),
                "errors": errors,
            }
        ),
    }


def validate_against_reference(
    report: dict[str, Any],
    reference: dict[str, Any],
) -> list[BusinessIssue]:
    reference_groups = {
        _key_id(group["key"]): group for group in reference.get("groups", [])
    }
    report_groups = {
        _key_id(group["key"]): group for group in report.get("groups", [])
    }
    new_issues: list[BusinessIssue] = []

    for key_id, ref_group in sorted(reference_groups.items()):
        report_group = report_groups.get(key_id)
        for name, ref_aggregate in sorted(ref_group["aggregates"].items()):
            if ref_aggregate.get("status") != "ok":
                continue
            if report_group is None:
                new_issues.append(
                    BusinessIssue(
                        ErrorCode.PRECISION_BOUND,
                        "参考真值中的分组未出现在执行结果中",
                        group_key=ref_group["key"],
                        aggregate=name,
                    )
                )
                continue
            actual_aggregate = report_group["aggregates"].get(name)
            if actual_aggregate is None or actual_aggregate.get("status") != "ok":
                continue

            actual_value = actual_aggregate["value"]
            reference_value = Decimal(str(ref_aggregate["value_text"]))
            actual_decimal = Decimal.from_float(float(actual_value))
            with localcontext() as ctx:
                ctx.prec = 200
                deviation = abs(actual_decimal - reference_value)
                estimate = Decimal(str(actual_aggregate["error_estimate"]))
                tolerance = (
                    estimate * Decimal("1.000000001") + Decimal("1e-300")
                )
                within = deviation <= tolerance
            deviation_float = float(deviation)
            if not math.isfinite(deviation_float):
                deviation_float = str(deviation)

            actual_aggregate["reference_check"] = {
                "basis": "value_before_rounding",
                "reference_method": ref_aggregate.get("method"),
                "reference_value": ref_aggregate.get("value"),
                "reference_value_text": ref_aggregate.get("value_text"),
                "absolute_deviation": deviation_float,
                "error_estimate": float(estimate),
                "within_declared_error": within,
            }
            if not within:
                issue = BusinessIssue(
                    ErrorCode.PRECISION_BOUND,
                    "执行结果与参考真值的偏差超过声明误差",
                    group_key=report_group["key"],
                    field=actual_aggregate.get("field"),
                    aggregate=name,
                    details={
                        "absolute_deviation": deviation_float,
                        "error_estimate": float(estimate),
                        "reference_method": ref_aggregate.get("method"),
                    },
                )
                new_issues.append(issue)

    report.setdefault("errors", []).extend(issue.to_dict() for issue in new_issues)
    report["reference_validation"] = {
        "reference_group_count": len(reference_groups),
        "checked_aggregate_count": sum(
            1
            for group in report.get("groups", [])
            for aggregate in group.get("aggregates", {}).values()
            if aggregate.get("status") == "ok"
            and aggregate.get("reference_check") is not None
        ),
        "failed_count": len(new_issues),
        "all_within_declared_error": not new_issues,
    }
    refresh_summary(report)
    return new_issues
