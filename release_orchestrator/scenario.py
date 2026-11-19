"""End-to-end deterministic scenario runner and JSON report writer."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

from .generator import generate_scenario
from .models import stable_json
from .service import Orchestrator


def _release_report(
    orchestrator: Orchestrator, release_id: str
) -> dict[str, Any]:
    report = orchestrator.release_report(release_id)
    status = orchestrator.get_status(release_id)
    report["conflicts"] = status["conflicts"]
    report["decision_sequence"] = status["decision_sequence"]
    report["rollback"] = status["rollback"]
    report["status_detail"] = status
    report["audit_records"] = orchestrator.get_audit(release_id)
    return report


def run_scenario(
    output_dir: str,
    *,
    scenario: str = "mixed",
    seed: int = 20261119,
    force: bool = False,
    **generator_options: Any,
) -> dict[str, Any]:
    generated = generate_scenario(
        output_dir,
        scenario=scenario,
        seed=seed,
        force=force,
        **generator_options,
    )
    output = Path(generated["output_dir"])
    db_path = output / "orchestrator.db"
    for suffix in ("", "-wal", "-shm"):
        candidate = Path(str(db_path) + suffix)
        if candidate.exists():
            candidate.unlink()
    orchestrator = Orchestrator(str(db_path))
    reports: list[dict[str, Any]] = []

    main = orchestrator.start_from_sample_dir(str(output))
    main_id = str(main["release_id"])

    if scenario == "concurrent":
        orchestrator.evaluate_next(main_id)
        conflict = orchestrator.start_release(
            generated["conflict_plan_path"],
            metrics_path=generated["conflict_metrics_path"],
            version_reports_path=generated["conflict_version_reports_path"],
            sample_dir=str(output),
        )
        conflict_id = str(conflict["release_id"])
        orchestrator.evaluate_next(conflict_id)
        orchestrator.run_release(main_id)
        reports.append(_release_report(orchestrator, main_id))
        reports.append(_release_report(orchestrator, conflict_id))
    else:
        status = orchestrator.run_release(main_id)
        if status["rollback"]["required"]:
            status = orchestrator.verify_rollback(main_id)
        reports.append(_release_report(orchestrator, main_id))

    report = {
        "schema_version": 1,
        "scenario": scenario,
        "seed": seed,
        "samples_dir": str(output),
        "database": str(db_path),
        "faults": generated["faults"],
        "releases": reports,
        "summary": {
            "release_count": len(reports),
            "completed": sum(
                1 for item in reports if item["status"] == "completed"
            ),
            "rollback_required": sum(
                1 for item in reports if item["rollback_required"]
            ),
            "rollback_completed": sum(
                1 for item in reports if item["rollback_completed"]
            ),
            "blocked": sum(
                1 for item in reports if item["status"] == "blocked"
            ),
            "ineffective_instance_count": sum(
                item["ineffective_instance_count"] for item in reports
            ),
        },
        "reproduce": (
            f"python run_scenario.py --scenario {scenario} --seed {seed} "
            f"--output {output} --force"
        ),
    }
    report_path = output / "report.json"
    report_path.write_text(
        json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    report["report_path"] = str(report_path)
    return report


def write_report(path: str, report: dict[str, Any]) -> None:
    Path(path).write_text(
        json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
