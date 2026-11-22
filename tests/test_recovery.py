from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import threading
import time
import unittest
from pathlib import Path

from live_transcode.engine import TranscodeEngine
from live_transcode.errors import E_RECOVERY_INVALID, TaskError
from live_transcode.models import ResampleRatio, TierParams

from .support import ROOT, Workspace, file_sha, json_bytes


class RecoveryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.ctx = Workspace("lt-recovery-")
        self.ctx.__enter__()
        self.fixtures = self.ctx.fixtures(seed=93, duration=1.4)

    def tearDown(self) -> None:
        self.ctx.__exit__(None, None, None)

    def _cancel_after(self, engine: TranscodeEngine, wav: str, tier: TierParams, count: int):
        cancel = threading.Event()
        seen = {"count": 0}

        def progress(event: dict) -> None:
            if event.get("event") == "slice":
                seen["count"] += 1
                if seen["count"] == count:
                    cancel.set()

        result = engine.generate_tier(wav, tier, cancel_event=cancel, progress=progress)
        self.assertTrue(result.cancelled)
        self.assertEqual(seen["count"], count)
        return result

    def test_cooperative_cancel_resume_preserves_committed_slices(self) -> None:
        engine = TranscodeEngine(self.ctx.store)
        wav = self.fixtures[3]["path"]
        tier = TierParams("resume", 22050, 40, 24, 1, 256)
        cancelled = self._cancel_after(engine, wav, tier, 7)
        committed = engine.store.list_committed_indices(cancelled.run_dir)
        self.assertEqual(committed, list(range(7)))
        before = {
            index: (
                (cancelled.run_dir / "slices" / f"{index:06d}" / "audio.pcm").stat().st_ino,
                file_sha(
                    cancelled.run_dir / "slices" / f"{index:06d}" / "audio.pcm"
                ),
                file_sha(
                    cancelled.run_dir / "slices" / f"{index:06d}" / "meta.json"
                ),
            )
            for index in committed
        }

        resumed = engine.generate_tier(wav, tier)
        self.assertFalse(resumed.cancelled)
        self.assertTrue(resumed.resumed)
        self.assertEqual(resumed.resume_index, 7)
        self.assertIsNotNone(resumed.sidecar)
        for index in committed:
            after = (
                (resumed.run_dir / "slices" / f"{index:06d}" / "audio.pcm").stat().st_ino,
                file_sha(resumed.run_dir / "slices" / f"{index:06d}" / "audio.pcm"),
                file_sha(resumed.run_dir / "slices" / f"{index:06d}" / "meta.json"),
            )
            self.assertEqual(before[index], after)

        slices = resumed.sidecar["slices"]
        ratio = ResampleRatio.build(48000, tier.target_rate)
        expected_handoff = (
            slices[6]["end_phase_num"]
            + (tier.filter_length // 2) * ratio.step_num
        ) % ratio.step_den
        self.assertEqual(slices[7]["phase_num"], slices[6]["end_phase_num"])
        self.assertEqual(slices[7]["phase_remainder_num"], expected_handoff)
        self.assertGreater(
            slices[7]["src_start"],
            slices[6]["src_end"] - (tier.filter_length + 2),
        )
        report = engine.audit_published_tier(wav, tier.tier_id)
        self.assertTrue(report["ok"])
        self.assertEqual(report["first_drift_slice"], None)

    def test_chain_tamper_aborts_resume_with_slice(self) -> None:
        engine = TranscodeEngine(self.ctx.store)
        wav = self.fixtures[0]["path"]
        tier = TierParams("chain", 24000, 32, 16, 1, 256)
        cancelled = self._cancel_after(engine, wav, tier, 5)
        meta_path = cancelled.run_dir / "slices" / "000002" / "meta.json"
        meta = json.loads(meta_path.read_text(encoding="utf-8"))
        meta["chain_hash"] = "0" * 64
        meta_path.write_bytes(json_bytes(meta))
        with self.assertRaises(TaskError) as raised:
            engine.generate_tier(wav, tier)
        self.assertEqual(raised.exception.code, E_RECOVERY_INVALID)
        self.assertEqual(raised.exception.details["slice_index"], 2)
        self.assertIn("chain_hash", raised.exception.details["problems"])

    def test_snapshot_version_tamper_aborts_resume(self) -> None:
        engine = TranscodeEngine(self.ctx.store)
        wav = self.fixtures[0]["path"]
        tier = TierParams("snapshot", 24000, 32, 16, 1, 256)
        cancelled = self._cancel_after(engine, wav, tier, 4)
        snapshot_path = cancelled.run_dir / "snapshot.json"
        snapshot = json.loads(snapshot_path.read_text(encoding="utf-8"))
        snapshot["snapshot_version"] = 999
        snapshot_path.write_bytes(json_bytes(snapshot))
        with self.assertRaises(TaskError) as raised:
            engine.generate_tier(wav, tier)
        self.assertEqual(raised.exception.code, E_RECOVERY_INVALID)
        self.assertIn("snapshot version", raised.exception.message)

    def test_external_sigkill_then_resume(self) -> None:
        wav = self.fixtures[3]["path"]
        command = [
            sys.executable,
            str(ROOT / "run.py"),
            "transcode",
            "--wav",
            wav,
            "--store",
            str(self.ctx.store),
            "--tier",
            "kill",
            "--rate",
            "44100",
            "--filter",
            "128",
            "--quant",
            "24",
            "--slice-samples",
            "512",
        ]
        process = subprocess.Popen(
            command,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            cwd=str(ROOT),
        )
        trace_path = self.ctx.store / "trace.jsonl"
        deadline = time.time() + 30
        killed = False
        while time.time() < deadline:
            if process.poll() is not None:
                break
            if trace_path.exists():
                records = [
                    json.loads(line)
                    for line in trace_path.read_text(encoding="utf-8").splitlines()
                    if line.strip()
                ]
                committed = sum(
                    record.get("action") == "slice_recomputed"
                    for record in records
                )
                if committed >= 5:
                    os.kill(process.pid, signal.SIGKILL)
                    killed = True
                    break
            time.sleep(0.01)
        process.wait(timeout=30)
        if process.stdout is not None:
            process.stdout.close()
        if process.stderr is not None:
            process.stderr.close()
        self.assertTrue(killed, "process finished before it could be killed")
        self.assertEqual(process.returncode, -signal.SIGKILL)
        snapshots = list(self.ctx.store.glob("runs/kill/*/snapshot.json"))
        self.assertEqual(len(snapshots), 1)
        run_dir = snapshots[0].parent
        self.assertFalse((run_dir / ".complete").exists())
        committed_before = TranscodeEngine(
            self.ctx.store
        ).store.list_committed_indices(run_dir)
        self.assertGreaterEqual(len(committed_before), 5)
        inode_before = {
            index: (run_dir / "slices" / f"{index:06d}" / "audio.pcm").stat().st_ino
            for index in committed_before
        }

        resumed = subprocess.run(
            command,
            cwd=str(ROOT),
            capture_output=True,
            text=True,
            timeout=60,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        payload = json.loads(resumed.stdout)
        self.assertTrue(payload["resumed"])
        self.assertGreaterEqual(payload["resume_index"], len(committed_before))
        self.assertTrue(payload["tier_version_hash"])
        for index in committed_before:
            self.assertEqual(
                inode_before[index],
                (run_dir / "slices" / f"{index:06d}" / "audio.pcm").stat().st_ino,
            )

        audited = subprocess.run(
            [
                sys.executable,
                str(ROOT / "run.py"),
                "audit",
                "--wav",
                wav,
                "--store",
                str(self.ctx.store),
                "--tier",
                "kill",
            ],
            cwd=str(ROOT),
            capture_output=True,
            text=True,
            timeout=60,
        )
        self.assertEqual(audited.returncode, 0, audited.stderr)
        self.assertTrue(json.loads(audited.stdout)["report"]["ok"])


if __name__ == "__main__":
    unittest.main()

