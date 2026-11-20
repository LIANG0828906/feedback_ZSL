from __future__ import annotations

import hashlib
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from shard_gateway.errors import (
    E_MANIFEST_INCOMPLETE,
    E_SESSION_ABORTED,
    E_SHARD_DIGEST_MISMATCH,
    E_SHARD_GAP,
    GatewayError,
)
from shard_gateway.recovery import recover
from shard_gateway.scheduler import ShardTask, schedule_shards
from shard_gateway.storage import FAULT_EXIT_CODE, UploadGateway
from shard_gateway.verifier import verify
from shard_gateway.util import safe_manifest_name


ROOT = Path(__file__).resolve().parents[1]


def sample_bytes(size: int) -> bytes:
    output = bytearray()
    counter = 0
    while len(output) < size:
        output.extend(hashlib.sha256(f"test-sample:{counter}".encode("ascii")).digest())
        counter += 1
    return bytes(output[:size])


def run_cli(*args: str) -> subprocess.CompletedProcess[str]:
    env = dict(os.environ)
    env["PYTHONHASHSEED"] = "0"
    env["PYTHONPATH"] = str(ROOT)
    return subprocess.run(
        [sys.executable, "-m", "shard_gateway", *args],
        cwd=ROOT,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )


class GatewayTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name) / "data"
        self.gateway = UploadGateway(self.root)
        self.data = sample_bytes(4 * 1024 + 123)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def create(self, session_id: str = "s1", *, quota: int | None = None) -> dict:
        return self.gateway.create_session(
            session_id=session_id,
            object_key=f"objects/{session_id}.bin",
            shard_size=1024,
            total_shards=5,
            expected_size=len(self.data),
            expected_sha256=hashlib.sha256(self.data).hexdigest(),
            session_quota=quota,
        )

    def write_all(self, session_id: str = "s1") -> None:
        for index in range(0, len(self.data), 1024):
            self.gateway.write_shard(
                session_id=session_id,
                shard_number=index // 1024 + 1,
                data=self.data[index : index + 1024],
            )

    def test_normal_commit_and_verify(self) -> None:
        created = self.create()
        self.assertEqual(created["version"], 1)
        self.write_all()
        manifest = self.gateway.commit_session("s1")
        self.assertEqual(manifest["total_shards"], 5)
        self.assertEqual(manifest["total_size"], len(self.data))
        report = verify(self.root)
        self.assertTrue(report["ok"], report)
        self.assertEqual(report["objects"][0]["object_sha256"], manifest["object_sha256"])
        self.assertEqual(report["objects"][0]["total_size"], len(self.data))

    def test_gap_rejected_without_manifest(self) -> None:
        self.create()
        self.gateway.write_shard(session_id="s1", shard_number=1, data=self.data[:1024])
        self.gateway.write_shard(session_id="s1", shard_number=3, data=self.data[2048:3072])
        with self.assertRaises(GatewayError) as caught:
            self.gateway.commit_session("s1")
        self.assertEqual(caught.exception.code, E_SHARD_GAP)
        self.assertIsNone(self.gateway.get_manifest_for_key("objects/s1.bin"))

    def test_digest_mismatch_rejected_without_manifest(self) -> None:
        self.create()
        self.write_all()
        state = self.gateway.get_session("s1")
        path = self.gateway.session_dir("s1") / state["shards"]["2"]["file"]
        content = bytearray(path.read_bytes())
        content[0] ^= 0xFF
        path.write_bytes(content)
        with self.assertRaises(GatewayError) as caught:
            self.gateway.commit_session("s1")
        self.assertEqual(caught.exception.code, E_SHARD_DIGEST_MISMATCH)
        self.assertIsNone(self.gateway.get_manifest_for_key("objects/s1.bin"))

    def test_expected_request_digest_rejected(self) -> None:
        self.create()
        with self.assertRaises(GatewayError) as caught:
            self.gateway.write_shard(
                session_id="s1",
                shard_number=1,
                data=self.data[:1024],
                expected_sha256="0" * 64,
            )
        self.assertEqual(caught.exception.code, E_SHARD_DIGEST_MISMATCH)
        state = self.gateway.get_session("s1")
        self.assertEqual(state["shards"], {})

    def test_duplicate_shard_uses_new_version_and_cleans_old_file(self) -> None:
        self.create()
        first = self.gateway.write_shard(
            session_id="s1", shard_number=1, data=self.data[:1024]
        )
        old_path = (
            self.gateway.session_dir("s1")
            / self.gateway.get_session("s1")["shards"]["1"]["file"]
        )
        second = self.gateway.write_shard(
            session_id="s1", shard_number=1, data=self.data[:1024]
        )
        state = self.gateway.get_session("s1")
        new_path = self.gateway.session_dir("s1") / state["shards"]["1"]["file"]
        self.assertEqual(first["version"] + 1, second["version"])
        self.assertEqual(state["version"], second["version"])
        self.assertFalse(old_path.exists())
        self.assertTrue(new_path.exists())

    def test_abort_blocks_write_and_commit(self) -> None:
        self.create()
        self.gateway.write_shard(session_id="s1", shard_number=1, data=self.data[:1024])
        aborted = self.gateway.abort_session("s1")
        self.assertEqual(aborted["status"], "aborted")
        with self.assertRaises(GatewayError) as write_error:
            self.gateway.write_shard(
                session_id="s1", shard_number=2, data=self.data[1024:2048]
            )
        self.assertEqual(write_error.exception.code, E_SESSION_ABORTED)
        with self.assertRaises(GatewayError) as commit_error:
            self.gateway.commit_session("s1")
        self.assertEqual(commit_error.exception.code, E_SESSION_ABORTED)
        self.assertIsNone(self.gateway.get_manifest_for_key("objects/s1.bin"))

    def test_scheduler_budget_waits_and_peak(self) -> None:
        self.gateway.create_session(
            session_id="budget",
            object_key="objects/budget.bin",
            shard_size=1024,
            total_shards=8,
            expected_size=8192,
        )
        data = sample_bytes(8192)
        tasks = [
            ShardTask(number=index // 1024 + 1, data=data[index : index + 1024])
            for index in range(0, len(data), 1024)
        ]
        result = schedule_shards(
            self.gateway,
            "budget",
            tasks,
            max_inflight=8,
            space_budget=2048,
        )
        self.assertEqual(result["completed_shards"], 8)
        self.assertEqual(result["inflight_peak"], 2)
        self.assertEqual(result["budget_bytes_peak"], 2048)
        self.assertGreaterEqual(result["wait_count"], 1)
        manifest = self.gateway.commit_session("budget")
        self.assertEqual(manifest["total_size"], 8192)

    def test_recovery_ignores_truncated_and_orphan_files(self) -> None:
        self.create()
        self.gateway.write_shard(
            session_id="s1", shard_number=1, data=self.data[:1024]
        )
        before = self.gateway.get_session("s1")
        session_dir = self.gateway.session_dir("s1")
        (session_dir / "tmp" / "shard-000002-truncated.part").write_bytes(b"x" * 111)
        (session_dir / "shards" / "shard-000003.v999.bin").write_bytes(b"y" * 47)
        report = recover(self.root)
        session = report["sessions"][0]
        self.assertEqual(session["version"], before["version"])
        self.assertEqual(session["recovered_shards"], 1)
        self.assertEqual(session["discarded_bytes"], 158)
        self.assertEqual(session["continuable_ranges"], [[2, 5]])
        self.assertEqual(
            session["completed_digests"]["1"], before["shards"]["1"]["digest"]
        )
        self.assertFalse((session_dir / "tmp" / "shard-000002-truncated.part").exists())
        self.assertFalse((session_dir / "shards" / "shard-000003.v999.bin").exists())

    def test_verifier_reports_digest_mismatch_to_stderr(self) -> None:
        self.create()
        self.write_all()
        self.gateway.commit_session("s1")
        state = self.gateway.get_session("s1")
        path = self.gateway.session_dir("s1") / state["shards"]["4"]["file"]
        content = bytearray(path.read_bytes())
        content[-1] ^= 0xFF
        path.write_bytes(content)
        result = run_cli("verify", "--root", str(self.root))
        self.assertEqual(result.returncode, 2)
        self.assertIn(E_SHARD_DIGEST_MISMATCH, result.stderr)
        report = json.loads(result.stdout)
        self.assertFalse(report["ok"])

    def test_verifier_reports_incomplete_manifest(self) -> None:
        self.create()
        self.write_all()
        self.gateway.commit_session("s1")
        manifest = self.gateway.get_manifest_for_key("objects/s1.bin")
        manifest["shards"] = manifest["shards"][:-1]
        path = (
            self.gateway.manifests_dir
            / safe_manifest_name("objects/s1.bin")
        )
        path.write_text(json.dumps(manifest), encoding="utf-8")
        result = run_cli("verify", "--root", str(self.root))
        self.assertEqual(result.returncode, 2)
        self.assertIn(E_MANIFEST_INCOMPLETE, result.stderr)


class FaultRecoveryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.base = Path(self.temp.name)
        self.root = self.base / "data"
        self.datadir = self.base / "shards"
        self.datadir.mkdir()
        self.data = sample_bytes(3072)
        self.shards = [
            self.data[index : index + 1024] for index in range(0, len(self.data), 1024)
        ]
        for index, shard in enumerate(self.shards, 1):
            (self.datadir / f"{index}.bin").write_bytes(shard)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def create(self, session_id: str) -> None:
        result = run_cli(
            "create",
            "--root",
            str(self.root),
            "--session-id",
            session_id,
            "--object-key",
            f"fault/{session_id}.bin",
            "--shard-size",
            "1024",
            "--total-shards",
            "3",
            "--expected-size",
            str(len(self.data)),
            "--expected-sha256",
            hashlib.sha256(self.data).hexdigest(),
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def put(self, session_id: str, number: int, fault: str | None = None) -> subprocess.CompletedProcess[str]:
        args = [
            "put-shard",
            "--root",
            str(self.root),
            "--session-id",
            session_id,
            "--shard-number",
            str(number),
            "--data-file",
            str(self.datadir / f"{number}.bin"),
        ]
        if fault:
            args.extend(["--fault-phase", fault])
        return run_cli(*args)

    def test_shard_landed_fault_recovery(self) -> None:
        self.create("landed")
        self.assertEqual(self.put("landed", 1).returncode, 0)
        before = json.loads(run_cli("state", "--root", str(self.root), "--session-id", "landed").stdout)
        crashed = self.put("landed", 2, "shard_landed")
        self.assertEqual(crashed.returncode, FAULT_EXIT_CODE)
        report = recover(self.root)
        session = report["sessions"][0]
        self.assertGreaterEqual(session["version"], before["version"])
        self.assertEqual(session["recovered_shards"], 1)
        self.assertEqual(
            session["completed_digests"]["1"], before["shards"]["1"]["digest"]
        )
        self.assertEqual(session["continuable_ranges"], [[2, 3]])
        self.assertGreater(session["discarded_bytes"], 0)
        for number in (1, 2, 3):
            self.assertEqual(self.put("landed", number).returncode, 0)
        manifest = UploadGateway(self.root).commit_session("landed")
        self.assertEqual(manifest["total_size"], len(self.data))

    def test_metadata_updated_fault_recovery(self) -> None:
        self.create("metadata")
        self.assertEqual(self.put("metadata", 1).returncode, 0)
        before = json.loads(
            run_cli("state", "--root", str(self.root), "--session-id", "metadata").stdout
        )
        crashed = self.put("metadata", 2, "metadata_updated")
        self.assertEqual(crashed.returncode, FAULT_EXIT_CODE)
        report = recover(self.root)
        session = report["sessions"][0]
        self.assertGreaterEqual(session["version"], before["version"] + 1)
        self.assertEqual(session["recovered_shards"], 2)
        for number in (1, 2):
            self.assertEqual(
                session["completed_digests"][str(number)],
                hashlib.sha256(self.shards[number - 1]).hexdigest(),
            )
        self.assertEqual(session["continuable_ranges"], [[3, 3]])

    def test_manifest_published_fault_recovery(self) -> None:
        sample = self.base / "sample.bin"
        sample.write_bytes(self.data)
        crashed = run_cli(
            "upload",
            "--root",
            str(self.root),
            "--session-id",
            "published",
            "--object-key",
            "fault/published.bin",
            "--sample",
            str(sample),
            "--shard-size",
            "1024",
            "--max-inflight",
            "2",
            "--space-budget",
            "2048",
            "--fault-phase",
            "manifest_published",
        )
        self.assertEqual(crashed.returncode, FAULT_EXIT_CODE)
        manifest_visible = run_cli(
            "manifest",
            "--root",
            str(self.root),
            "--object-key",
            "fault/published.bin",
        )
        self.assertEqual(manifest_visible.returncode, 0, manifest_visible.stderr)
        report = recover(self.root)
        session = report["sessions"][0]
        self.assertEqual(session["status"], "committed")
        self.assertGreaterEqual(session["version"], 4)
        self.assertTrue(verify(self.root)["ok"])
        rerun = run_cli(
            "upload",
            "--root",
            str(self.root),
            "--session-id",
            "published",
            "--object-key",
            "fault/published.bin",
            "--sample",
            str(sample),
            "--shard-size",
            "1024",
            "--max-inflight",
            "2",
            "--space-budget",
            "2048",
        )
        self.assertEqual(rerun.returncode, 0, rerun.stderr)
        self.assertTrue(json.loads(rerun.stdout)["already_committed"])


if __name__ == "__main__":
    unittest.main()
