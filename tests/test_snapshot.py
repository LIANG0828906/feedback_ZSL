from __future__ import annotations

import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from loudness_diag.errors import E_RECOVERY_INVALID, DiagnosticError
from loudness_diag.jsonutil import atomic_write_json, load_json
from loudness_diag.snapshot import SnapshotStore

from .helpers import make_block_row


class SnapshotTests(unittest.TestCase):
    def _store(self, root: str, *, total: int = 5) -> SnapshotStore:
        return SnapshotStore(
            root,
            asset_id="snapshot-asset",
            parameters={"block_samples": 9600, "oversample": 4},
            source_content_hash="source-hash",
            parameter_version="v1",
            total_blocks=total,
        )

    def _gate(self, rows: list[dict]) -> dict:
        return {
            "absolute_threshold_energy": 1e-6,
            "relative_threshold_energy": 1e-4,
            "integrated_loudness": -12.0,
            "gated_block_ids": ["00000000", "00000001"],
            "removed_block_ids": [
                row["block_id"] for row in rows[2:]
            ],
            "blocks": rows,
        }

    def test_resume_only_computes_and_writes_uncommitted_blocks(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            store = self._store(directory)
            self.assertTrue(store.commit_block(make_block_row(0)))
            self.assertTrue(store.commit_block(make_block_row(1)))
            committed_paths = [
                store.blocks_directory / "00000000.json",
                store.blocks_directory / "00000001.json",
            ]
            before = [path.read_bytes() for path in committed_paths]

            reopened = self._store(directory)
            self.assertEqual(reopened.committed_count, 2)
            computed: list[int] = []

            def compute(index: int) -> dict:
                computed.append(index)
                return make_block_row(index)

            rows, gate, computed_indices = reopened.resume(
                total_blocks=5,
                compute_block=compute,
                build_gate=self._gate,
            )
            self.assertEqual(computed, [2, 3, 4])
            self.assertEqual(computed_indices, [2, 3, 4])
            self.assertEqual(len(rows), 5)
            self.assertIsNotNone(gate)
            self.assertEqual(
                [path.read_bytes() for path in committed_paths],
                before,
            )
            self.assertFalse(reopened.commit_block(make_block_row(0)))

            final = self._store(directory)
            self.assertEqual(final.committed_count, 5)
            self.assertIsNotNone(final.gate_result)
            _, _, no_recompute = final.resume(
                total_blocks=5,
                compute_block=lambda index: self.fail("must not recompute"),
                build_gate=self._gate,
            )
            self.assertEqual(no_recompute, [])

    def test_missing_field_reports_first_broken_block(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            store = self._store(directory)
            for index in range(3):
                store.commit_block(make_block_row(index))
            broken = make_block_row(1)
            broken.pop("true_peak")
            atomic_write_json(store.blocks_directory / "00000001.json", broken)
            with self.assertRaises(DiagnosticError) as context:
                self._store(directory)
            self.assertEqual(context.exception.code, E_RECOVERY_INVALID)
            self.assertEqual(context.exception.first_broken_block, 1)

    def test_broken_hash_chain_reports_first_broken_block(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            store = self._store(directory)
            for index in range(3):
                store.commit_block(make_block_row(index))
            path = store.blocks_directory / "00000001.json"
            value = load_json(path)
            value["prev_hash"] = "f" * 64
            atomic_write_json(path, value)
            with self.assertRaises(DiagnosticError) as context:
                self._store(directory)
            self.assertEqual(context.exception.code, E_RECOVERY_INVALID)
            self.assertEqual(context.exception.first_broken_block, 1)

    def test_sigkill_then_resume_from_disk(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            script = r"""
import sys, time
from loudness_diag.snapshot import SnapshotStore
from tests.helpers import make_block_row
root = sys.argv[1]
store = SnapshotStore(
    root,
    asset_id="killed-asset",
    parameters={"block_samples": 9600, "oversample": 4},
    source_content_hash="kill-source",
    parameter_version="v1",
    total_blocks=5,
)
for index in range(5):
    store.commit_block(make_block_row(index))
    print("COMMITTED", index, flush=True)
    time.sleep(0.5)
"""
            process = subprocess.Popen(
                [sys.executable, "-c", script, directory],
                cwd=Path(__file__).resolve().parents[1],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            self.assertIsNotNone(process.stdout)
            first_line = process.stdout.readline()
            self.assertIn("COMMITTED", first_line)
            process.kill()
            _, stderr = process.communicate(timeout=5)
            self.assertEqual(process.returncode, -9, stderr)

            reopened = SnapshotStore(
                directory,
                asset_id="killed-asset",
                parameters={"block_samples": 9600, "oversample": 4},
                source_content_hash="kill-source",
                parameter_version="v1",
                total_blocks=5,
            )
            self.assertGreaterEqual(reopened.committed_count, 1)
            committed_before = reopened.committed_count
            existing_paths = [
                reopened.blocks_directory / f"{index:08d}.json"
                for index in range(committed_before)
            ]
            existing_bytes = [path.read_bytes() for path in existing_paths]
            computed: list[int] = []

            def compute(index: int) -> dict:
                computed.append(index)
                return make_block_row(index)

            reopened.resume(
                total_blocks=5,
                compute_block=compute,
                build_gate=self._gate,
            )
            self.assertEqual(
                computed,
                list(range(committed_before, 5)),
            )
            self.assertEqual(
                [path.read_bytes() for path in existing_paths],
                existing_bytes,
            )


if __name__ == "__main__":
    unittest.main()

