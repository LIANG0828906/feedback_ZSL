from __future__ import annotations

import tempfile
import unittest

from PyQt5.QtCore import QCoreApplication, QThread, QTimer

from mesh_editor.samplegen import build_demo_mesh
from mesh_editor.topology import weld_vertices
from mesh_editor.ui import EditWorker
from mesh_editor.versioning import VersionRepository


class WorkerTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.app = QCoreApplication.instance() or QCoreApplication([])

    def _run_worker(self, worker: EditWorker) -> list[tuple[str, object]]:
        events: list[tuple[str, object]] = []
        thread = QThread()
        worker.moveToThread(thread)
        thread.started.connect(worker.run)
        worker.committed.connect(lambda version, entry: events.append(("committed", version)))
        worker.cancelled.connect(lambda: events.append(("cancelled", None)))
        worker.failed.connect(lambda error: events.append(("failed", error)))
        worker.finished.connect(thread.quit)
        thread.finished.connect(self.app.quit)
        QTimer.singleShot(8000, self.app.quit)
        thread.start()
        self.app.exec_()
        thread.wait(2000)
        return events

    def test_background_worker_commits_new_version(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = VersionRepository(temp)
            repo.create_baseline(weld_vertices(build_demo_mesh(), 1e-6).mesh, weld_tolerance=1e-6)
            worker = EditWorker(
                repo,
                base_version=1,
                operation="transform",
                parameters={
                    "faces": [0, 1],
                    "translation": [0.1, 0.0, 0.0],
                    "rotation": [0.0, 0.0, 0.0],
                    "scale": 1.0,
                },
                weld_tolerance=1e-6,
            )
            events = self._run_worker(worker)
            self.assertIn(("committed", 2), events)
            self.assertEqual(repo.current_entry()["version"], 2)

    def test_cancelled_worker_does_not_commit_or_change_head(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = VersionRepository(temp)
            repo.create_baseline(weld_vertices(build_demo_mesh(), 1e-6).mesh, weld_tolerance=1e-6)
            before = repo.current_entry()["content_hash"]
            worker = EditWorker(
                repo,
                base_version=1,
                operation="simplify",
                parameters={"ratio": 0.2},
                weld_tolerance=1e-6,
            )
            worker.cancel()
            events = self._run_worker(worker)
            self.assertIn(("cancelled", None), events)
            self.assertEqual(repo.current_entry()["version"], 1)
            self.assertEqual(repo.current_entry()["content_hash"], before)


if __name__ == "__main__":
    unittest.main()
