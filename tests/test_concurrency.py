from __future__ import annotations

import threading
import time
import tempfile
import unittest

from surface_viewer.concurrency import render_fixed_fps
from surface_viewer.engine import IncrementalSurfaceEngine, ResourceLimits
from surface_viewer.errors import ErrorCode, SurfaceViewerError
from surface_viewer.model import parse_assembly
from surface_viewer.runtime import SurfaceRuntime
from surface_viewer.samplegen import (
    CASE_SPECS,
    generate_case,
    operation_changes,
    operations_for_case,
)


class ConcurrencyTests(unittest.TestCase):
    def test_snapshot_replacement_never_exposes_partial_state(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            assembly = parse_assembly(generate_case(CASE_SPECS[1]))
            runtime = SurfaceRuntime.bootstrap(
                assembly, temporary, verify_full=False
            )
            stop = threading.Event()
            observed: list[tuple[int, int]] = []
            errors: list[BaseException] = []

            def draw(handle) -> None:
                snapshot = handle.snapshot
                observed.append(
                    (
                        len(snapshot.artifacts),
                        len(snapshot.assembly.patches) * 4,
                    )
                )
                self.assertEqual(
                    snapshot.screen_preview.content_hash,
                    handle.read_screen().content_hash,
                )

            def renderer() -> None:
                try:
                    stats = render_fixed_fps(
                        runtime.snapshots,
                        draw,
                        fps=120,
                        stop_event=stop,
                        max_frames=80,
                    )
                    self.assertEqual(stats.errors, ())
                except BaseException as exc:
                    errors.append(exc)

            thread = threading.Thread(target=renderer)
            thread.start()
            for operation in operations_for_case(CASE_SPECS[1]):
                patch_id, changes = operation_changes(
                    runtime.engine.assembly, operation
                )
                runtime.update(patch_id, changes, verify_full=False)
            stop.set()
            thread.join(timeout=10)
            self.assertFalse(thread.is_alive())
            self.assertEqual(errors, [])
            self.assertTrue(observed)
            for artifact_count, expected_count in observed:
                self.assertEqual(artifact_count, expected_count)

    def test_stale_handle_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            assembly = parse_assembly(generate_case(CASE_SPECS[0]))
            runtime = SurfaceRuntime.bootstrap(
                assembly, temporary, verify_full=False
            )
            stale = runtime.current()
            patch_id, changes = operation_changes(
                runtime.engine.assembly, operations_for_case(CASE_SPECS[0])[0]
            )
            runtime.update(patch_id, changes, verify_full=False)
            with self.assertRaises(SurfaceViewerError) as raised:
                stale.read_screen()
            self.assertEqual(raised.exception.code, ErrorCode.STALE_HANDLE)

    def test_duplicate_recompute_submission_returns_busy(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            assembly = parse_assembly(generate_case(CASE_SPECS[0]))
            runtime = SurfaceRuntime.bootstrap(
                assembly, temporary, verify_full=False
            )
            started = threading.Event()
            release = threading.Event()
            original = runtime.engine.update

            def slow_update(*args, **kwargs):
                started.set()
                release.wait(5)
                return original(*args, **kwargs)

            runtime.engine.update = slow_update  # type: ignore[method-assign]
            patch_id, changes = operation_changes(
                runtime.engine.assembly, operations_for_case(CASE_SPECS[0])[0]
            )
            task = runtime.submit_update(
                patch_id, changes, verify_full=False
            )
            self.assertTrue(started.wait(5))
            with self.assertRaises(SurfaceViewerError) as raised:
                runtime.submit_update(
                    patch_id, changes, verify_full=False
                )
            self.assertEqual(raised.exception.code, ErrorCode.BUSY)
            release.set()
            task.result(timeout=20)

    def test_vertex_resource_limit_is_enforced(self) -> None:
        assembly = parse_assembly(generate_case(CASE_SPECS[0]))
        with self.assertRaises(SurfaceViewerError) as raised:
            IncrementalSurfaceEngine(
                assembly,
                resource_limits=ResourceLimits(max_vertices_per_patch=20),
            )
        self.assertEqual(raised.exception.code, ErrorCode.RESOURCE_LIMIT)
        self.assertIn("precision_level", raised.exception.field)

    def test_preview_resource_limit_is_enforced(self) -> None:
        assembly = parse_assembly(generate_case(CASE_SPECS[0]))
        with self.assertRaises(SurfaceViewerError) as raised:
            IncrementalSurfaceEngine(
                assembly,
                resource_limits=ResourceLimits(max_preview_bytes=16),
            )
        self.assertEqual(raised.exception.code, ErrorCode.RESOURCE_LIMIT)
        self.assertEqual(raised.exception.field, "screen_preview")


if __name__ == "__main__":
    unittest.main()
