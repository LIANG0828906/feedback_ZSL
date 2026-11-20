from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from mesh_editor.editing import Selection, simplify_edge_collapse, transform_selection
from mesh_editor.errors import (
    CommitError,
    E_COMMIT_FAILED,
    E_RECOVERY_INVALID,
    E_VERSION_STALE,
    RecoveryError,
    StaleVersionError,
)
from mesh_editor.samplegen import build_demo_mesh
from mesh_editor.topology import weld_vertices
from mesh_editor.versioning import DependencyCache, VersionRepository


def initial_mesh():
    return weld_vertices(build_demo_mesh(), 1e-6).mesh


def commit_transform(repo: VersionRepository, base: int, dx: float = 0.1) -> int:
    mesh, _ = repo.load_current()
    result = transform_selection(
        mesh,
        Selection(face_indices=(0, 1, 2)),
        translation=(dx, 0.0, 0.0),
    )
    entry = repo.commit_edit(
        base,
        result.mesh,
        metrics_before=result.metrics_before,
        metrics_after=result.metrics_after,
        operation=result.operation,
        weld_tolerance=1e-6,
    )
    return int(entry["version"])


class VersioningTests(unittest.TestCase):
    def test_commit_rollback_undo_and_monotonic_versions(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = VersionRepository(temp)
            mesh = initial_mesh()
            v1 = repo.create_baseline(mesh, weld_tolerance=1e-6)["version"]
            v2 = commit_transform(repo, v1)
            mesh2, entry2 = repo.load_current()
            v3 = commit_transform(repo, v2, 0.2)
            self.assertEqual((v1, v2, v3), (1, 2, 3))
            repo.undo_last_commit()
            restored, restored_entry = repo.load_current()
            self.assertEqual(restored_entry["version"], v2)
            self.assertEqual(restored.metrics(), mesh2.metrics())
            self.assertEqual(restored_entry["metrics_after"], entry2["metrics_after"])
            v4 = commit_transform(repo, v2, 0.3)
            self.assertEqual(v4, 4)
            _, entry4 = repo.load_current()
            self.assertEqual(entry4["parent"], v2)
            self.assertEqual(
                entry4["dependencies"]["base_content_hash"], entry2["content_hash"]
            )
            self.assertNotEqual(
                entry4["dependencies"]["dependency_key"],
                repo.read_manifest()["versions"][2]["dependencies"]["dependency_key"],
            )
            repo.rollback_to(v1)
            self.assertEqual(repo.current_entry()["version"], v1)
            report = repo.recover()
            self.assertEqual(report.recovered_version, v1)

    def test_manifest_contains_hash_chain_and_dependencies(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = VersionRepository(temp)
            repo.create_baseline(initial_mesh(), weld_tolerance=1e-6)
            commit_transform(repo, 1)
            manifest = repo.read_manifest(recovery=True)
            first, second = manifest["versions"]
            self.assertEqual(second["previous_entry_hash"], first["entry_hash"])
            self.assertEqual(manifest["chain_head"], second["entry_hash"])
            self.assertIn("weld_tolerance", second["dependencies"])
            self.assertIn("format_version", second["dependencies"])
            self.assertIn("content_hash", second)
            self.assertEqual(repo.recover().recovered_version, 2)

    def test_export_detects_stale_content_hash(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = VersionRepository(temp)
            repo.create_baseline(initial_mesh(), weld_tolerance=1e-6)
            entry = repo.current_entry()
            snapshot = repo.snapshots_dir / entry["mesh_relpath"]
            snapshot.write_bytes(snapshot.read_bytes()[:20])
            with self.assertRaises(StaleVersionError) as caught:
                repo.export_version(1, Path(temp) / "out.obj", "obj", 2)
        self.assertEqual(caught.exception.code, E_VERSION_STALE)

    def test_recovery_rejects_truncated_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = VersionRepository(temp)
            repo.create_baseline(initial_mesh(), weld_tolerance=1e-6)
            entry = repo.current_entry()
            snapshot = repo.snapshots_dir / entry["mesh_relpath"]
            snapshot.write_bytes(snapshot.read_bytes()[:-10])
            with self.assertRaises(RecoveryError) as caught:
                repo.recover()
        self.assertEqual(caught.exception.code, E_RECOVERY_INVALID)

    def test_recovery_rejects_manifest_number_mismatch(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = VersionRepository(temp)
            repo.create_baseline(initial_mesh(), weld_tolerance=1e-6)
            manifest = json.loads(repo.manifest_path.read_text(encoding="utf-8"))
            manifest["next_version"] = 99
            repo.manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
            with self.assertRaises(RecoveryError) as caught:
                repo.recover()
        self.assertEqual(caught.exception.code, E_RECOVERY_INVALID)

    def test_failed_commit_keeps_previous_version(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = VersionRepository(temp)
            repo.create_baseline(initial_mesh(), weld_tolerance=1e-6)
            before = repo.current_entry()["content_hash"]
            mesh, _ = repo.load_current()
            result = transform_selection(mesh, Selection(face_indices=(0,)))
            with mock.patch.dict(os.environ, {"MESH_EDITOR_FAIL_COMMIT": "1"}):
                with self.assertRaises(CommitError) as caught:
                    repo.commit_edit(
                        1,
                        result.mesh,
                        metrics_before=result.metrics_before,
                        metrics_after=result.metrics_after,
                        operation=result.operation,
                        weld_tolerance=1e-6,
                    )
            self.assertEqual(caught.exception.code, E_COMMIT_FAILED)
            self.assertEqual(repo.current_entry()["content_hash"], before)
            self.assertEqual(repo.recover().recovered_version, 1)
            self.assertFalse(list(repo.snapshots_dir.glob("v00000002")))

    def test_forced_termination_after_staging_recovers_committed_state(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = VersionRepository(temp)
            repo.create_baseline(initial_mesh(), weld_tolerance=1e-6)
            script = f"""
import os, sys
sys.path.insert(0, {str(Path.cwd())!r})
from mesh_editor.editing import Selection, transform_selection
from mesh_editor.versioning import VersionRepository
repo = VersionRepository({temp!r})
mesh, entry = repo.load_current()
result = transform_selection(mesh, Selection(face_indices=(0,)))
repo.commit_edit(1, result.mesh, metrics_before=result.metrics_before,
    metrics_after=result.metrics_after, operation=result.operation, weld_tolerance=1e-6)
"""
            env = dict(os.environ)
            env["MESH_EDITOR_CRASH_AFTER_STAGE"] = "1"
            process = subprocess.run(
                [sys.executable, "-c", script], env=env, capture_output=True, text=True, check=False
            )
            self.assertEqual(process.returncode, 91, process.stderr)
            self.assertEqual(repo.current_entry()["version"], 1)
            self.assertEqual(len(list(repo.snapshots_dir.glob(".staging-*"))), 1)
            report = repo.recover()
            self.assertEqual(report.recovered_version, 1)
            self.assertEqual(report.discarded_staging, 1)
            self.assertFalse(list(repo.snapshots_dir.glob(".staging-*")))

    def test_forced_termination_before_manifest_discards_orphan(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = VersionRepository(temp)
            repo.create_baseline(initial_mesh(), weld_tolerance=1e-6)
            script = f"""
import sys
sys.path.insert(0, {str(Path.cwd())!r})
from mesh_editor.editing import Selection, transform_selection
from mesh_editor.versioning import VersionRepository
repo = VersionRepository({temp!r})
mesh, entry = repo.load_current()
result = transform_selection(mesh, Selection(face_indices=(0,)))
repo.commit_edit(1, result.mesh, metrics_before=result.metrics_before,
    metrics_after=result.metrics_after, operation=result.operation, weld_tolerance=1e-6)
"""
            env = dict(os.environ)
            env["MESH_EDITOR_CRASH_BEFORE_MANIFEST"] = "1"
            process = subprocess.run(
                [sys.executable, "-c", script], env=env, capture_output=True, text=True, check=False
            )
            self.assertEqual(process.returncode, 92, process.stderr)
            self.assertTrue((repo.snapshots_dir / "v00000002").exists())
            report = repo.recover()
            self.assertEqual(report.recovered_version, 1)
            self.assertEqual(report.discarded_orphan_versions, 1)
            self.assertFalse((repo.snapshots_dir / "v00000002").exists())

    def test_rollback_invalidates_dependency_cache(self) -> None:
        cache = DependencyCache()
        with tempfile.TemporaryDirectory() as temp:
            repo = VersionRepository(temp, cache=cache)
            repo.create_baseline(initial_mesh(), weld_tolerance=1e-6)
            commit_transform(repo, 1)
            key = DependencyCache.make_key(
                base_version=2,
                base_content_hash=repo.current_entry()["content_hash"],
                weld_tolerance=1e-6,
                format_version=2,
                operation_signature={"type": "edge_collapse", "target_ratio": 0.5},
            )
            cache.put(key, "descendant-result")
            self.assertEqual(cache.get(key), "descendant-result")
            repo.rollback_to(1)
            self.assertIsNone(cache.get(key))
            self.assertGreaterEqual(cache.invalidations, 1)

    def test_stale_commit_base_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = VersionRepository(temp)
            repo.create_baseline(initial_mesh(), weld_tolerance=1e-6)
            commit_transform(repo, 1)
            mesh, _ = repo.load_current()
            result = transform_selection(mesh, Selection(face_indices=(0,)))
            with self.assertRaises(StaleVersionError):
                repo.commit_edit(
                    1,
                    result.mesh,
                    metrics_before=result.metrics_before,
                    metrics_after=result.metrics_after,
                    operation=result.operation,
                    weld_tolerance=1e-6,
                )


if __name__ == "__main__":
    unittest.main()
