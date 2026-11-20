import os
import tempfile
import unittest

import numpy as np

from pcwb.render import OrbitCamera, create_renderer
from pcwb.render.cpu_renderer import CPURenderer
from pcwb.render.headless_loop import run_headless_frames
from pcwb.engine import EditEngine
from pcwb.samples import generate_cloud
from pcwb.selection import sphere_select
from pcwb.telemetry import LatencyMonitor
from pcwb.versions import VersionStore


class RendererTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        store = VersionStore(os.path.join(self.tmp.name, "store"))
        xyz, rgb = generate_cloud(21, 1500)
        self.cloud = store.commit(xyz, rgb, record={"operator": "import"})
        self.store = store

    def tearDown(self):
        self.tmp.cleanup()

    def test_factory_returns_working_backend(self):
        renderer = create_renderer(160, 120)
        camera = OrbitCamera()
        camera.fit(self.cloud.bbox)
        result = renderer.render(self.cloud, camera)
        self.assertEqual(result.pixels.shape, (120, 160, 3))
        # background pixels exist and at least some point pixels differ
        unique_colors = np.unique(
            result.pixels.reshape(-1, 3), axis=0).shape[0]
        self.assertGreater(unique_colors, 1)
        out = os.path.join(self.tmp.name, "frame.ppm")
        result.write_ppm(out)
        self.assertTrue(os.path.exists(out))
        with open(out, "rb") as handle:
            self.assertTrue(handle.read(2) == b"P6")
        renderer.close()

    def test_egl_renderer_if_available(self):
        try:
            from pcwb.render.gl_renderer import EGLRenderer
        except Exception as exc:  # pragma: no cover
            self.skipTest(f"EGL unavailable: {exc}")
        renderer = EGLRenderer(160, 120)
        self.assertEqual(renderer.backend, "egl-opengl")
        camera = OrbitCamera()
        camera.fit(self.cloud.bbox)
        sel = sphere_select(self.cloud, (0, 0, 0), 2.5)
        result = renderer.render(
            self.cloud, camera, highlight_indices=sel.indices)
        self.assertEqual(result.pixels.shape, (120, 160, 3))
        # highlight color (255,215,0) must appear on screen
        flat = result.pixels.reshape(-1, 3).astype(np.int16)
        target = np.array([255, 215, 0], dtype=np.int16)
        hits = np.any(np.all(np.abs(flat - target) <= 2, axis=1))
        self.assertTrue(bool(hits), "selection highlight not rendered")
        renderer.close()

    def test_cpu_renderer_rotation_changes_image(self):
        renderer = CPURenderer(160, 120)
        camera = OrbitCamera()
        camera.fit(self.cloud.bbox)
        first = renderer.render(self.cloud, camera).pixels.copy()
        camera.rotate(0.7, 0.2)
        second = renderer.render(self.cloud, camera).pixels.copy()
        self.assertFalse(np.array_equal(first, second))

    def test_headless_loop_stays_responsive_during_edit(self):
        engine = EditEngine(self.store, self.cloud)
        renderer = CPURenderer(120, 90)
        monitor = LatencyMonitor()
        sel = sphere_select(self.cloud, (0, 0, 0), 2.0)
        engine.submit("smooth", sel,
                      {"radius": 0.5, "iterations": 20})
        report = run_headless_frames(
            engine, renderer, frames=12, monitor=monitor,
            inputs=[(i, ("rotate", 0.05, 0.02)) for i in range(12)])
        engine.wait(timeout=30)
        engine.shutdown()
        self.assertEqual(report["stats"]["frames"], 12)
        self.assertGreater(report["stats"]["input_latency_ms_max"], 0.0)
        self.assertTrue(engine.last_outcome.ok)
        renderer.close()


if __name__ == "__main__":
    unittest.main()
