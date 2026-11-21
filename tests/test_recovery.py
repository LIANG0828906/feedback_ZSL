from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path

import numpy as np

from acoustic_calibration.engine import CalibrationEngine, CancellationToken
from acoustic_calibration.errors import CalibrationError, E_CANCELLED, E_RECOVERY_INVALID
from acoustic_calibration.models import CalibrationParams
from acoustic_calibration.samples import generate_sample_wav


def _prepare_cancelled_run(root: Path) -> tuple[CalibrationEngine, CalibrationParams, Path]:
    info = generate_sample_wav(root / "sample.wav", duration_s=0.5)
    engine = CalibrationEngine(root / "workspace", info.path)
    base = CalibrationParams()
    engine.submit(base)
    changed = replace(base, smoothing_fraction=0.25)
    token = CancellationToken()

    def progress(event: dict[str, object]) -> None:
        if event.get("stage") == "channel_complete" and event.get("completed") == 1:
            token.cancel()

    with unittest.TestCase().assertRaises(CalibrationError) as context:
        engine.submit(changed, token=token, progress=progress)
    if context.exception.code != E_CANCELLED:
        raise context.exception
    return engine, changed, info.path


class RecoveryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_cancelled_run_recovers_from_disk_and_matches_clean_run(self) -> None:
        _, changed, source = _prepare_cancelled_run(self.root)
        recovered_engine = CalibrationEngine(self.root / "workspace", source)
        report = recovered_engine.recover()
        self.assertFalse(report.already_published)
        self.assertEqual(report.reused_channels, (0,))
        self.assertEqual(report.recomputed_channels, (1, 2, 3, 4, 5))
        self.assertEqual(report.state.version, 2)

        clean_engine = CalibrationEngine(self.root / "clean", source)
        clean = clean_engine.submit(changed)
        for recovered_channel, clean_channel in zip(report.state.channels, clean.channels, strict=True):
            self.assertEqual(recovered_channel.alignment, clean_channel.alignment)
            self.assertEqual(recovered_channel.spectral, clean_channel.spectral)
            self.assertEqual(recovered_channel.smoothing, clean_channel.smoothing)
            self.assertEqual(recovered_channel.filter.coefficients, clean_channel.filter.coefficients)
            self.assertEqual(
                tuple(error.upper_db for error in recovered_channel.errors),
                tuple(error.upper_db for error in clean_channel.errors),
            )
        self.assertEqual(
            tuple(alert.to_dict() for alert in report.state.consistency_alerts),
            tuple(alert.to_dict() for alert in clean.consistency_alerts),
        )

    def test_initial_cancelled_run_recovers_as_first_published_version(self) -> None:
        info = generate_sample_wav(self.root / "initial.wav", duration_s=0.4)
        workspace = self.root / "initial-workspace"
        engine = CalibrationEngine(workspace, info.path)
        token = CancellationToken()

        def progress(event: dict[str, object]) -> None:
            if event.get("stage") == "channel_complete" and event.get("completed") == 1:
                token.cancel()

        with self.assertRaises(CalibrationError) as context:
            engine.submit(CalibrationParams(), token=token, progress=progress)
        self.assertEqual(context.exception.code, E_CANCELLED)
        self.assertIsNone(engine.current_state())

        recovered = CalibrationEngine(workspace, info.path).recover()
        self.assertEqual(recovered.state.version, 1)
        self.assertEqual(recovered.reused_channels, (0,))
        self.assertEqual(recovered.recomputed_channels, (1, 2, 3, 4, 5))

    def test_snapshot_hash_break_reports_first_broken_interval(self) -> None:
        engine, _, source = _prepare_cancelled_run(self.root)
        snapshot = engine.snapshot_manager.load_latest_valid()
        self.assertIsNotNone(snapshot)
        manifest_path = snapshot.manifest_path
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        segment_name = manifest["segments"][0]["file"]
        segment_path = manifest_path.parent / segment_name
        os.chmod(segment_path, 0o644)
        payload = json.loads(segment_path.read_text(encoding="utf-8"))
        payload["payload"]["channel"]["alignment"]["arrival_sample"] += 1
        segment_path.write_text(json.dumps(payload), encoding="utf-8")

        new_engine = CalibrationEngine(self.root / "workspace", source)
        with self.assertRaises(CalibrationError) as context:
            new_engine.recover()
        self.assertEqual(context.exception.code, E_RECOVERY_INVALID)
        interval = context.exception.details["first_broken_interval"]
        self.assertEqual(interval["channel_index"], 0)
        self.assertEqual(interval["band_start"], 0)

    def test_snapshot_missing_field_is_rejected(self) -> None:
        engine, _, source = _prepare_cancelled_run(self.root)
        snapshot = engine.snapshot_manager.load_latest_valid()
        self.assertIsNotNone(snapshot)
        manifest_path = snapshot.manifest_path
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest.pop("chain_head")
        os.chmod(manifest_path, 0o644)
        manifest_path.write_text(json.dumps(manifest), encoding="utf-8")

        new_engine = CalibrationEngine(self.root / "workspace", source)
        with self.assertRaises(CalibrationError) as context:
            new_engine.recover()
        self.assertEqual(context.exception.code, E_RECOVERY_INVALID)
        self.assertIn("chain_head", context.exception.details["missing_fields"])

    def test_recovery_is_idempotent_after_publication(self) -> None:
        _, _, source = _prepare_cancelled_run(self.root)
        first_recovery = CalibrationEngine(self.root / "workspace", source).recover()
        second_recovery = CalibrationEngine(self.root / "workspace", source).recover()
        self.assertTrue(second_recovery.already_published)
        self.assertEqual(second_recovery.state.version, first_recovery.state.version)
        self.assertEqual(second_recovery.state.content_hash, first_recovery.state.content_hash)

    @unittest.skipUnless(hasattr(signal, "SIGKILL"), "requires SIGKILL")
    def test_real_sigkill_recovery_uses_only_disk_snapshot(self) -> None:
        info = generate_sample_wav(self.root / "sample.wav", duration_s=0.4)
        workspace = self.root / "workspace"
        parent_engine = CalibrationEngine(workspace, info.path)
        parent_engine.submit(CalibrationParams())
        changed = replace(CalibrationParams(), smoothing_fraction=0.25)
        script = r"""
import os
import signal
import sys
from dataclasses import replace
from pathlib import Path
from acoustic_calibration import CalibrationEngine, CalibrationParams

workspace = Path(sys.argv[1])
source = Path(sys.argv[2])
engine = CalibrationEngine(workspace, source)
params = replace(CalibrationParams(), smoothing_fraction=0.25)

def progress(event):
    if event.get("stage") == "channel_complete" and event.get("completed") == 1:
        os.kill(os.getpid(), signal.SIGKILL)

engine.submit(params, progress=progress)
"""
        result = subprocess.run(
            [sys.executable, "-c", script, str(workspace), str(info.path)],
            cwd=str(Path(__file__).resolve().parents[1]),
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(result.returncode, -signal.SIGKILL, result.stderr)

        recovered_engine = CalibrationEngine(workspace, info.path)
        snapshot = recovered_engine.snapshot_manager.load_latest_valid()
        self.assertIsNotNone(snapshot)
        self.assertEqual(snapshot.status, "running")
        self.assertEqual(snapshot.completed_channel_indices, (0,))
        report = recovered_engine.recover()
        self.assertEqual(report.state.version, 2)
        self.assertEqual(report.reused_channels, (0,))
        self.assertEqual(report.recomputed_channels, (1, 2, 3, 4, 5))
        self.assertEqual(
            tuple(segment.channel_index for segment in recovered_engine.snapshot_manager.load_latest_valid().segments),
            (0, 1, 2, 3, 4, 5),
        )

        clean_engine = CalibrationEngine(self.root / "clean", info.path)
        clean = clean_engine.submit(changed)
        for recovered_channel, clean_channel in zip(report.state.channels, clean.channels, strict=True):
            self.assertEqual(recovered_channel.filter.coefficients, clean_channel.filter.coefficients)
            self.assertEqual(
                tuple(error.upper_db for error in recovered_channel.errors),
                tuple(error.upper_db for error in clean_channel.errors),
            )


if __name__ == "__main__":
    unittest.main()
