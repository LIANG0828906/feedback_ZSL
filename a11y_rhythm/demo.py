"""Deterministic acceptance demo producing local samples and evidence."""

from __future__ import annotations

import json
import shutil
from pathlib import Path
from typing import Any

from .samples import generate_all
from .store import DeliverySession, atomic_write_json
from .timebase import load_source_bundle


class SimulatedKill(BaseException):
    pass


def _load(path: Path):
    bundle, wav_info, frame_table = load_source_bundle(path)
    coverage = min(wav_info.sample_count, frame_table.duration_samples)
    return bundle, frame_table, coverage


def run_demo(output_dir: str | Path = "artifacts/demo") -> dict[str, Any]:
    output = Path(output_dir)
    if output.exists():
        shutil.rmtree(output)
    output.mkdir(parents=True)
    samples_dir = output / "samples"
    manifest = generate_all(samples_dir)

    # 1. Conflict-chain solve on the dense mono fixture.
    conflict_bundle, conflict_frames, conflict_coverage = _load(
        samples_dir / "mono_conflict.json"
    )
    conflict_store = output / "conflict.session.json"
    conflict_session = DeliverySession.open(
        conflict_bundle, conflict_frames, conflict_coverage, conflict_store
    )
    conflict_snapshot = conflict_session.initialize()
    conflict_result = conflict_snapshot.result
    conflict_ids = sorted(
        {
            item_id
            for record in conflict_result.conflicts
            for item_id in (record.left_id, record.right_id)
        }
    )
    chain_length = max((len(record.chain) for record in conflict_result.conflicts), default=0)
    conflict_evidence = {
        "version": conflict_snapshot.version,
        "snapshot_chain_hash": conflict_snapshot.chain_hash,
        "result_hash": conflict_result.result_hash,
        "resolutions": len(conflict_result.conflicts),
        "conflict_chain_length": chain_length,
        "conflict_sentence_ids": conflict_ids,
        "all_active_sentences_readable": all(
            item.readable for item in conflict_result.items if item.status == "active"
        ),
        "compliance": {
            "compliant": conflict_snapshot.payload["compliance"]["compliant"],
            "violations": conflict_snapshot.payload["compliance"]["violations"],
        },
        "sample_conflict_evidence": [
            record.to_dict() for record in conflict_result.conflicts[:8]
        ],
    }

    # 2. Local incremental edit on the sparse fixture.
    sparse_bundle, sparse_frames, sparse_coverage = _load(samples_dir / "mono_sparse.json")
    sparse_store = output / "incremental.session.json"
    sparse_session = DeliverySession.open(
        sparse_bundle, sparse_frames, sparse_coverage, sparse_store
    )
    sparse_v1 = sparse_session.initialize()
    sparse_text = (
        "A longer description competes for this local window and shifts only "
        "its downstream speech."
    )
    sparse_v2 = sparse_session.update_item("ds001", {"text": sparse_text})
    sparse_r1 = sparse_v1.result.by_id()
    sparse_r2 = sparse_v2.result
    unchanged = {
        item_id: sparse_r1[item_id].result_hash == sparse_v2.result.by_id()[item_id].result_hash
        for item_id in sparse_r2.reused_ids
    }
    sparse_v3 = sparse_session.update_item(
        "ds001",
        {"text": "Second local signal edit."},
    )
    sparse_disk = sparse_session.store.load()
    version_chain_evidence = {
        "versions": [sparse_v1.version, sparse_v2.version, sparse_v3.version],
        "history_versions": [entry["version"] for entry in sparse_v3.history],
        "in_memory_version": sparse_session.snapshot.version,
        "disk_version": sparse_disk.version if sparse_disk else None,
        "memory_chain_hash": sparse_session.snapshot.chain_hash,
        "disk_chain_hash": sparse_disk.chain_hash if sparse_disk else None,
        "consistent": bool(
            sparse_disk
            and sparse_session.snapshot.version == sparse_disk.version
            and sparse_session.snapshot.chain_hash == sparse_disk.chain_hash
        ),
        "result_hash": sparse_v3.result.result_hash,
    }
    incremental_evidence = {
        "base_version": sparse_v1.version,
        "new_version": sparse_v2.version,
        "changed_id": "ds001",
        "affected_ids": sparse_r2.affected_ids,
        "affected_count": len(sparse_r2.affected_ids),
        "reused_ids": sparse_r2.reused_ids,
        "reused_count": len(sparse_r2.reused_ids),
        "reused_hashes_unchanged": all(unchanged.values()),
        "reuse_checks": unchanged,
        "conservative_reasons": sparse_r2.conservative_reasons,
        "result_hash": sparse_r2.result_hash,
        "snapshot_chain_hash": sparse_v2.chain_hash,
        "version_chain": version_chain_evidence,
    }

    # 3. Force a stop after two persisted checkpoints, then resume.
    recovery_patch = {
        "text": (
            "Recovered description spans the saved checkpoint."
        )
    }
    checkpoint_count = {"value": 0}

    def crash_after_two(checkpoint: dict[str, Any]) -> None:
        checkpoint_count["value"] += 1
        if checkpoint_count["value"] >= 2:
            raise SimulatedKill("simulated process termination after checkpoint")

    interrupted = False
    try:
        conflict_session.update_item("ds001", recovery_patch, progress=crash_after_two)
    except SimulatedKill:
        interrupted = True
    journal_after_interrupt = conflict_session.store.journal_path.exists()
    if not interrupted or not journal_after_interrupt:
        raise RuntimeError("forced termination did not leave a recoverable journal")

    reopened = DeliverySession.open(
        conflict_bundle, conflict_frames, conflict_coverage, conflict_store
    )
    recovered_status = reopened.recover()
    recovered_snapshot = reopened.snapshot
    if recovered_snapshot is None:
        raise RuntimeError("recovery did not publish a snapshot")

    # A clean run with the same base and patch proves recovery is not merely
    # "a result", but the exact deterministic result.
    clean_store = output / "recovery-control.session.json"
    clean_session = DeliverySession.open(
        conflict_bundle, conflict_frames, conflict_coverage, clean_store
    )
    clean_session.initialize()
    clean_snapshot = clean_session.update_item("ds001", recovery_patch)
    recovery_evidence = {
        "interrupted": interrupted,
        "journal_present_after_interrupt": journal_after_interrupt,
        "checkpoints_written_before_stop": checkpoint_count["value"],
        "recovery_status": recovered_status.to_dict(),
        "recovered_version": recovered_snapshot.version,
        "recovered_chain_hash": recovered_snapshot.chain_hash,
        "clean_control_version": clean_snapshot.version,
        "clean_control_chain_hash": clean_snapshot.chain_hash,
        "byte_equivalent_chain": recovered_snapshot.chain_hash == clean_snapshot.chain_hash,
        "result_hash_equal": (
            recovered_snapshot.result.result_hash == clean_snapshot.result.result_hash
        ),
        "history_versions": [entry["version"] for entry in recovered_snapshot.history],
        "journal_removed_after_commit": not reopened.store.journal_path.exists(),
        "delivery_artifacts": {
            "snapshot": str(reopened.store.path),
            "speech": str(reopened.store.speech_track_path),
            "description": str(reopened.store.description_track_path),
            "compliance": str(reopened.store.compliance_path),
            "trace": str(reopened.store.trace_path),
        },
    }

    # 4. Determinism evidence: generate one fixture a second time and compare
    # all bytes, without relying on timestamps.
    from .samples import SCENARIOS, generate_scenario

    repeat_dir = output / "samples-repeat"
    repeat = generate_scenario(repeat_dir, SCENARIOS[1])
    original = next(item for item in manifest["scenarios"] if item["name"] == SCENARIOS[1].name)
    determinism_evidence = {
        "scenario": SCENARIOS[1].name,
        "source_hash_equal": repeat["source_hash"] == original["source_hash"],
        "file_hashes_equal": repeat["files"] == original["files"],
        "files": repeat["files"],
    }
    same_seed_different_dir = all(
        (samples_dir / name).read_bytes() == (repeat_dir / name).read_bytes()
        for name in repeat["files"]
    )
    determinism_evidence["byte_identical_files"] = same_seed_different_dir

    trace_events = recovered_snapshot.trace_log
    verification = {
        "schema_version": 1,
        "deterministic": True,
        "sample_manifest": {
            "scenarios": [
                {
                    "name": item["name"],
                    "speech_count": item["speech_count"],
                    "description_count": item["description_count"],
                    "channels": item["channels"],
                    "max_cps": item["max_cps"],
                    "contention": item["contention"],
                    "source_hash": item["source_hash"],
                }
                for item in manifest["scenarios"]
            ]
        },
        "conflict_chain": conflict_evidence,
        "incremental": incremental_evidence,
        "recovery": recovery_evidence,
        "determinism": determinism_evidence,
        "trace": {
            "path": str(reopened.store.trace_path),
            "event_count": len(trace_events),
            "first_event": trace_events[0] if trace_events else None,
            "last_event": trace_events[-1] if trace_events else None,
        },
    }
    atomic_write_json(output / "verification.json", verification)
    atomic_write_json(output / "trace.json", trace_events)

    # Deliver the recovered snapshot's artifact names in a stable aggregate.
    delivery = {
        "version": recovered_snapshot.version,
        "chain_hash": recovered_snapshot.chain_hash,
        "result_hash": recovered_snapshot.result.result_hash,
        "report_hash": recovered_snapshot.payload["compliance"]["report_hash"],
        "speech_track": recovered_snapshot.payload["tracks"]["speech"],
        "description_track": recovered_snapshot.payload["tracks"]["description"],
        "compliance_report": recovered_snapshot.payload["compliance"],
        "speakers": recovered_snapshot.payload["speakers"],
    }
    atomic_write_json(output / "delivery.json", delivery)
    return verification

