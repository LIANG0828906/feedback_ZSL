from __future__ import annotations

import json
import sqlite3

import pytest

from release_orchestrator.errors import BusinessError
from release_orchestrator.generator import generate_scenario
from release_orchestrator.scenario import run_scenario
from release_orchestrator.service import Orchestrator


def test_mixed_scenario_aborts_and_survives_restart(tmp_path):
    output = tmp_path / "mixed"
    report = run_scenario(
        str(output),
        scenario="mixed",
        seed=20261119,
        force=True,
    )
    release = report["releases"][0]
    assert release["batches"][0]["result"] == "released"
    assert release["batches"][1]["result"] == "aborted"
    assert release["batches"][1]["reason_code"] == "INSTANCE_THRESHOLD_EXCEEDED"
    assert release["batches"][2]["result"] == "not_released"
    assert release["batches"][3]["result"] == "not_released"
    assert release["rollback_target_version"] == "v1-shutdown-30s"
    assert release["rollback"]["trigger_batch"] == 2
    assert release["rollback"]["trigger_instances"]
    assert release["ineffective_instance_count"] == 1
    assert release["status"] == "rollback_in_progress"

    # A new process/instance sees the same persisted aborted state.
    restarted = Orchestrator(str(output / "orchestrator.db"))
    status = restarted.get_status(release["release_id"])
    assert status["status"] == "rollback_in_progress"
    assert status["batches"][1]["status"] == "aborted"
    assert status["rollback"]["ineffective_instances"][0]["instance_id"] == (
        "svc-39"
    )
    with pytest.raises(BusinessError) as error:
        restarted.evaluate_next(release["release_id"])
    assert error.value.code == "RELEASE_TERMINAL"

    # The failed batch and passed batch stay occupied until rollback verifies.
    with sqlite3.connect(output / "orchestrator.db") as connection:
        lock_count = connection.execute(
            "SELECT COUNT(*) FROM instance_locks WHERE release_id = ?",
            (release["release_id"],),
        ).fetchone()[0]
        decided = connection.execute(
            """
            SELECT COUNT(*) FROM batches
            WHERE release_id = ? AND status <> 'pending'
              AND decision_json IS NOT NULL
            """,
            (release["release_id"],),
        ).fetchone()[0]
        audit = connection.execute(
            """
            SELECT COUNT(*) FROM decision_records
            WHERE release_id = ? AND record_type = 'batch_decision'
            """,
            (release["release_id"],),
        ).fetchone()[0]
    assert lock_count == 24
    assert decided == 2
    assert audit == 2


def test_instance_breach_rollback_completes_after_version_report(tmp_path):
    output = tmp_path / "breach"
    report = run_scenario(
        str(output),
        scenario="instance_breach",
        seed=20261119,
        force=True,
    )
    release = report["releases"][0]
    assert release["status"] == "rolled_back"
    assert release["rollback_completed"] is True
    assert release["ineffective_instance_count"] == 0
    assert release["threshold_instance_count"] == 1

    with sqlite3.connect(output / "orchestrator.db") as connection:
        assert (
            connection.execute(
                "SELECT COUNT(*) FROM instance_locks WHERE release_id = ?",
                (release["release_id"],),
            ).fetchone()[0]
            == 0
        )


def test_missing_samples_trigger_insufficient_samples(tmp_path):
    output = tmp_path / "missing"
    report = run_scenario(
        str(output),
        scenario="missing_samples",
        seed=20261119,
        force=True,
    )
    release = report["releases"][0]
    assert release["batches"][1]["reason_code"] == "INSUFFICIENT_SAMPLES"
    assert release["batches"][1]["trigger_instances"]
    assert release["status"] == "rolled_back"


def test_version_not_effective_blocks_rolled_back_state(tmp_path):
    output = tmp_path / "version"
    report = run_scenario(
        str(output),
        scenario="version_not_effective",
        seed=20261119,
        force=True,
    )
    release = report["releases"][0]
    assert release["status"] == "rollback_in_progress"
    assert release["rollback_completed"] is False
    assert release["ineffective_instance_count"] == 1
    ineffective = release["rollback"]["ineffective_instances"][0]
    assert ineffective["instance_id"] == "svc-39"
    assert ineffective["reported_version"] == "v2-shutdown-15s"
    assert ineffective["observed_at"]


def test_concurrent_release_conflict_does_not_rejudge_released_batch(tmp_path):
    output = tmp_path / "concurrent"
    report = run_scenario(
        str(output),
        scenario="concurrent",
        seed=20261119,
        force=True,
    )
    first, late = report["releases"]
    assert first["status"] == "completed"
    assert first["batches"][0]["result"] == "released"
    assert late["status"] == "blocked"
    assert late["rollback_required"] is False
    assert late["batches"][0]["reason_code"] == "INSTANCE_OCCUPANCY_CONFLICT"
    assert late["conflict_release_ids"] == [first["release_id"]]
    assert late["batches"][1]["result"] == "not_released"


def test_batch_one_decision_is_not_replayed_after_restart(tmp_path):
    output = tmp_path / "clean"
    generate_scenario(
        str(output),
        scenario="clean",
        seed=20261119,
        force=True,
    )
    db_path = output / "orchestrator.db"
    first = Orchestrator(str(db_path))
    started = first.start_from_sample_dir(str(output))
    release_id = started["release_id"]
    first.evaluate_next(release_id)
    assert first.get_status(release_id)["batches"][0]["status"] == "passed"

    second = Orchestrator(str(db_path))
    status = second.run_release(release_id)
    assert status["status"] == "completed"
    assert [item["result"] for item in status["batches"]] == [
        "released",
        "released",
        "released",
        "released",
    ]
    with sqlite3.connect(db_path) as connection:
        batch_one_decisions = connection.execute(
            """
            SELECT COUNT(*) FROM decision_records
            WHERE release_id = ? AND batch_no = 1
              AND record_type = 'batch_decision'
            """,
            (release_id,),
        ).fetchone()[0]
    assert batch_one_decisions == 1
