from __future__ import annotations

import os
import tempfile
import threading
import time
import unittest
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication

from spectraforge.checkpoint import CheckpointManager
from spectraforge.errors import WorkbenchError
from spectraforge.metadata import load_scene
from spectraforge.scheduler import BufferPool, ResourceConfig
from spectraforge.scheduler import ProcessingJob
from spectraforge.segmentation import make_segments, segmentation_spec
from spectraforge.ui.app import MainWindow
from spectraforge.ui.theme import APP_QSS

ROOT = Path(__file__).resolve().parents[1]


class TestSchedulerAndUI(unittest.TestCase):
    def test_buffer_pool_blocks_instead_of_dropping(self) -> None:
        pool = BufferPool(1000)
        stop = threading.Event()
        self.assertTrue(pool.acquire(700, stop))
        acquired = threading.Event()

        def worker() -> None:
            if pool.acquire(500, stop):
                acquired.set()
                pool.release(500)

        thread = threading.Thread(target=worker)
        thread.start()
        time.sleep(0.1)
        self.assertFalse(acquired.is_set())
        pool.release(700)
        thread.join(timeout=2)
        self.assertTrue(acquired.is_set())
        self.assertGreaterEqual(pool.metrics()["wait_count"], 1)

    def test_resource_parameter_errors_name_the_field(self) -> None:
        with self.assertRaises(WorkbenchError) as raised:
            ResourceConfig(0, 64).validate()
        self.assertEqual(raised.exception.code, "E_PARA")
        self.assertEqual(raised.exception.details["field"], "max_workers")

    def test_per_thread_buffer_limit_blocks_segment_start(self) -> None:
        with tempfile.TemporaryDirectory(prefix="sf-buffer-") as temp:
            scene = load_scene(ROOT / "samples" / "scenes" / "neon_forge_424242")
            segments = make_segments(
                scene.width,
                scene.height,
                scene.band_count,
                split_mode="bands",
            )
            config = segmentation_spec(
                scene.width,
                scene.height,
                scene.band_count,
                split_mode="bands",
                tile_width=32,
                tile_height=32,
                interpolation="bilinear",
            )
            manager = CheckpointManager(Path(temp), scene, segments, config)
            try:
                job = ProcessingJob(
                    scene,
                    manager,
                    resource=ResourceConfig(1, 1),
                    params=scene.metadata.default_parameters,
                )
                with self.assertRaises(WorkbenchError) as raised:
                    job.run()
                self.assertEqual(raised.exception.code, "E_PARA")
                self.assertEqual(raised.exception.details["field"], "per_thread_buffer_mb")
            finally:
                manager.close()

    def test_offscreen_ui_loads_processes_and_renders(self) -> None:
        app = QApplication.instance() or QApplication([])
        app.setStyle("Fusion")
        app.setStyleSheet(APP_QSS)
        with tempfile.TemporaryDirectory(prefix="sf-ui-") as temp:
            window = MainWindow(ROOT / "samples", Path(temp))
            window.show()
            app.processEvents()
            self.assertIsNotNone(window.scene)
            self.assertEqual(window.table.rowCount(), 12)
            window.start_processing()
            deadline = time.time() + 15
            while time.time() < deadline and window._job_running:
                app.processEvents()
                time.sleep(0.01)
            app.processEvents()
            self.assertFalse(window._job_running)
            self.assertIsNotNone(window.output)
            self.assertTrue(window.output.complete)
            self.assertEqual(window.manager.progress()["committed"], 12)
            window.table.selectRow(2)
            app.processEvents()
            screenshot = window.grab()
            self.assertFalse(screenshot.isNull())
            self.assertGreater(screenshot.width(), 1000)
            window.close()
            app.processEvents()


if __name__ == "__main__":
    unittest.main()
