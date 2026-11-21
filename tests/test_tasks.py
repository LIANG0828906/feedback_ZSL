from __future__ import annotations

import tempfile
import threading
import time
import unittest
from pathlib import Path

from volrender.errors import ErrorCode, VolumeRenderError
from volrender.renderer import RenderCancelled, render_offscreen
from volrender.tasks import OffscreenRenderService
from tests.helpers import make_volume


class TaskServiceTests(unittest.TestCase):
    def setUp(self) -> None:
        self._temp = tempfile.TemporaryDirectory()
        self.root = Path(self._temp.name)
        self.volume = make_volume(self.root, size=20)
        from volrender.synthetic import baseline_parameters

        self.params = baseline_parameters(self.volume, viewport=(40, 40)).with_changes(step_size=0.04)
        self.release = threading.Event()
        self.entered = threading.Event()

        def controlled_renderer(volume, params, cancel_token=None, batch_size=8):
            self.entered.set()
            while not self.release.wait(0.005):
                if cancel_token is not None and cancel_token.is_set():
                    raise RenderCancelled("cancelled in controlled renderer")
            return render_offscreen(volume, params, batch_size=batch_size)

        self.service = OffscreenRenderService(renderer=controlled_renderer, max_workers=1)

    def tearDown(self) -> None:
        self.release.set()
        self.service.shutdown(wait=True)
        self._temp.cleanup()

    def test_duplicate_volume_submission_is_busy(self) -> None:
        first = self.service.submit("volume-a", self.volume, self.params)
        self.assertTrue(self.entered.wait(2.0))
        with self.assertRaises(VolumeRenderError) as raised:
            self.service.submit("volume-a", self.volume, self.params)
        self.assertEqual(raised.exception.code, ErrorCode.E_BUSY)
        self.assertEqual(raised.exception.field, "volume_key")
        self.release.set()
        result = first.future.result(timeout=5.0)
        self.assertEqual(result.parameter_hash, self.params.parameter_hash)

    def test_cancel_releases_slot_and_preserves_committed_file(self) -> None:
        snapshot_path = self.root / "last-committed.json"
        snapshot_path.write_bytes(b"last-committed-snapshot")
        job = self.service.submit("volume-a", self.volume, self.params, snapshot_path=snapshot_path)
        self.assertTrue(self.entered.wait(2.0))
        self.assertTrue(self.service.cancel("volume-a"))
        with self.assertRaises(RenderCancelled):
            job.future.result(timeout=5.0)
        deadline = time.time() + 2.0
        while self.service.active_volume_keys() and time.time() < deadline:
            time.sleep(0.01)
        self.assertEqual(self.service.active_volume_keys(), ())
        self.assertEqual(snapshot_path.read_bytes(), b"last-committed-snapshot")
        self.release.set()

    def test_actual_renderer_cancels_at_checkpoint(self) -> None:
        with self.assertRaises(RenderCancelled):
            render_offscreen(self.volume, self.params, cancel_token=_SetToken(), batch_size=1)


class _SetToken:
    def is_set(self) -> bool:
        return True


if __name__ == "__main__":
    unittest.main()
