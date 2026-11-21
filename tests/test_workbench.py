from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from pcwb.repository import PointCloudRepository
from pcwb.workbench import WorkbenchOptions, run_workbench
from tests.helpers import sample_cloud


class WorkbenchTests(unittest.TestCase):
    def test_headless_renderer_reads_published_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo_path = Path(directory) / "repo"
            repo = PointCloudRepository.create(repo_path, sample_cloud(40))
            expected = repo.current_snapshot
            report = run_workbench(
                repo,
                WorkbenchOptions(headless=True, frames=3),
            )
            self.assertIn(report["renderer"], {"opengl", "software_snapshot_fallback"})
            self.assertEqual(report["final_version"], expected.version)
            self.assertEqual(report["final_hash"], expected.content_hash)
            self.assertEqual(report["frames"], 3)


if __name__ == "__main__":
    unittest.main()
