#!/usr/bin/env python3
"""Deterministic end-to-end demonstration for the local shard gateway.

The script only uses local sample bytes, local subprocesses and local files.
It does not consult the clock or the network.
"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any

from shard_gateway.recovery import recover
from shard_gateway.scheduler import ShardTask, schedule_shards
from shard_gateway.storage import FAULT_EXIT_CODE, UploadGateway
from shard_gateway.verifier import verify


REPO = Path(__file__).resolve().parent
WORK = REPO / "demo-work"
ROOT = WORK / "gateway-data"
SAMPLE = WORK / "samples" / "sample.bin"
SAMPLE_SIZE = 32768 + 777
SHARD_SIZE = 4096
TOTAL_SHARDS = (SAMPLE_SIZE + SHARD_SIZE - 1) // SHARD_SIZE


def run_cli(*args: str) -> subprocess.CompletedProcess[str]:
    env = dict(os.environ)
    env["PYTHONHASHSEED"] = "0"
    env["PYTHONPATH"] = str(REPO)
    return subprocess.run(
        [sys.executable, "-m", "shard_gateway", *args],
        cwd=REPO,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )


def must_run(*args: str) -> dict[str, Any]:
    result = run_cli(*args)
    if result.returncode != 0:
        raise RuntimeError(
            f"command failed ({result.returncode}): {' '.join(args)}\n{result.stderr}"
        )
    return json.loads(result.stdout)


def log_json(value: Any) -> None:
    print(json.dumps(value, sort_keys=True, indent=2))


def upload(session_id: str, object_key: str) -> dict[str, Any]:
    return must_run(
        "upload",
        "--root",
        str(ROOT),
        "--session-id",
        session_id,
        "--object-key",
        object_key,
        "--sample",
        str(SAMPLE),
        "--shard-size",
        str(SHARD_SIZE),
        "--max-inflight",
        "3",
        "--space-budget",
        str(3 * SHARD_SIZE),
        "--session-quota",
        str(SAMPLE_SIZE),
    )


def create(session_id: str, object_key: str, sample_sha256: str) -> dict[str, Any]:
    return must_run(
        "create",
        "--root",
        str(ROOT),
        "--session-id",
        session_id,
        "--object-key",
        object_key,
        "--shard-size",
        str(SHARD_SIZE),
        "--total-shards",
        str(TOTAL_SHARDS),
        "--expected-size",
        str(SAMPLE_SIZE),
        "--expected-sha256",
        sample_sha256,
        "--session-quota",
        str(SAMPLE_SIZE),
    )


def put_shard(session_id: str, number: int, phase: str | None = None) -> subprocess.CompletedProcess[str]:
    shard_path = WORK / "samples" / f"shard-{number:03d}.bin"
    args = [
        "put-shard",
        "--root",
        str(ROOT),
        "--session-id",
        session_id,
        "--shard-number",
        str(number),
        "--data-file",
        str(shard_path),
    ]
    if phase is not None:
        args.extend(["--fault-phase", phase])
    return run_cli(*args)


def session_from_recovery(session_id: str) -> dict[str, Any]:
    report = recover(ROOT)
    assert report["ok"], json.dumps(report, sort_keys=True)
    for session in report["sessions"]:
        if session["session_id"] == session_id:
            return session
    raise AssertionError(f"session {session_id} missing from recovery report")


def object_report(object_key: str) -> dict[str, Any]:
    report = verify(ROOT)
    assert report["ok"], json.dumps(report, sort_keys=True)
    for item in report["objects"]:
        if item["object_key"] == object_key:
            return item
    raise AssertionError(f"object {object_key} missing from verification report")


def main() -> int:
    if WORK.exists():
        shutil.rmtree(WORK)
    (WORK / "samples").mkdir(parents=True)
    sample_info = must_run(
        "make-sample",
        "--path",
        str(SAMPLE),
        "--size",
        str(SAMPLE_SIZE),
        "--seed",
        "shard-gateway-demo-v1",
    )
    sample_sha256 = sample_info["sha256"]
    sample = SAMPLE.read_bytes()
    for number in range(1, TOTAL_SHARDS + 1):
        start = (number - 1) * SHARD_SIZE
        (WORK / "samples" / f"shard-{number:03d}.bin").write_bytes(
            sample[start : start + SHARD_SIZE]
        )

    normal = upload("normal-01", "demo/normal.bin")
    normal_object = object_report("demo/normal.bin")
    assert normal_object["object_sha256"] == sample_sha256

    budget = must_run(
        "upload",
        "--root",
        str(ROOT),
        "--session-id",
        "budget-01",
        "--object-key",
        "demo/budget.bin",
        "--sample",
        str(SAMPLE),
        "--shard-size",
        str(SHARD_SIZE),
        "--max-inflight",
        "8",
        "--space-budget",
        str(2 * SHARD_SIZE),
        "--session-quota",
        str(SAMPLE_SIZE),
    )
    assert budget["scheduler"]["inflight_peak"] == 2
    assert budget["scheduler"]["wait_count"] > 0
    budget_object = object_report("demo/budget.bin")
    assert budget_object["object_sha256"] == sample_sha256

    gateway = UploadGateway(ROOT)
    create("duplicate-01", "demo/duplicate.bin", sample_sha256)
    tasks = [
        ShardTask(number=number, data=sample[(number - 1) * SHARD_SIZE : number * SHARD_SIZE])
        for number in range(1, TOTAL_SHARDS + 1)
    ]
    first = schedule_shards(
        gateway,
        "duplicate-01",
        tasks,
        max_inflight=4,
        space_budget=4 * SHARD_SIZE,
    )
    version_before_retry = gateway.get_session("duplicate-01")["version"]
    before_retry_state = gateway.get_session("duplicate-01")
    old_path = (
        gateway.session_dir("duplicate-01")
        / before_retry_state["shards"]["3"]["file"]
    )
    gateway.write_shard(
        session_id="duplicate-01",
        shard_number=3,
        data=sample[2 * SHARD_SIZE : 3 * SHARD_SIZE],
    )
    assert not old_path.exists()
    retry = schedule_shards(
        gateway,
        "duplicate-01",
        tasks,
        max_inflight=4,
        space_budget=4 * SHARD_SIZE,
    )
    version_after_retry_writes = gateway.get_session("duplicate-01")["version"]
    duplicate_manifest = gateway.commit_session("duplicate-01")
    duplicate_state = gateway.get_session("duplicate-01")
    duplicate_object = object_report("demo/duplicate.bin")
    assert duplicate_object["object_sha256"] == sample_sha256
    assert duplicate_manifest["object_sha256"] == sample_sha256
    assert len(list((gateway.session_dir("duplicate-01") / "shards").glob("*.bin"))) == TOTAL_SHARDS

    fault_results: dict[str, Any] = {}
    for phase, session_id, object_key in (
        ("shard_landed", "fault-landed", "demo/fault-landed.bin"),
        ("metadata_updated", "fault-metadata", "demo/fault-metadata.bin"),
    ):
        create(session_id, object_key, sample_sha256)
        assert put_shard(session_id, 1).returncode == 0
        before = must_run("state", "--root", str(ROOT), "--session-id", session_id)
        crashed = put_shard(session_id, 2, phase)
        assert crashed.returncode == FAULT_EXIT_CODE
        recovered = session_from_recovery(session_id)
        expected_numbers = ["1"] if phase == "shard_landed" else ["1", "2"]
        digests_match = all(
            recovered["completed_digests"][number]
            == hashlib.sha256(
                sample[(int(number) - 1) * SHARD_SIZE : int(number) * SHARD_SIZE]
            ).hexdigest()
            for number in expected_numbers
        )
        assert recovered["version"] >= before["version"]
        assert digests_match
        resumed = upload(session_id, object_key)
        assert not resumed["already_committed"]
        assert object_report(object_key)["object_sha256"] == sample_sha256
        fault_results[phase] = {
            "process_exit_code": crashed.returncode,
            "version_before_fault": before["version"],
            "recovered_version": recovered["version"],
            "recovered_shards": recovered["recovered_shards"],
            "discarded_bytes": recovered["discarded_bytes"],
            "completed_digests_match": digests_match,
            "final_object_sha256": object_report(object_key)["object_sha256"],
        }

    manifest_key = "demo/fault-manifest.bin"
    crashed_manifest = run_cli(
        "upload",
        "--root",
        str(ROOT),
        "--session-id",
        "fault-manifest",
        "--object-key",
        manifest_key,
        "--sample",
        str(SAMPLE),
        "--shard-size",
        str(SHARD_SIZE),
        "--max-inflight",
        "3",
        "--space-budget",
        str(3 * SHARD_SIZE),
        "--session-quota",
        str(SAMPLE_SIZE),
        "--fault-phase",
        "manifest_published",
    )
    assert crashed_manifest.returncode == FAULT_EXIT_CODE
    visible_before_recovery = must_run(
        "manifest", "--root", str(ROOT), "--object-key", manifest_key
    )
    recovered_manifest = session_from_recovery("fault-manifest")
    assert recovered_manifest["status"] == "committed"
    assert recovered_manifest["version"] >= TOTAL_SHARDS + 1
    manifest_object = object_report(manifest_key)
    assert manifest_object["object_sha256"] == sample_sha256
    rerun = upload("fault-manifest", manifest_key)
    assert rerun["already_committed"]
    fault_results["manifest_published"] = {
        "process_exit_code": crashed_manifest.returncode,
        "version_before_fault": TOTAL_SHARDS + 1,
        "recovered_version": recovered_manifest["version"],
        "manifest_visible_before_recovery": visible_before_recovery["object_sha256"]
        == sample_sha256,
        "recovered_status": recovered_manifest["status"],
        "final_object_sha256": manifest_object["object_sha256"],
    }

    final_verify = verify(ROOT)
    assert final_verify["ok"], json.dumps(final_verify, sort_keys=True)
    all_match = all(
        item["object_sha256"] == sample_sha256 for item in final_verify["objects"]
    )
    assert all_match

    summary = {
        "sample": {
            "size": SAMPLE_SIZE,
            "sha256": sample_sha256,
            "shard_size": SHARD_SIZE,
            "total_shards": TOTAL_SHARDS,
        },
        "normal": {
            "scheduler": normal["scheduler"],
            "object_sha256": normal_object["object_sha256"],
            "manifest_sha256": normal_object["manifest_sha256"],
        },
        "budget_limited": {
            "scheduler": budget["scheduler"],
            "object_sha256": budget_object["object_sha256"],
        },
        "duplicate_retry": {
            "first_pass_completed_shards": first["completed_shards"],
            "version_before_retry": version_before_retry,
            "retry_completed_shards": retry["completed_shards"],
            "version_after_retry_writes": version_after_retry_writes,
            "version_after_commit": duplicate_state["version"],
            "shard_files_after_cleanup": len(
                list((gateway.session_dir("duplicate-01") / "shards").glob("*.bin"))
            ),
            "object_sha256": duplicate_object["object_sha256"],
        },
        "fault_restarts": fault_results,
        "verification": {
            "ok": final_verify["ok"],
            "objects": len(final_verify["objects"]),
            "all_object_digests_match_sample": all_match,
            "objects_report": [
                {
                    "object_key": item["object_key"],
                    "object_sha256": item["object_sha256"],
                    "manifest_sha256": item["manifest_sha256"],
                }
                for item in final_verify["objects"]
            ],
        },
    }
    log_json(summary)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
