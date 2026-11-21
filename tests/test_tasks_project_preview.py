from __future__ import annotations

import json
import tempfile
import threading
import unittest
from pathlib import Path
from types import SimpleNamespace

from tests.helpers import rectangle
from vpipe.errors import (
    E_BUSY,
    E_PREVIEW_STALE,
    E_PROJECT_MISMATCH,
    PipelineError,
)
from vpipe.metrics import ErrorThresholds
from vpipe.project import load_project, save_project
from vpipe.raster import PreviewService, RasterSpec, rasterize_path
from vpipe.tasks import BooleanService


class TaskProjectPreviewTests(unittest.TestCase):
    def setUp(self) -> None:
        self.first = rectangle(0, 0, 10, 10, "a")
        self.second = rectangle(5, 5, 15, 15, "b")

    def test_duplicate_pair_is_busy_and_cancel_preserves_last_result(self) -> None:
        service = BooleanService()
        committed = service.run_sync(self.first, self.second, "union", 1e-5)
        entered = threading.Event()
        release = threading.Event()

        def gate() -> None:
            entered.set()
            release.wait(5.0)

        task_id = service.submit(
            self.first,
            self.second,
            "intersection",
            1e-5,
            commit_gate=gate,
        )
        self.assertTrue(entered.wait(5.0))
        with self.assertRaises(PipelineError) as raised:
            service.submit(self.first, self.second, "xor", 1e-5)
        self.assertEqual(raised.exception.code, E_BUSY)
        self.assertTrue(service.cancel(task_id))
        release.set()
        info = service.wait(task_id, 5.0)
        self.assertIsNotNone(info)
        self.assertEqual(info.status, "cancelled")
        self.assertEqual(service.store.latest().result_hash, committed.result_hash)
        snapshot, frame = service.store.snapshot()
        self.assertEqual(snapshot.result_hash, committed.result_hash)
        self.assertEqual(frame.relevant_result_hash, committed.result_hash)

    def test_preview_rejects_stale_binding(self) -> None:
        service = BooleanService()
        first_result = service.run_sync(self.first, self.second, "union", 1e-5)
        service.run_sync(self.first, self.second, "intersection", 1e-5)
        with self.assertRaises(PipelineError) as raised:
            service.store.read_preview(first_result.result_hash)
        self.assertEqual(raised.exception.code, E_PREVIEW_STALE)

        preview = PreviewService()
        path = self.first
        spec = RasterSpec.auto([path], max_side=32)
        frame = rasterize_path(path, spec, 1e-6)
        with self.assertRaises(PipelineError) as raised:
            preview.publish("sha256:uncommitted", frame)
        self.assertEqual(raised.exception.code, E_PREVIEW_STALE)

    def test_project_round_trip_and_atomic_recovery(self) -> None:
        service = BooleanService()
        record = service.run_sync(self.first, self.second, "union", 1e-5)
        with tempfile.TemporaryDirectory() as temporary:
            project_path = Path(temporary) / "project.vproj"
            document = save_project(
                project_path,
                self.first,
                self.second,
                "union",
                1e-5,
                {"input_versions": [1, 1]},
                ErrorThresholds().to_dict(),
                record,
            )
            self.assertEqual(len(document["content_hash"]), 71)
            loaded = load_project(project_path)
            self.assertEqual(loaded.operation, "union")
            self.assertTrue(loaded.last_committed["committed"])
            restored = BooleanService().restore_project(loaded)
            self.assertIsNotNone(restored)
            self.assertEqual(restored.result_hash, record.result_hash)
            self.assertFalse(any(path.name.endswith(".tmp") for path in Path(temporary).iterdir()))

    def test_project_version_and_hash_mismatch_abort(self) -> None:
        service = BooleanService()
        record = service.run_sync(self.first, self.second, "union", 1e-5)
        with tempfile.TemporaryDirectory() as temporary:
            project_path = Path(temporary) / "project.vproj"
            save_project(
                project_path,
                self.first,
                self.second,
                "union",
                1e-5,
                {},
                ErrorThresholds().to_dict(),
                record,
            )
            document = json.loads(project_path.read_text(encoding="utf-8"))
            document["payload"]["tolerance"] = 0.02
            project_path.write_text(
                json.dumps(document, ensure_ascii=False, sort_keys=True),
                encoding="utf-8",
            )
            with self.assertRaises(PipelineError) as raised:
                load_project(project_path)
            self.assertEqual(raised.exception.code, E_PROJECT_MISMATCH)

            document = json.loads(project_path.read_text(encoding="utf-8"))
            document["project_version"] = 99
            project_path.write_text(json.dumps(document), encoding="utf-8")
            with self.assertRaises(PipelineError) as raised:
                load_project(project_path)
            self.assertEqual(raised.exception.code, E_PROJECT_MISMATCH)

    def test_uncommitted_intermediate_cannot_be_saved(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            with self.assertRaises(PipelineError) as raised:
                save_project(
                    Path(temporary) / "project.vproj",
                    self.first,
                    self.second,
                    "union",
                    1e-5,
                    {},
                    ErrorThresholds().to_dict(),
                    SimpleNamespace(committed=False),
                )
            self.assertEqual(raised.exception.code, E_PROJECT_MISMATCH)


if __name__ == "__main__":
    unittest.main()

