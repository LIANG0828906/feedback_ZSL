from __future__ import annotations

import json

from release_orchestrator.generator import generate_scenario


def read_jsonl(path):
    return [
        json.loads(line)
        for line in path.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]


def test_generator_is_byte_reproducible(tmp_path):
    first = tmp_path / "first"
    second = tmp_path / "second"
    generate_scenario(str(first), scenario="clean", seed=918273, force=True)
    generate_scenario(str(second), scenario="clean", seed=918273, force=True)

    for name in (
        "plan.json",
        "metrics.jsonl",
        "version_reports.jsonl",
        "scenario.json",
    ):
        assert (first / name).read_bytes() == (second / name).read_bytes()

    plan = json.loads((first / "plan.json").read_text(encoding="utf-8"))
    assert len(plan["batches"]) == 4
    assert all(len(batch["instances"]) == 12 for batch in plan["batches"])
    assert len({item for batch in plan["batches"] for item in batch["instances"]}) == 48
    assert len(read_jsonl(first / "metrics.jsonl")) == 48 * 10


def test_fault_injection_options_only_change_fixture(tmp_path):
    output = tmp_path / "fault"
    generated = generate_scenario(
        str(output),
        scenario="mixed",
        seed=55,
        force=True,
        breach_instance="svc-01",
        unapplied_instance="svc-02",
    )
    assert generated["faults"]["breach_instance"] == "svc-01"
    assert generated["faults"]["unapplied_instance"] == "svc-02"
    metrics = read_jsonl(output / "metrics.jsonl")
    bad = [
        row
        for row in metrics
        if row["instance_id"] == "svc-01"
        and row["batch"] == generated["faults"]["breach_batch"]
    ]
    assert bad
    assert all(row["connection_reject_rate"] > 0.025 for row in bad)
    reports = read_jsonl(output / "version_reports.jsonl")
    latest_for_unapplied = max(
        (
            row
            for row in reports
            if row["instance_id"] == "svc-02"
        ),
        key=lambda row: row["timestamp"],
    )
    assert latest_for_unapplied["parameter_version"] == "v2-shutdown-15s"
