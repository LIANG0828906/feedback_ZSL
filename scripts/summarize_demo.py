from __future__ import annotations

import argparse
import hashlib
import json
import platform
from pathlib import Path
from typing import Any

import numpy as np


ROOT = Path(__file__).resolve().parents[1]


def load(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def error_codes(report: dict[str, Any]) -> list[str]:
    return sorted({error["code"] for error in report.get("errors", [])})


def all_reference_checks_pass(report: dict[str, Any]) -> bool:
    checks = [
        aggregate.get("reference_check")
        for group in report.get("groups", [])
        for aggregate in group.get("aggregates", {}).values()
        if aggregate.get("status") == "ok"
    ]
    return bool(checks) and all(
        check is not None and check["within_declared_error"]
        for check in checks
    )


def spilled_bounds_not_smaller(
    spilled: dict[str, Any],
    no_spill: dict[str, Any],
) -> bool:
    def by_key(report: dict[str, Any]) -> dict[str, dict[str, Any]]:
        return {
            json.dumps(group["key"], sort_keys=True): group
            for group in report["groups"]
        }

    spilled_groups = by_key(spilled)
    no_spill_groups = by_key(no_spill)
    if set(spilled_groups) != set(no_spill_groups):
        return False
    for key, group in spilled_groups.items():
        for name, aggregate in group["aggregates"].items():
            other = no_spill_groups[key]["aggregates"][name]
            if aggregate.get("status") != "ok":
                continue
            if aggregate["error_estimate"] < other["error_estimate"]:
                return False
    return True


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--artifacts", required=True)
    parser.add_argument("--repeat-artifacts", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    artifacts = Path(args.artifacts)
    repeat = Path(args.repeat_artifacts)
    normal = load(artifacts / "normal_report.json")
    anomalies = load(artifacts / "anomalies_report.json")
    no_spill = load(artifacts / "no_spill_report.json")
    spill_failure = load(artifacts / "spill_failure_report.json")

    compared_files = [
        "normal_report.json",
        "reference.json",
        "anomalies_report.json",
        "no_spill_report.json",
        "spill_failure_report.json",
    ]
    deterministic = all(
        (artifacts / name).read_bytes() == (repeat / name).read_bytes()
        for name in compared_files
    )

    normal_spill = normal["spill"]
    no_spill_spill = no_spill["spill"]
    summary = {
        "schema_version": "1.0",
        "environment": {
            "python": platform.python_version(),
            "numpy": np.__version__,
        },
        "samples": {
            "events": {
                "path": "samples/events.jsonl",
                "sha256": sha256(ROOT / "samples" / "events.jsonl"),
                "documents": normal["input"]["documents"],
            },
            "anomalies": {
                "path": "samples/anomalies.jsonl",
                "sha256": sha256(ROOT / "samples" / "anomalies.jsonl"),
                "documents": anomalies["input"]["documents"],
            },
        },
        "normal": {
            "deterministic_digest": normal["summary"]["deterministic_digest"],
            "group_count": normal["summary"]["group_count"],
            "error_count": normal["summary"]["error_count"],
            "precision_error_count": normal["summary"]["precision_error_count"],
            "all_within_declared_error": all_reference_checks_pass(normal),
            "spill": {
                "successful_spills": normal_spill["successful_spills"],
                "failed_spills": normal_spill["failed_spills"],
                "group_occurrences_before_merge": normal_spill[
                    "group_occurrences_before_merge"
                ],
                "distinct_groups_before_merge": normal_spill[
                    "distinct_groups_in_partials"
                ],
                "distinct_groups_after_merge": normal_spill[
                    "distinct_groups_after_merge"
                ],
                "group_count_consistent": normal_spill["group_count_consistent"],
                "merge_files": len(normal_spill["merge_order"]),
            },
        },
        "no_spill_comparison": {
            "group_count": no_spill["summary"]["group_count"],
            "same_group_count": (
                normal["summary"]["group_count"] == no_spill["summary"]["group_count"]
            ),
            "successful_spills": no_spill_spill["successful_spills"],
            "all_within_declared_error": all_reference_checks_pass(no_spill),
            "spilled_error_bounds_not_smaller": spilled_bounds_not_smaller(
                normal, no_spill
            ),
        },
        "anomalies": {
            "group_count": anomalies["summary"]["group_count"],
            "error_count": anomalies["summary"]["error_count"],
            "error_codes": error_codes(anomalies),
            "good_group_still_completed": any(
                group["key"] == {"bucket": 1, "region": "north", "tenant": "good"}
                and all(
                    aggregate.get("status") == "ok"
                    for aggregate in group["aggregates"].values()
                )
                for group in anomalies["groups"]
            ),
        },
        "spill_failure_injection": {
            "error_codes": error_codes(spill_failure),
            "failed_spills": spill_failure["spill"]["failed_spills"],
            "group_count_after_failure": spill_failure["summary"]["group_count"],
            "group_count_consistent": spill_failure["spill"][
                "group_count_consistent"
            ],
        },
        "determinism": {
            "compared_files": compared_files,
            "byte_identical_on_repeat": deterministic,
        },
    }
    expected_anomaly_codes = {
        "E_FIELD_MISSING",
        "E_TYPE_INCOMPARABLE",
        "E_SCALE_MISMATCH",
        "E_STDDEV_INSUFFICIENT_SAMPLES",
    }
    acceptance_passed = all(
        [
            deterministic,
            summary["normal"]["all_within_declared_error"],
            summary["normal"]["precision_error_count"] == 0,
            summary["normal"]["spill"]["group_count_consistent"],
            summary["no_spill_comparison"]["all_within_declared_error"],
            summary["no_spill_comparison"]["same_group_count"],
            summary["no_spill_comparison"][
                "spilled_error_bounds_not_smaller"
            ],
            summary["anomalies"]["good_group_still_completed"],
            expected_anomaly_codes.issubset(
                set(summary["anomalies"]["error_codes"])
            ),
            "E_SPILL_WRITE_FAILED"
            in summary["spill_failure_injection"]["error_codes"],
            summary["spill_failure_injection"]["group_count_consistent"],
            summary["spill_failure_injection"]["group_count_after_failure"] == 5,
        ]
    )
    summary["acceptance_passed"] = acceptance_passed
    output = Path(args.output)
    output.write_text(
        json.dumps(summary, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    print(output.read_text(encoding="utf-8"), end="")
    return 0 if acceptance_passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
