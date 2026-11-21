from __future__ import annotations

import unittest

from surface_viewer.engine import IncrementalSurfaceEngine
from surface_viewer.model import parse_assembly
from surface_viewer.preview import OpenGLOffscreenPreviewRenderer
from surface_viewer.samplegen import (
    CASE_SPECS,
    generate_case,
    operation_changes,
    operations_for_case,
)


@unittest.skipUnless(
    OpenGLOffscreenPreviewRenderer.available(),
    "EGL surfaceless OpenGL is unavailable",
)
class OpenGLPreviewTests(unittest.TestCase):
    def test_offscreen_open_gl_matches_full_rerender(self) -> None:
        assembly = parse_assembly(generate_case(CASE_SPECS[0]))
        engine = IncrementalSurfaceEngine(
            assembly, preview_backend="opengl"
        )
        engine.initialize(verify_full=True)
        operation = operations_for_case(CASE_SPECS[0])[2]
        patch_id, changes = operation_changes(engine.assembly, operation)
        update = engine.update(patch_id, changes, verify_full=True)
        self.assertTrue(update.verification["byte_exact"])
        self.assertEqual(update.affected_patches, ("p0000",))


if __name__ == "__main__":
    unittest.main()
