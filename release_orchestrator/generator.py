"""Deterministic local sample and fault-injection fixture generator."""

from __future__ import annotations

import json
import random
import shutil
from pathlib import Path
from typing import Any

from .models import stable_json


DEFAULT_SEED = 20261119
BASE_EPOCH = 1_800_000_000.0
ROLLBACK_EPOCH = BASE_EPOCH + 10_000.0
TARGET_VERSION = "v2-shutdown-15s"
PREVIOUS_STABLE_VERSION = "v1-shutdown-30s"
PARAMETER_KEY = "service.shutdown_timeout_seconds"

SCENARIOS = {
    "clean",
    "instance_breach",
    "missing_samples",
    "concurrent",
    "version_not_effective",
    "mixed",
}


def _write_json(path: Path, value: Any) -> None:
    path.write_text(
        json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def _write_jsonl(path: Path, rows: list[dict[str, Any]]) -> None:
    with path.open("w", encoding="utf-8", newline="\n") as handle:
        for row in rows:
            handle.write(stable_json(row) + "\n")


def _batch_ids(seed: int) -> list[list[str]]:
    instances = [f"svc-{number:02d}" for number in range(1, 49)]
    random.Random(seed).shuffle(instances)
    return [instances[index : index + 12] for index in range(0, 48, 12)]


def _thresholds(minimum_samples: int) -> dict[str, Any]:
    return {
        "batch": {
            "error_rate": 0.02,
            "connection_reject_rate": 0.01,
        },
        "instance": {
            "error_rate": 0.04,
            "connection_reject_rate": 0.025,
        },
        "minimum_samples_per_instance": minimum_samples,
    }


def _build_plan(
    release_id: str,
    batch_ids: list[list[str]],
    *,
    observation_seconds: int,
    minimum_samples: int,
    metrics_file: str,
    version_reports_file: str,
    scenario: str,
    seed: int,
) -> dict[str, Any]:
    batches = []
    for batch_no, instances in enumerate(batch_ids, start=1):
        batches.append(
            {
                "batch": batch_no,
                "instances": sorted(instances),
                "observation_seconds": observation_seconds,
                "start_offset_seconds": batch_no * 100,
            }
        )
    return {
        "schema_version": 1,
        "release_id": release_id,
        "parameter_key": PARAMETER_KEY,
        "target_version": TARGET_VERSION,
        "previous_stable_version": PREVIOUS_STABLE_VERSION,
        "thresholds": _thresholds(minimum_samples),
        "batches": batches,
        "metrics_file": metrics_file,
        "version_reports_file": version_reports_file,
        "generation": {
            "scenario": scenario,
            "seed": seed,
        },
    }


def _build_metrics(
    release_id: str,
    batch_ids: list[list[str]],
    *,
    seed: int,
    observation_seconds: int,
    breach_instance: str | None = None,
    breach_metric: str = "connection_reject_rate",
    missing_instance: str | None = None,
) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    rng = random.Random(seed)
    for batch_no, instances in enumerate(batch_ids, start=1):
        batch_start = BASE_EPOCH + batch_no * 100
        for instance_id in sorted(instances):
            if instance_id == missing_instance:
                continue
            for second in range(observation_seconds):
                error_rate = 0.004 + rng.random() * 0.002
                reject_rate = 0.0012 + rng.random() * 0.0012
                if instance_id == breach_instance:
                    if breach_metric == "error_rate":
                        error_rate = 0.048 + rng.random() * 0.004
                    else:
                        reject_rate = 0.048 + rng.random() * 0.004
                rows.append(
                    {
                        "release_id": release_id,
                        "batch": batch_no,
                        "instance_id": instance_id,
                        "timestamp": batch_start + second,
                        "request_count": 80 + rng.randrange(41),
                        "error_rate": round(error_rate, 8),
                        "connection_reject_rate": round(reject_rate, 8),
                        "parameter_version": TARGET_VERSION,
                    }
                )
    return rows


def _build_version_reports(
    release_id: str,
    batch_ids: list[list[str]],
    *,
    observation_seconds: int,
    target_through_batch: int,
    rollback: bool,
    unapplied_instance: str | None = None,
) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for batch_no, instances in enumerate(batch_ids, start=1):
        for instance_id in instances:
            rows.append(
                {
                    "release_id": release_id,
                    "instance_id": instance_id,
                    "timestamp": BASE_EPOCH - 100 + batch_no,
                    "parameter_version": PREVIOUS_STABLE_VERSION,
                }
            )
            if batch_no <= target_through_batch:
                rows.append(
                    {
                        "release_id": release_id,
                        "instance_id": instance_id,
                        "timestamp": BASE_EPOCH
                        + batch_no * 100
                        + observation_seconds,
                        "parameter_version": TARGET_VERSION,
                    }
                )
    if rollback:
        for instances in batch_ids:
            for instance_id in instances:
                rows.append(
                    {
                        "release_id": release_id,
                        "instance_id": instance_id,
                        "timestamp": ROLLBACK_EPOCH
                        + (1 if instance_id == unapplied_instance else 0),
                        "parameter_version": (
                            TARGET_VERSION
                            if instance_id == unapplied_instance
                            else PREVIOUS_STABLE_VERSION
                        ),
                    }
                )
    return rows


def _conflict_batch_ids(seed: int, shared_instance_count: int) -> list[list[str]]:
    base = _batch_ids(seed)
    shared = sorted(base[0])[:shared_instance_count]
    remaining = [instance for batch in base[1:] for instance in batch]
    conflict_first = sorted(shared + remaining[: 12 - len(shared)])
    chosen = set(conflict_first)
    rest = sorted(
        [instance for batch in base for instance in batch if instance not in chosen]
    )
    return [conflict_first] + [
        rest[index : index + 12] for index in range(0, len(rest), 12)
    ]


def generate_scenario(
    output_dir: str,
    *,
    scenario: str = "mixed",
    seed: int = DEFAULT_SEED,
    force: bool = False,
    breach_batch: int = 2,
    breach_instance: str | None = None,
    breach_metric: str = "connection_reject_rate",
    missing_batch: int = 2,
    missing_instance: str | None = None,
    unapplied_instance: str | None = None,
    shared_instance_count: int = 5,
    observation_seconds: int = 10,
    minimum_samples: int = 5,
) -> dict[str, Any]:
    """Generate a reproducible fixture directory.

    Faults are options, never code branches in the service.
    """

    if scenario not in SCENARIOS:
        raise ValueError(
            f"unknown scenario {scenario!r}; choose from {sorted(SCENARIOS)}"
        )
    if observation_seconds < 1:
        raise ValueError("observation_seconds must be >= 1")
    if minimum_samples < 1 or minimum_samples > observation_seconds:
        raise ValueError(
            "minimum_samples must be between 1 and observation_seconds"
        )
    if breach_metric not in {"error_rate", "connection_reject_rate"}:
        raise ValueError(
            "breach_metric must be error_rate or connection_reject_rate"
        )
    if not 1 <= breach_batch <= 4 or not 1 <= missing_batch <= 4:
        raise ValueError("batch numbers must be between 1 and 4")
    if not 1 <= shared_instance_count <= 12:
        raise ValueError("shared_instance_count must be between 1 and 12")

    output = Path(output_dir).expanduser().resolve()
    if output.exists() and any(output.iterdir()) and not force:
        raise FileExistsError(
            f"output directory is not empty: {output}; pass --force to overwrite"
        )
    output.mkdir(parents=True, exist_ok=True)
    if force:
        for known in (
            "plan.json",
            "metrics.jsonl",
            "version_reports.jsonl",
            "plan_conflict.json",
            "metrics_conflict.jsonl",
            "version_reports_conflict.jsonl",
            "scenario.json",
            "report.json",
            "orchestrator.db",
            "orchestrator.db-wal",
            "orchestrator.db-shm",
        ):
            path = output / known
            if path.is_file():
                path.unlink()

    batch_ids = _batch_ids(seed)
    release_id = f"rel-{scenario}-{seed}"
    instance_to_batch = {
        instance: batch_no
        for batch_no, instances in enumerate(batch_ids, start=1)
        for instance in instances
    }

    selected_breach: str | None = None
    selected_missing: str | None = None
    selected_unapplied: str | None = None
    if scenario in {"instance_breach", "version_not_effective", "mixed"}:
        selected_breach = breach_instance or sorted(
            batch_ids[breach_batch - 1]
        )[0]
        if selected_breach not in instance_to_batch:
            raise ValueError(f"unknown breach_instance: {selected_breach}")
        breach_batch = instance_to_batch[selected_breach]
    if scenario == "missing_samples":
        selected_missing = missing_instance or sorted(
            batch_ids[missing_batch - 1]
        )[1]
        if selected_missing not in instance_to_batch:
            raise ValueError(f"unknown missing_instance: {selected_missing}")
        missing_batch = instance_to_batch[selected_missing]
    if scenario in {"version_not_effective", "mixed"}:
        selected_unapplied = unapplied_instance or sorted(
            batch_ids[breach_batch - 1]
        )[-1]
        if selected_unapplied not in instance_to_batch:
            raise ValueError(
                f"unknown unapplied_instance: {selected_unapplied}"
            )

    failure_batch = (
        breach_batch
        if selected_breach
        else missing_batch
        if selected_missing
        else 4
    )
    plan = _build_plan(
        release_id,
        batch_ids,
        observation_seconds=observation_seconds,
        minimum_samples=minimum_samples,
        metrics_file="metrics.jsonl",
        version_reports_file="version_reports.jsonl",
        scenario=scenario,
        seed=seed,
    )
    metrics = _build_metrics(
        release_id,
        batch_ids,
        seed=seed,
        observation_seconds=observation_seconds,
        breach_instance=selected_breach,
        breach_metric=breach_metric,
        missing_instance=selected_missing,
    )
    rollback = scenario in {
        "instance_breach",
        "missing_samples",
        "version_not_effective",
        "mixed",
    }
    version_reports = _build_version_reports(
        release_id,
        batch_ids,
        observation_seconds=observation_seconds,
        target_through_batch=(
            4 if scenario == "clean" else max(0, failure_batch)
        ),
        rollback=rollback,
        unapplied_instance=selected_unapplied,
    )
    _write_json(output / "plan.json", plan)
    _write_jsonl(output / "metrics.jsonl", metrics)
    _write_jsonl(output / "version_reports.jsonl", version_reports)

    conflict_plan = None
    if scenario == "concurrent":
        conflict_batches = _conflict_batch_ids(seed, shared_instance_count)
        conflict_release_id = f"rel-concurrent-late-{seed}"
        conflict_plan = _build_plan(
            conflict_release_id,
            conflict_batches,
            observation_seconds=observation_seconds,
            minimum_samples=minimum_samples,
            metrics_file="metrics_conflict.jsonl",
            version_reports_file="version_reports_conflict.jsonl",
            scenario=scenario,
            seed=seed,
        )
        conflict_metrics = _build_metrics(
            conflict_release_id,
            conflict_batches,
            seed=seed + 1,
            observation_seconds=observation_seconds,
        )
        conflict_reports = _build_version_reports(
            conflict_release_id,
            conflict_batches,
            observation_seconds=observation_seconds,
            target_through_batch=4,
            rollback=False,
        )
        _write_json(output / "plan_conflict.json", conflict_plan)
        _write_jsonl(output / "metrics_conflict.jsonl", conflict_metrics)
        _write_jsonl(
            output / "version_reports_conflict.jsonl",
            conflict_reports,
        )

    metadata = {
        "schema_version": 1,
        "scenario": scenario,
        "seed": seed,
        "release_id": release_id,
        "plan": "plan.json",
        "metrics": "metrics.jsonl",
        "version_reports": "version_reports.jsonl",
        "conflict_plan": "plan_conflict.json" if conflict_plan else None,
        "conflict_metrics": (
            "metrics_conflict.jsonl" if conflict_plan else None
        ),
        "conflict_version_reports": (
            "version_reports_conflict.jsonl" if conflict_plan else None
        ),
        "faults": {
            "breach_batch": breach_batch if selected_breach else None,
            "breach_instance": selected_breach,
            "breach_metric": breach_metric if selected_breach else None,
            "missing_batch": missing_batch if selected_missing else None,
            "missing_instance": selected_missing,
            "unapplied_instance": selected_unapplied,
            "shared_instance_count": (
                shared_instance_count if scenario == "concurrent" else None
            ),
        },
        "batches": 4,
        "instances": 48,
        "observation_seconds": observation_seconds,
        "minimum_samples_per_instance": minimum_samples,
    }
    _write_json(output / "scenario.json", metadata)
    return {
        **metadata,
        "output_dir": str(output),
        "plan_path": str(output / "plan.json"),
        "metrics_path": str(output / "metrics.jsonl"),
        "version_reports_path": str(output / "version_reports.jsonl"),
        "conflict_plan_path": (
            str(output / "plan_conflict.json") if conflict_plan else None
        ),
        "conflict_metrics_path": (
            str(output / "metrics_conflict.jsonl") if conflict_plan else None
        ),
        "conflict_version_reports_path": (
            str(output / "version_reports_conflict.jsonl")
            if conflict_plan
            else None
        ),
    }


def remove_generated(output_dir: str) -> None:
    path = Path(output_dir)
    if path.exists():
        shutil.rmtree(path)
