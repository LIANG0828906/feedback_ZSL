from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path

from tileforge.errors import E_RECOVERY_INVALID, TileForgeError
from tileforge.pipeline import convert_incremental, offline_convert
from tileforge.samplegen import SAMPLE_SPECS, generate_sequence, sample_config
from tileforge.binary import write_sequence_atomic


class RecoveryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.sequence = generate_sequence(SAMPLE_SPECS[2], 75)
        self.input_path = self.root / "input.tcseq"
        write_sequence_atomic(self.input_path, self.sequence)
        self.config = sample_config(SAMPLE_SPECS[2])
        self.workspace = self.root / "workbench"

    def tearDown(self) -> None:
        self.temp.cleanup()

    def _crash(self, count: int = 5) -> None:
        config_path = self.root / "config.json"
        config_path.write_text(json.dumps(self.config.to_dict(), indent=2), encoding="utf-8")
        process = subprocess.run(
            [
                sys.executable,
                "-m",
                "tileforge",
                "convert",
                "--input",
                str(self.input_path),
                "--config",
                str(config_path),
                "--workspace",
                str(self.workspace),
                "--crash-after-commits",
                str(count),
            ],
            cwd=Path(__file__).resolve().parents[1],
            env=os.environ.copy(),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        self.assertEqual(process.returncode, 75, process.stderr)

    def test_forced_termination_resumes_without_rewriting_completed_tiles(self) -> None:
        self._crash(5)
        run_dirs = [path for path in (self.workspace / "runs").iterdir() if path.is_dir()]
        self.assertEqual(len(run_dirs), 1)
        commit_files = sorted((run_dirs[0] / "commits").glob("*.json"))
        self.assertEqual(len(commit_files), 5)
        before = {path.name: path.read_bytes() for path in commit_files}

        newly_committed: list[int] = []
        summary = convert_incremental(
            self.input_path,
            self.config,
            self.workspace,
            on_commit=lambda tile_id, chain: newly_committed.append(tile_id),
        )
        self.assertTrue(summary.version_hash)
        self.assertEqual(newly_committed, list(range(5, len(self.sequence.blocks))))
        self.assertEqual(len(before), 5)
        traces = sorted((self.workspace / "traces").glob("*.jsonl"))
        events = [json.loads(line) for path in traces for line in path.read_text(encoding="utf-8").splitlines()]
        recovery = [event for event in events if event["event"] == "recovery_scan"]
        self.assertTrue(any(event["recovered_count"] == 5 for event in recovery))
        scheduler_starts = [event for event in events if event["event"] == "scheduler_start"]
        self.assertTrue(any(event["tile_count"] == len(self.sequence.blocks) - 5 for event in scheduler_starts))

    def test_broken_hash_chain_aborts_recovery(self) -> None:
        self._crash(4)
        run_dir = next(path for path in (self.workspace / "runs").iterdir() if path.is_dir())
        second = sorted((run_dir / "commits").glob("*.json"))[1]
        record = json.loads(second.read_text(encoding="utf-8"))
        record["prev_chain"] = "0" * 64
        second.write_text(json.dumps(record), encoding="utf-8")
        with self.assertRaises(TileForgeError) as context:
            convert_incremental(self.input_path, self.config, self.workspace)
        self.assertEqual(context.exception.code, E_RECOVERY_INVALID)
        self.assertEqual(context.exception.details["field"], "prev_chain")
        self.assertFalse((self.workspace / "CURRENT.json").exists())

    def test_missing_recovery_field_aborts(self) -> None:
        self._crash(3)
        run_dir = next(path for path in (self.workspace / "runs").iterdir() if path.is_dir())
        first = sorted((run_dir / "commits").glob("*.json"))[0]
        record = json.loads(first.read_text(encoding="utf-8"))
        del record["tone_map_curve"]
        first.write_text(json.dumps(record), encoding="utf-8")
        with self.assertRaises(TileForgeError) as context:
            convert_incremental(self.input_path, self.config, self.workspace)
        self.assertEqual(context.exception.code, E_RECOVERY_INVALID)
        self.assertIn("tone_map_curve", context.exception.details["missing"])

    def test_missing_state_with_orphan_commits_aborts(self) -> None:
        self._crash(3)
        run_dir = next(path for path in (self.workspace / "runs").iterdir() if path.is_dir())
        (run_dir / "state.json").unlink()
        with self.assertRaises(TileForgeError) as context:
            convert_incremental(self.input_path, self.config, self.workspace)
        self.assertEqual(context.exception.code, E_RECOVERY_INVALID)
        self.assertEqual(context.exception.details["field"], "state.json")

    def test_recovery_uses_disk_base_when_another_sequence_published_meanwhile(self) -> None:
        self._crash(5)
        other_sequence = generate_sequence(SAMPLE_SPECS[0], 76)
        other_path = self.root / "other.tcseq"
        write_sequence_atomic(other_path, other_sequence)
        other = convert_incremental(other_path, sample_config(SAMPLE_SPECS[0]), self.workspace)
        self.assertEqual(other.version_id, 1)

        newly_committed: list[int] = []
        recovered = convert_incremental(
            self.input_path,
            self.config,
            self.workspace,
            on_commit=lambda tile_id, chain: newly_committed.append(tile_id),
        )
        self.assertEqual(newly_committed, list(range(5, len(self.sequence.blocks))))
        offline = offline_convert(
            self.input_path,
            self.root / "offline.tcseq",
            self.config,
        )
        self.assertEqual(recovered.version_hash, offline.version_hash)
        self.assertGreater(recovered.version_id or 0, other.version_id or 0)


if __name__ == "__main__":
    unittest.main()
