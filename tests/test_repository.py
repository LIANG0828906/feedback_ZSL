from __future__ import annotations

import os
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

from pcwb.errors import CommitFailed
from pcwb.format import serialize_point_cloud
from pcwb.operators import run_operator
from pcwb.repository import PointCloudRepository
from tests.helpers import all_selection, sample_cloud

ROOT = Path(__file__).resolve().parents[1]


class RepositoryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name) / "repo"
        self.cloud = sample_cloud(12)
        self.repo = PointCloudRepository.create(self.root, self.cloud)

    def tearDown(self) -> None:
        self.directory.cleanup()

    def _color_edit(self, repo: PointCloudRepository, rgb: list[int]):
        snapshot = repo.current_snapshot
        selection = all_selection(snapshot.cloud, snapshot.version)
        return run_operator(snapshot, selection, "color_overwrite", {"rgb": rgb})

    def test_commit_is_monotonic_and_manifest_hash_is_durable(self) -> None:
        base = self.repo.current_snapshot
        result = self._color_edit(self.repo, [1, 2, 3])
        commit = self.repo.commit(
            result.cloud, base_version=base.version, record=result.record
        )
        self.assertEqual(commit.version, 2)
        current = self.repo.current_snapshot
        self.assertEqual(current.version, 2)
        self.assertEqual(current.content_hash, result.record.output_hash)
        manifest = self.repo.manifest()
        self.assertEqual(manifest["current_version"], 2)
        self.assertEqual(
            manifest["versions"][-1]["output_hash"], result.record.output_hash
        )
        self.assertTrue(commit.object_path.exists())
        self.assertFalse(list(commit.object_path.parent.glob("*.tmp")))

    def test_manifest_failure_preserves_previous_version_and_cleans_partial(
        self,
    ) -> None:
        base = self.repo.current_snapshot
        result = self._color_edit(self.repo, [9, 8, 7])

        def fail_manifest(_manifest):
            raise OSError("injected manifest write failure")

        self.repo._write_manifest_atomic = fail_manifest  # type: ignore[method-assign]
        with self.assertRaises(CommitFailed) as caught:
            self.repo.commit(
                result.cloud, base_version=base.version, record=result.record
            )
        self.assertEqual(caught.exception.code, "E_COMMIT_FAILED")
        self.assertEqual(self.repo.current_snapshot.version, 1)
        self.assertEqual(self.repo.current_snapshot.content_hash, base.content_hash)
        self.assertFalse((self.repo.objects_dir / "v00000002.pcwb").exists())
        self.assertFalse(list(self.repo.objects_dir.glob("*.tmp")))

        del self.repo._write_manifest_atomic
        result = self._color_edit(self.repo, [7, 7, 7])
        commit = self.repo.commit(
            result.cloud, base_version=base.version, record=result.record
        )
        self.assertEqual(commit.version, 2)

    def test_checkout_preserves_history_and_new_commit_uses_historical_parent(
        self,
    ) -> None:
        base = self.repo.current_snapshot
        result_v2 = self._color_edit(self.repo, [2, 2, 2])
        v2 = self.repo.commit(
            result_v2.cloud,
            base_version=base.version,
            record=result_v2.record,
        )
        self.repo.checkout(1)
        self.assertEqual(self.repo.current_snapshot.version, 1)
        result = self._color_edit(self.repo, [3, 3, 3])
        v3 = self.repo.commit(result.cloud, base_version=1, record=result.record)
        self.assertEqual(v3.version, 3)
        self.assertEqual(v3.snapshot.parent_version, 1)
        self.assertEqual(self.repo.manifest()["max_version"], 3)
        self.assertEqual(self.repo.load_version(2).content_hash, v2.output_hash)

    def test_recover_removes_orphans_and_temporary_files(self) -> None:
        cloud = sample_cloud(3)
        orphan = self.repo.objects_dir / "v00000002.pcwb"
        orphan.write_bytes(serialize_point_cloud(cloud))
        temporary = self.repo.objects_dir / ".v00000002.pcwb.dead.tmp"
        temporary.write_bytes(b"half")
        report = self.repo.recover()
        self.assertEqual(report.current_version, 1)
        self.assertEqual(report.removed_orphan_objects, 1)
        self.assertGreaterEqual(report.removed_temporary_files, 1)
        self.assertFalse(orphan.exists())
        self.assertFalse(temporary.exists())
        self.assertEqual(self.repo.current_snapshot.version, 1)

    def test_forced_termination_restores_only_last_committed_or_ignores_orphan(
        self,
    ) -> None:
        environment = dict(os.environ)
        environment["PYTHONPATH"] = str(ROOT / "src")
        script = textwrap.dedent(
            """
            import os, tempfile
            from pathlib import Path
            from pcwb.format import serialize_point_cloud
            from pcwb.repository import PointCloudRepository
            from pcwb.model import PointCloud
            import numpy as np

            repo = PointCloudRepository(ROOT)
            payload = serialize_point_cloud(PointCloud.from_arrays(
                np.array([[0.0, 0.0, 0.0]], dtype=np.float32),
                np.array([[1, 2, 3]], dtype=np.uint8),
            ))
            fd, name = tempfile.mkstemp(prefix=".uncommitted.", suffix=".tmp", dir=repo.objects_dir)
            with os.fdopen(fd, "wb") as stream:
                stream.write(payload)
                stream.flush()
                os.fsync(stream.fileno())
            final = repo.objects_dir / "v00000002.pcwb"
            os.link(name, final)
            os._exit(71)
            """
        ).replace("ROOT", repr(str(self.root)))
        completed = subprocess.run(
            [sys.executable, "-c", script],
            cwd=ROOT,
            env=environment,
            check=False,
        )
        self.assertEqual(completed.returncode, 71)
        reopened = PointCloudRepository(self.root)
        self.assertEqual(reopened.current_snapshot.version, 1)
        self.assertFalse((self.root / "objects" / "v00000002.pcwb").exists())
        self.assertFalse(list((self.root / "objects").glob("*.tmp")))

    def test_crash_after_commit_recovers_that_commit(self) -> None:
        environment = dict(os.environ)
        environment["PYTHONPATH"] = str(ROOT / "src")
        script = textwrap.dedent(
            """
            import os
            from pcwb.repository import PointCloudRepository
            from pcwb.operators import run_operator
            from pcwb.selection import Selection
            import numpy as np

            repo = PointCloudRepository(ROOT)
            base = repo.current_snapshot
            selection = Selection.from_indices(base, np.arange(base.count))
            result = run_operator(base, selection, "color_overwrite", {"rgb": [4, 5, 6]})
            repo.commit(result.cloud, base_version=base.version, record=result.record)
            os._exit(0)
            """
        ).replace("ROOT", repr(str(self.root)))
        completed = subprocess.run(
            [sys.executable, "-c", script],
            cwd=ROOT,
            env=environment,
            check=False,
        )
        self.assertEqual(completed.returncode, 0)
        reopened = PointCloudRepository(self.root)
        self.assertEqual(reopened.current_snapshot.version, 2)
        self.assertEqual(reopened.current_snapshot.cloud.rgb[0].tolist(), [4, 5, 6])


if __name__ == "__main__":
    unittest.main()
