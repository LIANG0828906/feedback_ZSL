from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from surface_viewer.errors import ErrorCode, SurfaceViewerError
from surface_viewer.model import parse_assembly
from surface_viewer.runtime import SurfaceRuntime
from surface_viewer.samplegen import (
    CASE_SPECS,
    generate_case,
    operation_changes,
    operations_for_case,
    write_cases,
)

ROOT = Path(__file__).resolve().parents[1]


class RecoveryTests(unittest.TestCase):
    def test_atomic_commit_can_be_recovered(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            assembly = parse_assembly(generate_case(CASE_SPECS[1]))
            runtime = SurfaceRuntime.bootstrap(
                assembly, root / "store", verify_full=False
            )
            operation = operations_for_case(CASE_SPECS[1])[0]
            patch_id, changes = operation_changes(runtime.engine.assembly, operation)
            update = runtime.update(patch_id, changes, verify_full=False)
            recovered = runtime.store.recover(
                expected_assembly_id=assembly.assembly_id
            )
            self.assertEqual(recovered.commit_seq, update.commit.commit_seq)
            self.assertEqual(recovered.chain_head, update.commit.chain_head)
            self.assertEqual(
                set(recovered.artifacts), set(runtime.engine.cache)
            )
            self.assertEqual(
                set(recovered.manifest["patch_versions"]),
                {"p0000", "p0001", "p0002", "p0003"},
            )
            self.assertEqual(
                set(
                    recovered.manifest["parameter_versions"]["p0001"]
                ),
                {
                    "control_points",
                    "surface_type",
                    "precision_level",
                    "flip_normal",
                },
            )

    def test_broken_hash_chain_returns_recovery_invalid(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            assembly = parse_assembly(generate_case(CASE_SPECS[0]))
            runtime = SurfaceRuntime.bootstrap(
                assembly, root / "store", verify_full=False
            )
            manifest_path = runtime.store.manifest_path
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            manifest["artifacts"][0]["content_hash"] = "f" * 64
            manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
            with self.assertRaises(SurfaceViewerError) as raised:
                runtime.store.recover(
                    expected_assembly_id=assembly.assembly_id
                )
            self.assertEqual(
                raised.exception.code, ErrorCode.RECOVERY_INVALID
            )

    def test_unsupported_manifest_version_returns_recovery_invalid(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            assembly = parse_assembly(generate_case(CASE_SPECS[0]))
            runtime = SurfaceRuntime.bootstrap(
                assembly, root / "store", verify_full=False
            )
            manifest = json.loads(
                runtime.store.manifest_path.read_text(encoding="utf-8")
            )
            manifest["manifest_version"] = "future-version"
            runtime.store.manifest_path.write_text(
                json.dumps(manifest), encoding="utf-8"
            )
            with self.assertRaises(SurfaceViewerError) as raised:
                runtime.store.recover(expected_assembly_id=assembly.assembly_id)
            self.assertEqual(
                raised.exception.code, ErrorCode.RECOVERY_INVALID
            )
            self.assertEqual(raised.exception.field, "manifest_version")

    def test_forced_termination_leaves_old_manifest_and_recovers_changed_products(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            workspace = Path(temporary)
            samples = workspace / "samples"
            write_cases(samples)
            case_dir = samples / "tiles_2x2"
            store = workspace / "store"
            environment = dict(os.environ)
            environment["PYTHONPATH"] = str(ROOT)
            process = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "surface_viewer.crash_worker",
                    "--assembly",
                    str(case_dir / "assembly.json"),
                    "--store",
                    str(store),
                    "--operations",
                    str(case_dir / "operations.json"),
                    "--operation-index",
                    "0",
                    "--crash-after-artifacts",
                    "1",
                ],
                cwd=ROOT,
                env=environment,
                capture_output=True,
                text=True,
                timeout=30,
            )
            self.assertEqual(process.returncode, 23, process.stderr)

            assembly = parse_assembly(
                json.loads(
                    (case_dir / "assembly.json").read_text(encoding="utf-8")
                )
            )
            operation = json.loads(
                (case_dir / "operations.json").read_text(encoding="utf-8")
            )["operations"][0]
            patch_id, changes = operation_changes(assembly, operation)
            modified = assembly.apply_changes(patch_id, changes)

            runtime = SurfaceRuntime.bootstrap(
                modified, store, verify_full=True
            )
            self.assertEqual(
                runtime.bootstrap_result.recovery.commit_seq, 1
            )
            self.assertEqual(
                runtime.bootstrap_result.update.commit.commit_seq, 2
            )
            self.assertEqual(
                runtime.bootstrap_result.recomputed_after_recovery, 3
            )
            self.assertEqual(runtime.bootstrap_result.reused_from_disk, 13)


if __name__ == "__main__":
    unittest.main()
