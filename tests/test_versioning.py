from __future__ import annotations

import threading
import unittest

from tactical_pick.errors import (
    E_SCENE_INVALID,
    E_STALE_HANDLE,
    E_VERSION_STALE,
    PickError,
)
from tactical_pick.picking import pick_from_handle, pick_from_store
from tactical_pick.versioning import VersionStore

from tests.helpers import center_camera, simple_scene


class VersioningTests(unittest.TestCase):
    def test_batch_is_atomic_and_monotonic(self) -> None:
        store = VersionStore(simple_scene())
        original = store.current_snapshot()
        with self.assertRaises(PickError) as caught:
            store.apply_batch(
                [
                    {
                        "op": "translate_unit",
                        "unit_id": 10,
                        "delta": [0.2, 0.0, 0.0],
                    },
                    {
                        "op": "set_unit_bounds",
                        "unit_id": 10,
                        "minimum": [0.0, 0.0, 0.0],
                        "maximum": [0.0, 0.0, 0.0],
                    },
                ]
            )
        self.assertEqual(caught.exception.code, E_SCENE_INVALID)
        self.assertEqual(caught.exception.location["operation_index"], 1)
        self.assertEqual(store.current_version, original.version)
        self.assertEqual(store.current_hash, original.content_hash)
        self.assertIs(store.current_snapshot(), original)

        published = store.apply_batch(
            [
                {
                    "op": "translate_unit",
                    "unit_id": 10,
                    "delta": [0.2, 0.1, 0.0],
                },
                {
                    "op": "set_walkable_cells",
                    "cells": [{"row": 0, "col": 1, "walkable": False}],
                },
            ]
        )
        self.assertEqual(published.version, 2)
        self.assertNotEqual(published.content_hash, original.content_hash)
        self.assertEqual(store.versions(), (1, 2))

    def test_expected_version_and_stale_handle(self) -> None:
        store = VersionStore(simple_scene())
        camera = center_camera()
        old_handle = store.acquire(expected_version=1)
        old_result = pick_from_handle(old_handle, camera, [400, 300])
        store.apply_batch(
            [{"op": "translate_unit", "unit_id": 10, "delta": [0.1, 0.0, 0.0]}]
        )
        still_old = pick_from_handle(old_handle, camera, [400, 300])
        self.assertEqual(still_old.version, 1)
        self.assertEqual(still_old.content_hash, old_result.content_hash)
        with self.assertRaises(PickError) as caught:
            store.acquire(expected_version=1)
        self.assertEqual(caught.exception.code, E_VERSION_STALE)
        old_handle.release()
        with self.assertRaises(PickError) as caught:
            pick_from_handle(old_handle, camera, [400, 300])
        self.assertEqual(caught.exception.code, E_STALE_HANDLE)
        new_result = pick_from_store(store, camera, [400, 300], expected_version=2)
        self.assertEqual(new_result.version, 2)

    def test_query_threads_observe_only_complete_published_snapshots(self) -> None:
        scene = simple_scene()
        store = VersionStore(scene)
        camera = center_camera()
        start = threading.Event()
        done = threading.Event()
        results: list[tuple[int, str]] = []
        errors: list[BaseException] = []
        lock = threading.Lock()

        def query_worker() -> None:
            start.wait()
            try:
                for _ in range(40):
                    result = pick_from_store(store, camera, [400, 300])
                    with lock:
                        results.append((result.version, result.content_hash))
            except BaseException as exc:  # pragma: no cover - assertion path
                errors.append(exc)
            finally:
                done.set()

        threads = [threading.Thread(target=query_worker) for _ in range(3)]
        for thread in threads:
            thread.start()
        start.set()
        for index in range(20):
            store.apply_batch(
                [
                    {
                        "op": "translate_unit",
                        "unit_id": 10,
                        "delta": [0.01, 0.0, 0.0],
                    }
                ]
            )
        for thread in threads:
            thread.join()
        self.assertFalse(errors)
        history = {snapshot.version: snapshot.content_hash for snapshot in store.history()}
        for version, content_hash in results:
            self.assertEqual(history[version], content_hash)
        self.assertEqual(store.current_version, 21)
        self.assertTrue(all(1 <= version <= 21 for version, _ in results))

    def test_concurrent_batch_conflict_does_not_publish(self) -> None:
        store = VersionStore(simple_scene())
        store.apply_batch(
            [{"op": "translate_unit", "unit_id": 10, "delta": [0.1, 0.0, 0.0]}]
        )
        with self.assertRaises(PickError) as caught:
            store.apply_batch(
                [{"op": "translate_unit", "unit_id": 10, "delta": [0.1, 0.0, 0.0]}],
                expected_version=1,
            )
        self.assertEqual(caught.exception.code, E_VERSION_STALE)
        self.assertEqual(store.current_version, 2)

    def test_malformed_batch_is_scene_invalid_without_publication(self) -> None:
        store = VersionStore(simple_scene())
        with self.assertRaises(PickError) as caught:
            store.apply_batch(
                [
                    {
                        "op": "replace_units",
                        "units": [
                            {
                                "unit_id": 99,
                                "bounds": {
                                    "kind": "aabb",
                                    "minimum": [0.0],
                                    "maximum": [1.0],
                                },
                            }
                        ],
                    }
                ]
            )
        self.assertEqual(caught.exception.code, E_SCENE_INVALID)
        self.assertEqual(caught.exception.location["operation_index"], 0)
        self.assertEqual(store.current_version, 1)


if __name__ == "__main__":
    unittest.main()
