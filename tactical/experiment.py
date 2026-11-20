"""Deterministic two-scale experiment and byte-reproducible trace output."""

from __future__ import annotations

import json
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from unittest.mock import patch

from .errors import E_BATCH_FAILED, ServiceError
from .hashing import canonical_json_bytes, sha256_bytes, sha256_json
from .samples import (
    build_sample,
    generate_sample_file,
    read_sample,
    sample_from_dict,
    write_sample,
)
from .service import TacticalService


def _json_line(record: dict[str, Any]) -> bytes:
    return canonical_json_bytes(record) + b"\n"


def _memory_record(service: TacticalService) -> dict[str, int]:
    snapshot = service.snapshot
    field_bytes = sum(field.array_nbytes() for field in snapshot.fields.values())
    mask_bytes = snapshot.grid.height * snapshot.grid.width
    return {
        "grid_array_bytes": snapshot.grid.array_nbytes(),
        "field_array_bytes": field_bytes,
        "snapshot_array_bytes": snapshot.array_nbytes(),
        "cache_dependency_count": snapshot.cache_dependency_count,
        "cache_entries": len(snapshot.path_cache),
        "recompute_mask_bytes": mask_bytes,
        "two_version_array_bytes": snapshot.array_nbytes() * 2,
    }


@dataclass(frozen=True)
class ExperimentResult:
    size: int
    trace_path: Path
    trace_sha256: str
    baseline: dict[str, Any]
    final_version_hash: str
    final_field_hashes: dict[int, str]


def run_experiment(
    sample: dict[str, Any],
    output_dir: str | Path,
    *,
    run_probes: bool = True,
) -> ExperimentResult:
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    size = int(sample["height"])
    if int(sample["width"]) != size:
        raise ValueError("experiment requires a square sample")
    grid, targets, queries, batches = sample_from_dict(sample)
    snapshot_dir = output / f"snapshots_{size}"
    if snapshot_dir.exists():
        shutil.rmtree(snapshot_dir)
    snapshot_dir.mkdir(parents=True, exist_ok=True)

    trace = bytearray()
    sample_hash = sha256_json(sample)
    service = TacticalService(
        grid,
        targets,
        cache_queries=queries,
        store=snapshot_dir,
    )
    initial_snapshot = service.snapshot
    initial_record = {
        "record": "initial",
        "size": size,
        "sample_hash": sample_hash,
        "version": initial_snapshot.version,
        "version_hash": initial_snapshot.version_hash,
        "field_hashes": {
            str(unit): value
            for unit, value in sorted(initial_snapshot.field_hashes().items())
        },
        "generation_cells": {
            str(unit): field.generation_cells
            for unit, field in sorted(initial_snapshot.fields.items())
        },
        **_memory_record(service),
    }
    trace.extend(_json_line(initial_record))

    first_query = queries[0]
    try:
        for batch_index, changes in enumerate(batches, start=1):
            base_snapshot = service.snapshot
            prepared = service.prepare_batch(changes, verify_closure=True)
            observed = service.get_path(
                first_query[0],
                first_query[1],
                expected_version=base_snapshot.version,
                expected_hash=base_snapshot.version_hash,
            )
            isolation_ok = (
                service.version == base_snapshot.version
                and service.version_hash == base_snapshot.version_hash
                and observed.version == base_snapshot.version
                and observed.version_hash == base_snapshot.version_hash
                and prepared.snapshot.version == base_snapshot.version + 1
            )
            if not isolation_ok:
                raise RuntimeError("published snapshot isolation check failed")
            trace.extend(
                _json_line(
                    {
                        "record": "isolation",
                        "batch": batch_index,
                        "published_version": base_snapshot.version,
                        "candidate_version": prepared.snapshot.version,
                        "observed_query_version": observed.version,
                        "observed_query_hash": observed.version_hash,
                        "observed_field_hash": observed.field_hash,
                        "query_path_hash": observed.path_hash,
                        "query_cost": observed.cost,
                        "ok": True,
                    }
                )
            )
            stats = service.publish_prepared(
                prepared,
                expected_base_version=base_snapshot.version,
                expected_base_hash=base_snapshot.version_hash,
            )
            batch_record = stats.trace_record()
            batch_record["size"] = size
            batch_record.update(_memory_record(service))
            trace.extend(_json_line(batch_record))

            if run_probes and batch_index == 2:
                probe_changes = (changes[0],)
                version_before = service.version
                hash_before = service.version_hash
                probe_code = ""
                try:
                    with patch(
                        "tactical.service.incremental_update",
                        side_effect=RuntimeError("injected mid-batch failure"),
                    ):
                        service.commit_batch(probe_changes)
                except ServiceError as exc:
                    probe_code = exc.code
                if (
                    probe_code != E_BATCH_FAILED
                    or service.version != version_before
                    or service.version_hash != hash_before
                ):
                    raise RuntimeError("rollback probe did not preserve the snapshot")
                rollback_query = service.get_path(
                    first_query[0],
                    first_query[1],
                    expected_version=version_before,
                    expected_hash=hash_before,
                )
                trace.extend(
                    _json_line(
                        {
                            "record": "rollback_probe",
                            "batch": batch_index,
                            "injected_code": probe_code,
                            "version_before": version_before,
                            "version_after": service.version,
                            "hash_after": service.version_hash,
                            "old_snapshot_still_readable": rollback_query.version
                            == version_before,
                            "ok": True,
                        }
                    )
                )

        published = service.snapshot
        recovered_service = TacticalService.recover(snapshot_dir)
        try:
            recovered = recovered_service.snapshot
        finally:
            recovered_service.close()
        recovery_ok = (
            recovered.version == published.version
            and recovered.version_hash == published.version_hash
            and recovered.field_hashes() == published.field_hashes()
            and set(recovered.path_cache) == set(published.path_cache)
        )
        if not recovery_ok:
            raise RuntimeError("recovery did not reproduce the latest snapshot")
        final_record = {
            "record": "final",
            "size": size,
            "version": published.version,
            "version_hash": published.version_hash,
            "field_hashes": {
                str(unit): value
                for unit, value in sorted(published.field_hashes().items())
            },
            "generation_cells": {
                str(unit): field.generation_cells
                for unit, field in sorted(published.fields.items())
            },
            **_memory_record(service),
        }
        trace.extend(_json_line(final_record))
        trace.extend(
            _json_line(
                {
                    "record": "recovery",
                    "snapshot_version": recovered.version,
                    "snapshot_hash": recovered.version_hash,
                    "verified": True,
                }
            )
        )
    finally:
        service.close()

    trace_bytes = bytes(trace)
    trace_path = output / f"trace_{size}.jsonl"
    _atomic_write(trace_path, trace_bytes)
    trace_hash = sha256_bytes(trace_bytes)
    snapshot_files = sorted(path.name for path in snapshot_dir.glob("snapshot-*.snap"))
    baseline = {
        "format": "tactical-baseline-v1",
        "size": size,
        "sample_hash": sample_hash,
        "trace_sha256": trace_hash,
        "final_version": published.version,
        "final_version_hash": published.version_hash,
        "final_field_hashes": {
            str(unit): value for unit, value in sorted(published.field_hashes().items())
        },
        "snapshot_files": snapshot_files,
    }
    return ExperimentResult(
        size,
        trace_path,
        trace_hash,
        baseline,
        published.version_hash,
        published.field_hashes(),
    )


def _atomic_write(path: Path, data: bytes) -> None:
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_bytes(data)
    temporary.replace(path)


def baseline_document(results: list[ExperimentResult]) -> dict[str, Any]:
    return {
        "format": "tactical-baseline-v1",
        "generator": "splitmix-grid-v1",
        "samples": {
            str(result.size): result.baseline
            for result in sorted(results, key=lambda item: item.size)
        },
    }


def write_baseline(path: str | Path, results: list[ExperimentResult]) -> str:
    document = baseline_document(results)
    data = canonical_json_bytes(document) + b"\n"
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    _atomic_write(destination, data)
    return sha256_bytes(data)


def generate_and_run(
    output_dir: str | Path,
    sizes: list[int],
    seed: int,
) -> tuple[list[ExperimentResult], str]:
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    results: list[ExperimentResult] = []
    for size in sizes:
        generated = generate_sample_file(output, seed, size)
        sample = read_sample(generated.path)
        results.append(run_experiment(sample, output))
    baseline_hash = write_baseline(output / "baseline.json", results)
    return results, baseline_hash


def verify_replay(
    output_dir: str | Path,
    sizes: list[int],
    seed: int,
) -> dict[str, Any]:
    root = Path(output_dir)
    replay_dir = root / "replay"
    if replay_dir.exists():
        shutil.rmtree(replay_dir)
    replay_dir.mkdir(parents=True, exist_ok=True)
    report: dict[str, Any] = {
        "format": "tactical-replay-report-v1",
        "seed": int(seed),
        "sizes": sorted(int(size) for size in sizes),
        "checks": {},
    }
    replay_results: list[ExperimentResult] = []
    for size in sizes:
        first_trace = (root / f"trace_{size}.jsonl").read_bytes()
        first_sample = (root / f"sample_{size}.json").read_bytes()
        sample_now = build_sample(seed, size)
        write_sample(replay_dir / f"sample_{size}.json", sample_now)
        sample_after = (replay_dir / f"sample_{size}.json").read_bytes()
        result = run_experiment(sample_now, replay_dir)
        replay_trace = result.trace_path.read_bytes()
        check = {
            "sample_bytes_equal": first_sample == sample_after,
            "trace_bytes_equal": first_trace == replay_trace,
            "trace_sha256": sha256_bytes(replay_trace),
        }
        if not check["sample_bytes_equal"] or not check["trace_bytes_equal"]:
            raise RuntimeError(f"determinism check failed for size {size}")
        report["checks"][str(size)] = check
        replay_results.append(result)
    replay_baseline = baseline_document(replay_results)
    report["baseline_equal"] = replay_baseline == baseline_document_from_file(
        root / "baseline.json"
    )
    if not report["baseline_equal"]:
        raise RuntimeError("replay baseline differs")
    report["ok"] = True
    report_data = canonical_json_bytes(report) + b"\n"
    _atomic_write(root / "replay_report.json", report_data)
    return report


def baseline_document_from_file(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))
