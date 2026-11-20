"""Reproducible end-to-end demonstration with a real forced process termination."""

from __future__ import annotations

import hashlib
import json
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any

from .audio import generate_sample_set
from .config import FingerprintConfig
from .errors import BizError
from .fingerprint import compare_extractions
from .library import Library, TrackSpec
from .migration import MigrationContext
from .records import pcm_compression_metadata
from .retrieval import compare_query_sequences
from .util import TraceLogger, atomic_write_json, canonical_bytes


def _project_root() -> Path:
    return Path(__file__).resolve().parents[1]


def _sha_file(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _run_cli(args: list[str]) -> subprocess.CompletedProcess[bytes]:
    return subprocess.run(
        [sys.executable, "-m", "fpstore.cli", *args],
        cwd=_project_root(),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )


def _dump_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2).encode("utf-8") + b"\n")


def _query_sequence(
    library: Library,
    sample_rows: list[dict[str, Any]],
    config: FingerprintConfig,
    trace: TraceLogger,
    *,
    phase: str,
) -> list[dict[str, Any]]:
    results = []
    for row in sample_rows:
        name = str(row["track_id"])
        result = library.search(Path(str(row["path"])), config=config, track_id=f"{phase}:{name}", top_k=3)
        results.append(result)
        trace.emit(
            f"query_{phase}",
            query=name,
            library_version_hash=result["library_version_hash"],
            sample=name,
            candidates=[
                {
                    "sample": candidate["sample"],
                    "fingerprint_version": candidate["fingerprint_version"],
                    "offset_ms": candidate["offset_ms"],
                    "confidence": candidate["confidence"],
                    "hashes": candidate["match_hashes"][:3],
                }
                for candidate in result["candidates"]
            ],
            alignment_error_count=len(result["alignment_errors"]),
            snapshot_released=result["snapshot_released"],
        )
    return results


def _compact_diff(report: dict[str, Any], limit: int = 40) -> dict[str, Any]:
    compact = dict(report)
    details = report.get("details", [])
    compact["details_total"] = len(details)
    compact["details"] = details[:limit]
    compact["details_truncated"] = max(0, len(details) - limit)
    return compact


def _execute_demo(work: Path, seed: int, crash_after: int) -> dict[str, Any]:
    if work.exists():
        shutil.rmtree(work)
    work.mkdir(parents=True)
    samples_dir = work / "samples"
    sample_rows = generate_sample_set(samples_dir, seed=seed)
    trace = TraceLogger(work / "trace.jsonl")
    trace.emit(
        "demo_start",
        seed=seed,
        sample_count=len(sample_rows),
        samples=[
            {
                "track_id": row["track_id"],
                "sample_rate": row["sample_rate"],
                "channels": row["channels"],
                "duration_s": row["duration_s"],
            }
            for row in sample_rows
        ],
    )

    v3 = FingerprintConfig(version=3, time_quant_bits=0)
    library_root = work / "library"
    library = Library(library_root)
    import_base = [
        "import",
        "--library",
        str(library_root),
        "--samples-dir",
        str(samples_dir),
        "--track-id-from-stem",
        "--version",
        "3",
        "--quant-bits",
        "0",
        "--trace",
        str(work / "trace.jsonl"),
        "--crash-after",
        str(crash_after),
    ]
    crashed = _run_cli(import_base)
    if crashed.returncode != 137:
        raise BizError(
            "E_STATE_INVALID",
            "forced-termination subprocess did not exit with code 137",
            {"returncode": crashed.returncode, "stdout": crashed.stdout.decode(errors="replace"), "stderr": crashed.stderr.decode(errors="replace")},
        )
    after_crash = library.staging_details()
    if after_crash is None:
        raise BizError("E_RECOVERY_INVALID", "crash occurred before a committed staging snapshot existed")
    trace.emit(
        "crash_observed",
        process_returncode=crashed.returncode,
        committed_segment_count=after_crash["entry_count"],
        staging_manifest_hash=after_crash["manifest_hash"],
        published_version_hash=after_crash["published_version_hash"],
        committed_segments=after_crash["committed_segments"],
    )
    resumed = _run_cli(
        [
            "import",
            "--library",
            str(library_root),
            "--resume",
            "--version",
            "3",
            "--quant-bits",
            "0",
            "--trace",
            str(work / "trace.jsonl"),
        ]
    )
    if resumed.returncode != 0:
        raise BizError(
            "E_RECOVERY_INVALID",
            "resume subprocess failed",
            {"stdout": resumed.stdout.decode(errors="replace"), "stderr": resumed.stderr.decode(errors="replace")},
        )
    resume_result = json.loads(resumed.stdout)
    after_resume = library.staging_details()
    assert after_resume is not None
    trace.emit(
        "recovery_complete",
        staging_manifest_hash=after_resume["manifest_hash"],
        committed_segment_count=after_resume["entry_count"],
        new_commits_this_run=resume_result["committed_segments"],
        skipped_segments=resume_result["skipped_segments"],
        chain_root=after_resume["root_chain_hash"],
    )
    publish_v3 = library.publish(trace=trace)
    trace.emit(
        "query_after_first_publish",
        library_version_hash=publish_v3["published_version_hash"],
        entry_count=publish_v3["entry_count"],
    )
    first_queries = _query_sequence(library, sample_rows, v3, trace, phase="v3_first")

    old_snapshot = library.open_snapshot()
    old_hash = old_snapshot.library_version_hash
    old_record_count = len(old_snapshot.records)
    v2 = FingerprintConfig(version=2, time_quant_bits=0)
    add_v2 = library.import_tracks(
        [TrackSpec(str(row["track_id"]), Path(str(row["path"])), v2) for row in sample_rows],
        trace=trace,
    )
    publish_second = library.publish(trace=trace)
    new_snapshot = library.open_snapshot()
    new_hash = new_snapshot.library_version_hash
    trace.emit(
        "publish_isolation",
        old_published_version_hash=old_hash,
        new_published_version_hash=new_hash,
        old_snapshot_record_count=old_record_count,
        new_snapshot_record_count=len(new_snapshot.records),
        old_snapshot_still_readable=(old_snapshot.closed is False),
        second_import_committed_segments=add_v2.committed_segments,
        isolation_choice="old-or-new-complete-snapshot",
    )
    old_snapshot.close()
    new_snapshot.close()
    second_queries = _query_sequence(library, sample_rows, v3, trace, phase="v3_second")

    migration_library_root = work / "migration_library"
    migration_library = Library(migration_library_root)
    v1 = FingerprintConfig(version=1, time_quant_bits=None)
    migration_library.import_tracks(
        [TrackSpec(str(row["track_id"]), Path(str(row["path"])), v1) for row in sample_rows],
        trace=trace,
    )
    migration_publish_before = migration_library.publish(trace=trace)
    before_queries = _query_sequence(migration_library, sample_rows, v1, trace, phase="before_migration")
    migration_result = migration_library.migrate(
        MigrationContext("not_recorded", pcm_compression_metadata(16)),
        trace=trace,
    )
    after_queries = _query_sequence(migration_library, sample_rows, v1, trace, phase="after_migration")
    consistency = compare_query_sequences(
        before_queries,
        after_queries,
        tolerance_ms=20,
        query_names=[str(row["track_id"]) for row in sample_rows],
    )
    _dump_json(work / "migration_report.json", migration_result["report"])
    _dump_json(work / "migration_consistency.json", consistency)
    trace.emit(
        "migration_consistency",
        old_published_version_hash=migration_result["old_published_version_hash"],
        published_version_hash=migration_result["published_version_hash"],
        migrated_records=migration_result["migrated_records"],
        consistency_ok=consistency["ok"],
        difference_count=consistency["difference_count"],
        differences=consistency["differences"],
    )

    stereo_row = next(row for row in sample_rows if int(row["channels"]) == 2)
    stereo_path = Path(str(stereo_row["path"]))
    quant_diff = compare_extractions(
        stereo_path,
        FingerprintConfig(version=3, time_quant_bits=0),
        FingerprintConfig(version=3, time_quant_bits=5),
    )
    band_diff = compare_extractions(
        stereo_path,
        FingerprintConfig(version=3, time_quant_bits=0, band_edges_hz=(0, 500, 2000, 4000, 8000)),
        FingerprintConfig(version=3, time_quant_bits=0, band_edges_hz=(0, 1000, 3000, 6000)),
    )
    channel_diff = compare_extractions(
        stereo_path,
        FingerprintConfig(version=3, time_quant_bits=0, channel_merge="mean"),
        FingerprintConfig(version=3, time_quant_bits=0, channel_merge="left"),
    )
    recompute_report = {
        "track_id": stereo_row["track_id"],
        "quantization": _compact_diff(quant_diff),
        "band_layout": _compact_diff(band_diff),
        "channel_merge": _compact_diff(channel_diff),
        "max_abs_offset_ms_overall": max(
            quant_diff["max_abs_offset_ms"],
            band_diff["max_abs_offset_ms"],
            channel_diff["max_abs_offset_ms"],
        ),
    }
    _dump_json(work / "recompute_report.json", recompute_report)
    trace.emit(
        "recompute_diff",
        track_id=stereo_row["track_id"],
        quantization={
            "affected": quant_diff["affected"],
            "offset_changed_pairs": quant_diff["offset_changed_pairs"],
            "hash_changed_pairs": quant_diff["hash_changed_pairs"],
            "max_abs_offset_ms": quant_diff["max_abs_offset_ms"],
        },
        band_layout={
            "affected": band_diff["affected"],
            "offset_changed_pairs": band_diff["offset_changed_pairs"],
            "hash_changed_pairs": band_diff["hash_changed_pairs"],
            "max_abs_offset_ms": band_diff["max_abs_offset_ms"],
        },
        channel_merge={
            "affected": channel_diff["affected"],
            "offset_changed_pairs": channel_diff["offset_changed_pairs"],
            "hash_changed_pairs": channel_diff["hash_changed_pairs"],
            "max_abs_offset_ms": channel_diff["max_abs_offset_ms"],
        },
    )

    final_status = library.status()
    summary = {
        "seed": seed,
        "crash_after_segments": crash_after,
        "crash_returncode": crashed.returncode,
        "committed_after_crash": after_crash["entry_count"],
        "total_committed_after_resume": after_resume["entry_count"],
        "resume_new_commits": resume_result["committed_segments"],
        "resume_skipped_committed": resume_result["skipped_segments"],
        "first_published_version_hash": publish_v3["published_version_hash"],
        "second_published_version_hash": publish_second["published_version_hash"],
        "isolation_old_snapshot_hash": old_hash,
        "isolation_new_snapshot_hash": new_hash,
        "latest_published_version_hash": final_status["published"]["manifest_hash"],
        "first_query_top": [
            {
                "query": row["track_id"],
                "library_version_hash": result["library_version_hash"],
                "candidates": [
                    {
                        "sample": candidate["sample"],
                        "offset_ms": candidate["offset_ms"],
                        "confidence": candidate["confidence"],
                    }
                    for candidate in result["candidates"][:2]
                ],
            }
            for row, result in zip(sample_rows, first_queries)
        ],
        "second_query_top": [
            {
                "query": row["track_id"],
                "library_version_hash": result["library_version_hash"],
                "candidates": [
                    {
                        "sample": candidate["sample"],
                        "offset_ms": candidate["offset_ms"],
                        "confidence": candidate["confidence"],
                    }
                    for candidate in result["candidates"][:2]
                ],
            }
            for row, result in zip(sample_rows, second_queries)
        ],
        "migration_old_version_hash": migration_publish_before["published_version_hash"],
        "migration_new_version_hash": migration_result["published_version_hash"],
        "migration_consistency_ok": consistency["ok"],
        "migration_difference_count": consistency["difference_count"],
        "recompute_max_abs_offset_ms": recompute_report["max_abs_offset_ms_overall"],
        "trace_sha256": None,
    }
    _dump_json(work / "demo_summary.json", summary)
    trace.emit(
        "demo_complete",
        first_published_version_hash=summary["first_published_version_hash"],
        latest_published_version_hash=summary["latest_published_version_hash"],
        migration_consistency_ok=summary["migration_consistency_ok"],
        recompute_max_abs_offset_ms=summary["recompute_max_abs_offset_ms"],
    )
    summary["trace_sha256"] = _sha_file(trace.path)
    _dump_json(work / "demo_summary.json", summary)
    summary["report_sha256"] = _sha_file(work / "demo_summary.json")
    return summary


def run_demo(
    work: Path,
    *,
    seed: int = 20261121,
    crash_after: int = 3,
    verify_determinism: bool = False,
) -> dict[str, Any]:
    summary = _execute_demo(work, seed, crash_after)
    if verify_determinism:
        det_root = work.parent / f"{work.name}-determinism"
        first = _execute_demo(det_root / "run-a", seed, crash_after)
        second = _execute_demo(det_root / "run-b", seed, crash_after)
        trace_a = (det_root / "run-a" / "trace.jsonl").read_bytes()
        trace_b = (det_root / "run-b" / "trace.jsonl").read_bytes()
        report_a = (det_root / "run-a" / "demo_summary.json").read_bytes()
        report_b = (det_root / "run-b" / "demo_summary.json").read_bytes()
        verification = {
            "trace_byte_identical": trace_a == trace_b,
            "summary_byte_identical": report_a == report_b,
            "trace_sha256_run_a": hashlib.sha256(trace_a).hexdigest(),
            "trace_sha256_run_b": hashlib.sha256(trace_b).hexdigest(),
            "run_a_summary": first,
            "run_b_summary": second,
        }
        if not verification["trace_byte_identical"] or not verification["summary_byte_identical"]:
            raise BizError(
                "E_STATE_INVALID",
                "determinism verification failed",
                verification,
            )
        atomic_write_json(work / "determinism_verification.json", verification)
        summary["determinism_verification"] = {
            "trace_byte_identical": True,
            "summary_byte_identical": True,
            "trace_sha256": verification["trace_sha256_run_a"],
        }
    return summary
