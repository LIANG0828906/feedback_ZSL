from __future__ import annotations

import sqlite3

import pytest

from release_orchestrator.errors import PlanValidationError
from release_orchestrator.plan import parse_plan
from release_orchestrator.service import Orchestrator


def base_plan() -> dict:
    return {
        "release_id": "rel-validation",
        "parameter_key": "service.shutdown_timeout_seconds",
        "target_version": "v2",
        "previous_stable_version": "v1",
        "batches": [
            {"batch": 1, "instances": ["svc-1"], "observation_seconds": 5},
            {"batch": 2, "instances": ["svc-2"], "observation_seconds": 5},
        ],
    }


def test_missing_parameter_version_rejects_whole_plan(tmp_path):
    plan = base_plan()
    del plan["previous_stable_version"]
    db_path = tmp_path / "orchestrator.db"
    orchestrator = Orchestrator(str(db_path))

    with pytest.raises(PlanValidationError) as error:
        orchestrator.start_release(plan)

    assert error.value.code == "PLAN_VERSION_MISSING"
    assert error.value.details["issues"][0]["field"] == "previous_stable_version"
    with sqlite3.connect(db_path) as connection:
        assert connection.execute("SELECT COUNT(*) FROM releases").fetchone()[0] == 0
        assert connection.execute("SELECT COUNT(*) FROM batches").fetchone()[0] == 0


def test_duplicate_instance_is_rejected_with_batch_context():
    plan = base_plan()
    plan["batches"][1]["instances"] = ["svc-1"]

    with pytest.raises(PlanValidationError) as error:
        parse_plan(plan)

    assert error.value.code == "PLAN_DUPLICATE_INSTANCE"
    issue = error.value.details["issues"][0]
    assert issue["batch"] == 2
    assert issue["instance_id"] == "svc-1"


def test_overlapping_observation_windows_are_rejected():
    plan = base_plan()
    plan["batches"][0]["start_offset_seconds"] = 0
    plan["batches"][1]["start_offset_seconds"] = 3

    with pytest.raises(PlanValidationError) as error:
        parse_plan(plan)

    assert error.value.code == "PLAN_BATCH_OVERLAP"
    assert {
        error.value.details["issues"][0]["batch"],
        error.value.details["issues"][0]["other_batch"],
    } == {1, 2}


def test_duplicate_batch_number_is_rejected():
    plan = base_plan()
    plan["batches"][1]["batch"] = 1

    with pytest.raises(PlanValidationError) as error:
        parse_plan(plan)

    assert error.value.code == "PLAN_BATCH_OVERLAP"


def test_partially_declared_batch_versions_are_rejected():
    plan = base_plan()
    plan["batches"][0]["parameter_version"] = "v2"

    with pytest.raises(PlanValidationError) as error:
        parse_plan(plan)

    assert error.value.code == "PLAN_VERSION_MISSING"
    issue = error.value.details["issues"][0]
    assert issue["batch"] == 2
    assert issue["instance_id"] == "svc-2"


def test_missing_observation_duration_is_rejected():
    plan = base_plan()
    del plan["batches"][0]["observation_seconds"]

    with pytest.raises(PlanValidationError) as error:
        parse_plan(plan)

    assert error.value.details["issues"]
