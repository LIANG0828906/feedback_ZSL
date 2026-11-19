from __future__ import annotations

from release_orchestrator.decision import evaluate_batch
from release_orchestrator.plan import parse_plan


def make_plan(instance_count: int = 12):
    instances = [f"svc-{index:02d}" for index in range(instance_count)]
    return parse_plan(
        {
            "release_id": "rel-decision",
            "parameter_key": "service.shutdown_timeout_seconds",
            "target_version": "v2",
            "previous_stable_version": "v1",
            "thresholds": {
                "batch": {
                    "error_rate": 0.02,
                    "connection_reject_rate": 0.01,
                },
                "instance": {
                    "error_rate": 0.04,
                    "connection_reject_rate": 0.025,
                },
                "minimum_samples_per_instance": 5,
            },
            "batches": [
                {
                    "batch": 1,
                    "instances": instances,
                    "observation_seconds": 5,
                }
            ],
        }
    )


def samples_for(plan, overrides=None):
    overrides = overrides or {}
    rows = []
    for instance in plan.batches[0].instances:
        values = overrides.get(instance, {})
        for second in range(5):
            rows.append(
                {
                    "instance_id": instance,
                    "observed_at": float(second),
                    "error_rate": values.get("error_rate", 0.005),
                    "connection_reject_rate": values.get(
                        "connection_reject_rate", 0.0018
                    ),
                    "request_count": 100,
                    "parameter_version": "v2",
                }
            )
    return rows


def test_single_instance_breach_overrides_good_batch_average():
    plan = make_plan()
    rows = samples_for(
        plan,
        {"svc-00": {"connection_reject_rate": 0.05}},
    )

    decision = evaluate_batch(plan, plan.batches[0], rows)

    assert decision.result == "aborted"
    assert decision.reason_code == "INSTANCE_THRESHOLD_EXCEEDED"
    assert decision.aggregate["connection_reject_rate"]["value"] < 0.01
    assert decision.threshold_instances == ("svc-00",)
    violation = decision.violations[0]
    assert violation.instance_id == "svc-00"
    assert violation.metric == "connection_reject_rate"
    assert violation.value == 0.05
    assert violation.threshold == 0.025
    assert violation.sample_count == 5


def test_insufficient_instance_samples_never_defaults_to_pass():
    plan = make_plan()
    rows = [
        row
        for row in samples_for(plan)
        if not (row["instance_id"] == "svc-03" and row["observed_at"] < 1)
    ]

    decision = evaluate_batch(plan, plan.batches[0], rows)

    assert decision.result == "aborted"
    assert decision.reason_code == "INSUFFICIENT_SAMPLES"
    violation = next(
        item for item in decision.violations if item.instance_id == "svc-03"
    )
    assert violation.metric == "sample_count"
    assert violation.value == 4
    assert violation.threshold == 5


def test_missing_instance_is_reported_with_zero_samples():
    plan = make_plan()
    rows = [
        row for row in samples_for(plan) if row["instance_id"] != "svc-11"
    ]

    decision = evaluate_batch(plan, plan.batches[0], rows)

    instance = next(
        item for item in decision.instances if item["instance_id"] == "svc-11"
    )
    assert instance["sample_count"] == 0
    assert decision.reason_code == "INSUFFICIENT_SAMPLES"


def test_aggregate_violation_is_also_aborted():
    plan = make_plan(instance_count=2)
    rows = samples_for(
        plan,
        {
            "svc-00": {"error_rate": 0.03},
            "svc-01": {"error_rate": 0.03},
        },
    )

    decision = evaluate_batch(plan, plan.batches[0], rows)

    assert decision.result == "aborted"
    assert decision.reason_code == "AGGREGATE_THRESHOLD_EXCEEDED"
