from __future__ import annotations

import json
import os
import tempfile
import unittest
import wave
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

import numpy as np

from audio_sync.config import AlignmentConfig
from audio_sync.engine import AlignmentEngine
from audio_sync.errors import E_PARA, SyncError
from audio_sync.export import export_alignment
from audio_sync.generator import SyntheticSessionConfig, generate_session
from audio_sync.manifest import load_session


class ExportAndGuiTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory(prefix="audio-sync-export-")
        cls.root = Path(cls.temp.name)
        cls.session_dir = cls.root / "session"
        generate_session(
            cls.session_dir,
            config=SyntheticSessionConfig(session_id="export-test", seed=20261125, duration_seconds=7.0),
        )
        cls.session = load_session(cls.session_dir)
        cls.result = AlignmentEngine(cls.session, AlignmentConfig()).run("full")

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def test_multichannel_npy_wav_and_layout_sidecar(self) -> None:
        output = self.root / "export"
        info = export_alignment(self.result, output)
        data = np.load(output / "aligned_multichannel.npy")
        self.assertEqual(data.shape, (self.session.manifest.session_samples, 4))
        self.assertEqual(data.dtype, np.float32)
        with wave.open(str(output / "aligned_multichannel.wav"), "rb") as handle:
            self.assertEqual(handle.getnchannels(), 4)
            self.assertEqual(handle.getsampwidth(), 4)
            self.assertEqual(handle.getframerate(), self.session.manifest.nominal_sample_rate)
            self.assertEqual(handle.getnframes(), self.session.manifest.session_samples)
        sidecar = json.loads((output / "aligned_multichannel.layout.json").read_text(encoding="utf-8"))
        self.assertEqual(sidecar["layout"]["channel_count"], 4)
        self.assertEqual(
            sorted(item["channel_id"] for item in sidecar["layout"]["slot_map"]),
            sorted(self.result.channels),
        )
        self.assertIn("low_confidence_channels", sidecar)
        self.assertTrue(Path(info["error_report"]["path"]).exists())

    def test_low_confidence_channels_are_explicitly_reported(self) -> None:
        result = AlignmentEngine(
            self.session,
            AlignmentConfig(confidence_limit_samples=0.001),
        ).run("full")
        self.assertEqual(
            result.low_confidence_channels,
            ["capture_a", "capture_b", "capture_c"],
        )
        output = self.root / "low-confidence"
        export_alignment(result, output)
        sidecar = json.loads((output / "aligned_multichannel.layout.json").read_text(encoding="utf-8"))
        self.assertEqual(sidecar["low_confidence_channels"], result.low_confidence_channels)
        self.assertEqual(sidecar["low_confidence_policy"], "flagged_and_reported_never_silently_accepted")

    def test_layout_must_map_every_channel(self) -> None:
        with self.assertRaises(SyncError) as context:
            export_alignment(
                self.result,
                self.root / "bad-layout",
                channels=["ref", "capture_a"],
                speakers=["FL", "FR"],
            )
        self.assertEqual(context.exception.code, E_PARA)
        self.assertEqual(context.exception.field, "layout")

    def test_gui_loads_and_renders_offscreen(self) -> None:
        from PySide6 import QtWidgets

        from audio_sync.gui import MainWindow

        app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
        window = MainWindow(self.session_dir)
        window.resize(1200, 800)
        window.show()
        app.processEvents()
        self.assertIsNotNone(window.session)
        window.result = self.result
        window._on_success(self.result)
        app.processEvents()
        pixmap = window.canvas.grab()
        self.assertFalse(pixmap.isNull())
        self.assertGreater(pixmap.width(), 500)
        self.assertEqual(window.table.rowCount(), 4)
        window.close()
        app.processEvents()


if __name__ == "__main__":
    unittest.main()

