from __future__ import annotations

import tempfile
import threading
import unittest
import json
import os
from dataclasses import replace
from pathlib import Path

import numpy as np

from acoustic_calibration.engine import CalibrationEngine, CancellationToken
from acoustic_calibration.errors import CalibrationError, E_BUSY, E_CANCELLED, E_VERSION_STALE
from acoustic_calibration.models import CalibrationParams, TargetCurve
from acoustic_calibration.samples import generate_sample_wav
from acoustic_calibration.spectral import make_bands


class EngineVersionTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.info = generate_sample_wav(self.root / "sample.wav", duration_s=0.5)
        self.engine = CalibrationEngine(self.root / "workspace", self.info.path)
        self.params = CalibrationParams()

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_monotonic_versions_diff_and_append_only_rollback(self) -> None:
        first = self.engine.submit(self.params)
        second = self.engine.submit(replace(self.params, error_threshold_db=3.0))
        self.assertEqual((first.version, second.version), (1, 2))
        self.assertEqual(second.parent_hash, first.content_hash)
        first_file = self.engine.state_store.version_path(1)
        first_bytes = first_file.read_bytes()

        difference = self.engine.diff(1, 2)
        self.assertIn("error_threshold_db", difference["param_changes"])
        self.assertEqual(
            difference["param_changes"]["error_threshold_db"],
            {"old": 12.0, "new": 3.0},
        )

        rollback = self.engine.rollback(1)
        self.assertEqual(rollback.version, 3)
        self.assertEqual(rollback.rollback_of, 1)
        self.assertEqual(rollback.parent_hash, second.content_hash)
        self.assertEqual(first_file.read_bytes(), first_bytes)
        self.assertEqual(tuple(state.version for state in self.engine.state_store.validate_chain()), (1, 2, 3))

    def test_error_threshold_change_reuses_spectral_smoothing_and_filters(self) -> None:
        first = self.engine.submit(self.params)
        second = self.engine.submit(replace(self.params, error_threshold_db=0.2))
        self.assertTrue(all(node.startswith("error:") for node in second.recomputed_nodes))
        self.assertEqual(second.affected_channels, tuple(range(6)))
        self.assertEqual(len(second.affected_bands), len(second.bands))
        for before, after in zip(first.channels, second.channels, strict=True):
            self.assertEqual(before.filter.coefficients, after.filter.coefficients)
            self.assertEqual(before.spectral, after.spectral)
            self.assertEqual(before.smoothing, after.smoothing)
            self.assertEqual(
                first.node_hash(f"filter:{before.alignment.channel_name}"),
                second.node_hash(f"filter:{after.alignment.channel_name}"),
            )
            self.assertEqual(
                first.node_hash(f"spectral:{before.alignment.channel_name}:0"),
                second.node_hash(f"spectral:{after.alignment.channel_name}:0"),
            )

    def test_target_curve_change_recomputes_only_affected_band_nodes(self) -> None:
        bands = make_bands(self.params.spectral)
        points = [(band.center_hz, 0.0) for band in bands]
        changed_index = len(points) // 2
        points[changed_index] = (points[changed_index][0], 1.0)
        target = TargetCurve(tuple(points))
        first = self.engine.submit(self.params)
        second = self.engine.submit(replace(self.params, target_curve=target))
        self.assertEqual(second.affected_bands, (changed_index,))
        self.assertTrue(any(node.startswith("filter:") for node in second.recomputed_nodes))
        self.assertTrue(any(node.startswith("error:") for node in second.recomputed_nodes))
        self.assertFalse(any(node.startswith("smooth:") for node in second.recomputed_nodes))
        self.assertFalse(any(node.startswith("spectral:") for node in second.recomputed_nodes))
        for before, after in zip(first.channels, second.channels, strict=True):
            self.assertEqual(before.smoothing.power_db, after.smoothing.power_db)

    def test_stale_parent_hash_is_rejected(self) -> None:
        self.engine.submit(self.params)
        with self.assertRaises(CalibrationError) as context:
            self.engine.submit(replace(self.params, error_threshold_db=4.0), expected_latest_hash="stale")
        self.assertEqual(context.exception.code, E_VERSION_STALE)
        self.assertEqual(self.engine.current_state().version, 1)

    def test_corrupted_published_content_hash_is_rejected(self) -> None:
        self.engine.submit(self.params)
        path = self.engine.state_store.version_path(1)
        payload = json.loads(path.read_text(encoding="utf-8"))
        payload["params"]["error_threshold_db"] = 1.0
        os.chmod(path, 0o644)
        path.write_text(json.dumps(payload), encoding="utf-8")
        with self.assertRaises(CalibrationError) as context:
            self.engine.state_store.load(1)
        self.assertEqual(context.exception.code, E_VERSION_STALE)

    def test_busy_and_cooperative_cancel_keep_last_version(self) -> None:
        first = self.engine.submit(self.params)
        entered = threading.Event()
        release = threading.Event()

        def progress(event: dict[str, object]) -> None:
            if event.get("stage") == "channel_complete" and event.get("completed") == 1:
                entered.set()
                release.wait(5.0)

        task = self.engine.start_async(
            replace(self.params, smoothing_fraction=0.25),
            progress=progress,
        )
        self.assertTrue(entered.wait(10.0))
        with self.assertRaises(CalibrationError) as context:
            self.engine.start_async(replace(self.params, error_threshold_db=2.0))
        self.assertEqual(context.exception.code, E_BUSY)
        self.assertTrue(self.engine.cancel())
        release.set()
        with self.assertRaises(CalibrationError) as cancelled:
            task.wait(15.0)
        self.assertEqual(cancelled.exception.code, E_CANCELLED)
        current = self.engine.current_state()
        self.assertEqual(current.version, first.version)
        self.assertEqual(current.content_hash, first.content_hash)

    def test_queries_never_observe_partial_publication(self) -> None:
        first = self.engine.submit(self.params)
        observed: list[tuple[int, int, int]] = []
        errors: list[BaseException] = []
        stop = threading.Event()

        def reader() -> None:
            try:
                while not stop.is_set():
                    state = self.engine.current_state()
                    if state is not None:
                        observed.append((state.version, len(state.channels), len(state.bands)))
            except BaseException as exc:  # pragma: no cover - assertion path
                errors.append(exc)

        thread = threading.Thread(target=reader)
        thread.start()
        try:
            second = self.engine.submit(replace(self.params, smoothing_fraction=0.2))
        finally:
            stop.set()
            thread.join(5.0)
        self.assertFalse(errors)
        self.assertTrue(observed)
        self.assertTrue(all(item[1:] == (6, 24) for item in observed))
        self.assertTrue(all(item[0] in (first.version, second.version) for item in observed))


if __name__ == "__main__":
    unittest.main()
