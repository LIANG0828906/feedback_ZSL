from __future__ import annotations

import json
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

import numpy as np

from volrender import (
    E_BUSY,
    E_SNAPSHOT_INVALID,
    E_SNAPSHOT_STALE,
    RenderSession,
    VolumeRenderer,
    VolRenderError,
    load_snapshot,
    parse_snapshot,
)
from volrender.camera import look_at_view

from tests.helpers import synthetic_transfer, synthetic_volume


ROOT = Path(__file__).resolve().parents[1]


def make_renderer(volume=None) -> VolumeRenderer:
    return VolumeRenderer(
        volume=volume if volume is not None else synthetic_volume(20),
        transfer=synthetic_transfer(),
        image_size=(28, 28),
        error_threshold=1.0e-3,
    )


class SnapshotTests(unittest.TestCase):
    def test_state_hash_covers_lut_view_step_and_mode(self) -> None:
        renderer = make_renderer()
        hashes = [renderer.state_hash()]
        renderer.set_view_matrix(look_at_view((80.0, -70.0, 60.0)))
        hashes.append(renderer.state_hash())
        renderer.set_step(1.5)
        hashes.append(renderer.state_hash())
        renderer.set_transfer_points(
            [
                (0.0, 0.0, 0.0, 0.0, 0.0),
                (0.5, 0.3, 0.4, 0.7, 0.9),
                (1.0, 0.9, 1.0, 1.0, 1.0),
            ]
        )
        hashes.append(renderer.state_hash())
        renderer.set_precision("fixed")
        hashes.append(renderer.state_hash())
        self.assertEqual(len(hashes), len(set(hashes)))

    def test_snapshot_roundtrip_restore_and_resume(self) -> None:
        renderer = make_renderer()
        original = renderer.render()
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "render.volsnap.json"
            renderer.save_snapshot(path)
            snapshot = load_snapshot(path)
            self.assertEqual(snapshot.content_hash, original.content_hash)
            self.assertEqual(snapshot.state_hash, original.state_hash)
            np.testing.assert_array_equal(snapshot.rgba8, original.rgba8)
            np.testing.assert_allclose(
                snapshot.error_map,
                original.error_map.astype(np.float32).astype(np.float64),
            )

            renderer.set_step(2.0)
            renderer.restore_snapshot(snapshot)
            resumed = renderer.resume_render()
            self.assertEqual(resumed.content_hash, original.content_hash)
            self.assertEqual(resumed.state_hash, original.state_hash)

    def test_expected_state_hash_mismatch_is_stale(self) -> None:
        renderer = make_renderer()
        result = renderer.render()
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "render.volsnap.json"
            renderer.set_step(2.0)
            with self.assertRaises(VolRenderError) as raised:
                renderer.save_snapshot(
                    path,
                    result=result,
                    expected_state_hash=renderer.state_hash(),
                )
            self.assertEqual(raised.exception.code, E_SNAPSHOT_STALE)
            self.assertFalse(path.exists())

    def test_fixture_codes_match_scripted_expectations(self) -> None:
        cases = json.loads(
            (ROOT / "cases" / "cases.json").read_text(encoding="utf-8")
        )
        for case in cases["snapshot_cases"]:
            with self.subTest(case=case["name"]):
                path = ROOT / "cases" / case["file"]
                if case["expected_code"] == "OK":
                    snapshot = load_snapshot(path)
                    self.assertEqual(snapshot.content_hash, snapshot.raw["result"]["content_hash"])
                else:
                    with self.assertRaises(VolRenderError) as raised:
                        load_snapshot(path)
                    self.assertEqual(raised.exception.code, case["expected_code"])

    def test_corrupt_snapshot_does_not_partially_apply(self) -> None:
        renderer = make_renderer()
        renderer.render()
        state_before = renderer.state_hash()
        corrupt = (ROOT / "cases" / "snapshots" / "corrupt_missing_field.volsnap.json").read_bytes()
        with self.assertRaises(VolRenderError) as raised:
            parse_snapshot(corrupt)
        self.assertEqual(raised.exception.code, E_SNAPSHOT_INVALID)
        self.assertEqual(renderer.state_hash(), state_before)
        self.assertIsNotNone(renderer.last_good_result)

    def test_restore_from_other_volume_is_stale_and_non_mutating(self) -> None:
        source = make_renderer(synthetic_volume(18))
        source.render()
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "other.volsnap.json"
            source.save_snapshot(path)
            snapshot = load_snapshot(path)
        target = make_renderer(synthetic_volume(20))
        state_before = target.state_hash()
        view_before = target.view_matrix.copy()
        with self.assertRaises(VolRenderError) as raised:
            target.restore_snapshot(snapshot)
        self.assertEqual(raised.exception.code, E_SNAPSHOT_STALE)
        self.assertEqual(target.state_hash(), state_before)
        np.testing.assert_array_equal(target.view_matrix, view_before)

    def test_corrupt_json_is_invalid(self) -> None:
        with self.assertRaises(VolRenderError) as raised:
            parse_snapshot(b'{"schema":"volrender.snapshot","schema_version":1')
        self.assertEqual(raised.exception.code, E_SNAPSHOT_INVALID)

    def test_malformed_root_fields_are_invalid(self) -> None:
        malformed = [
            {},
            {"schema": "volrender.snapshot", "schema_version": "bad"},
            {
                "schema": "volrender.snapshot",
                "schema_version": 1,
                "state": [],
                "state_hash": "x",
                "result": {},
                "snapshot_hash": "x",
            },
        ]
        for document in malformed:
            with self.subTest(document=document):
                with self.assertRaises(VolRenderError) as raised:
                    parse_snapshot(document)
                self.assertEqual(raised.exception.code, E_SNAPSHOT_INVALID)


class SessionTests(unittest.TestCase):
    def test_duplicate_submission_returns_busy(self) -> None:
        renderer = make_renderer()
        session = RenderSession(renderer)
        entered = threading.Event()
        release = threading.Event()
        original_render = renderer.render

        def blocking_render(*args, **kwargs):
            entered.set()
            release.wait(5.0)
            return original_render(*args, **kwargs)

        errors: list[BaseException] = []

        def worker() -> None:
            try:
                session.submit_render()
            except BaseException as exc:  # noqa: BLE001
                errors.append(exc)

        with mock.patch.object(renderer, "render", side_effect=blocking_render):
            thread = threading.Thread(target=worker)
            thread.start()
            self.assertTrue(entered.wait(2.0))
            with self.assertRaises(VolRenderError) as raised:
                session.submit_render()
            self.assertEqual(raised.exception.code, E_BUSY)
            release.set()
            thread.join(5.0)
        self.assertFalse(thread.is_alive())
        self.assertEqual(errors, [])

    def test_error_releases_offscreen_and_keeps_last_good(self) -> None:
        renderer = make_renderer()
        session = RenderSession(renderer)
        good = session.submit_render()
        with mock.patch.object(
            renderer,
            "render",
            side_effect=VolRenderError("E_INPUT_INVALID", "forced failure"),
        ):
            with self.assertRaises(VolRenderError):
                session.submit_render()
        self.assertIsNone(renderer._offscreen_result)
        self.assertIs(renderer.last_good_result, good)
        self.assertIs(session.last_good_result, good)

    def test_session_is_reusable_after_error(self) -> None:
        renderer = make_renderer()
        session = RenderSession(renderer)
        with mock.patch.object(
            renderer,
            "render",
            side_effect=RuntimeError("forced"),
        ):
            with self.assertRaises(RuntimeError):
                session.submit_render()
        result = session.submit_render()
        self.assertIsNotNone(result)
        self.assertFalse(session.busy)


if __name__ == "__main__":
    unittest.main()
