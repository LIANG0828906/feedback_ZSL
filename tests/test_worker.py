import time
import unittest

from PySide6.QtWidgets import QApplication

from terrain_tool.errors import ErrorCode, TerrainError
from terrain_tool.worker import RecomputeManager

_app = QApplication.instance() or QApplication([])


class WorkerManagerTest(unittest.TestCase):
    def setUp(self) -> None:
        self.manager = RecomputeManager()
        self.events: list = []
        self.manager.job_finished.connect(lambda r: self.events.append(("ok", r)))
        self.manager.job_failed.connect(lambda e: self.events.append(("fail", e.code)))

    def tearDown(self) -> None:
        self.manager.shutdown()

    def test_busy_rejects_second_job(self) -> None:
        def slow(token, progress):
            for _ in range(30):
                token.raise_if_cancelled()
                time.sleep(0.005)
            return "done"

        self.manager.submit(slow)
        with self.assertRaises(TerrainError) as ctx:
            self.manager.submit(lambda t, p: "x")
        self.assertEqual(ctx.exception.code, ErrorCode.E_BUSY)
        self.manager.cancel()

    def test_cooperative_cancel_and_release(self) -> None:
        def slow(token, progress):
            for _ in range(100):
                token.raise_if_cancelled()
                time.sleep(0.005)
            return "done"

        self.manager.submit(slow)
        self.assertTrue(self.manager.cancel())
        deadline = time.time() + 3
        while self.manager.busy and time.time() < deadline:
            _app.processEvents()
            time.sleep(0.01)
        self.assertFalse(self.manager.busy)
        self.assertIn(("fail", ErrorCode.E_CANCELLED), self.events)

    def test_job_runs_in_background_and_returns(self) -> None:
        self.manager.submit(lambda t, p: 42)
        deadline = time.time() + 3
        while not self.events and time.time() < deadline:
            _app.processEvents()
            time.sleep(0.01)
        self.assertIn(("ok", 42), self.events)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
