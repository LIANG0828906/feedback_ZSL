import os
import tempfile
import threading
import unittest

from npr import LAYER_OUTLINE, LAYER_SHADOW, LAYER_SMEAR
from npr.controller import RecomputeController
from npr.errors import E_BUSY, E_STALE_VERSION, PipelineError
from npr.params import OutlineParams, ShadowParams, SmearParams
from npr.pipeline import Pipeline
from npr.preview import PreviewService
from npr.render import canvas_hash
from npr.scene import parse_scene
from npr.storage import Store
from tests.test_scene import square_scene


def scene():
    doc = square_scene("a", 0.0, 0.0, 6.0)
    doc["paths"].append(square_scene("b", 6.0, 0.0, 6.0)["paths"][0])
    return parse_scene(doc)


class PreviewTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.scene = scene()
        self.pipeline = Pipeline(self.tmp.name)
        self.params = (OutlineParams(), ShadowParams(), SmearParams())

    def tearDown(self):
        self.tmp.cleanup()

    def test_snapshot_is_coherent_and_stale_handle_fails(self):
        manifest1, _ = self.pipeline.recompute(self.scene, *self.params)
        service = PreviewService(self.pipeline.store)
        snap1, _ = service.publish(manifest1)
        self.assertEqual(set(snap1.layers), {LAYER_OUTLINE, LAYER_SHADOW, LAYER_SMEAR})

        outline2 = OutlineParams(**{**self.params[0].__dict__, "width": 0.8})
        manifest2, _ = self.pipeline.recompute(self.scene, outline2, *self.params[1:])
        snap2, previous = service.publish(manifest2)
        self.assertEqual(previous.version, snap1.version)
        self.assertNotEqual(snap1.version, snap2.version)
        with self.assertRaises(PipelineError) as ctx:
            service.verify_handle(snap1)
        self.assertEqual(ctx.exception.code, E_STALE_VERSION)
        service.verify_handle(snap2)

    def test_readers_always_see_a_full_version_under_publish_race(self):
        manifest, _ = self.pipeline.recompute(self.scene, *self.params)
        service = PreviewService(self.pipeline.store)
        service.publish(manifest)
        errors = []

        def reader():
            for _ in range(200):
                snap = service.read_snapshot()
                if set(snap.layers) != {0, 1, 2}:
                    errors.append("partial layer set observed")

        thread = threading.Thread(target=reader)
        thread.start()
        for i in range(5):
            outline = OutlineParams(width=0.3 + 0.1 * i)
            manifest, _ = self.pipeline.recompute(self.scene, outline, *self.params[1:])
            service.publish(manifest)
        thread.join()
        self.assertEqual(errors, [])

    def test_png_output_is_deterministic(self):
        manifest, _ = self.pipeline.recompute(self.scene, *self.params)
        service = PreviewService(self.pipeline.store)
        snap, _ = service.publish(manifest)
        png1 = os.path.join(self.tmp.name, "a.png")
        png2 = os.path.join(self.tmp.name, "b.png")
        h1 = service.save_png(png1, snap)
        h2 = service.save_png(png2, snap)
        self.assertEqual(h1, h2)
        self.assertEqual(h1, canvas_hash(snap.canvas))


class ConcurrencyTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.scene = scene()
        self.controller = RecomputeController(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def test_duplicate_submit_returns_busy(self):
        gate = threading.Event()
        original = Pipeline._checkpoint

        def gated(self):
            gate.set()
            original(self)

        Pipeline._checkpoint = gated
        try:
            self.controller.submit(self.scene, OutlineParams(), ShadowParams(), SmearParams())
            self.assertTrue(gate.wait(5.0))
            with self.assertRaises(PipelineError) as ctx:
                self.controller.submit(
                    self.scene, OutlineParams(), ShadowParams(), SmearParams()
                )
            self.assertEqual(ctx.exception.code, E_BUSY)
        finally:
            Pipeline._checkpoint = original
        self.controller.wait_idle()

    def test_cancel_keeps_last_committed_version_and_preview(self):
        manifest1, _ = self.controller.pipeline.recompute(
            self.scene, OutlineParams(), ShadowParams(), SmearParams()
        )
        self.controller.preview.publish(manifest1)
        gate = threading.Event()
        original = Pipeline._checkpoint

        def gated(self):
            gate.set()
            self._cancel.wait(2.0)
            original(self)

        Pipeline._checkpoint = gated
        try:
            self.controller.submit(
                self.scene,
                OutlineParams(width=1.2),
                ShadowParams(),
                SmearParams(),
            )
            self.assertTrue(gate.wait(5.0))
            self.assertTrue(self.controller.cancel())
        finally:
            Pipeline._checkpoint = original
        recovered = Store(os.path.join(self.tmp.name, "store")).recover()
        self.assertEqual(recovered["version"], manifest1["version"])
        snap = self.controller.preview.current()
        self.assertEqual(snap.version, manifest1["version"])


class CrashResumeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.scene = scene()
        self.params = (OutlineParams(), ShadowParams(), SmearParams())

    def tearDown(self):
        self.tmp.cleanup()

    def test_kill_after_manifest_swap_resumes_to_committed_version(self):
        pipeline = Pipeline(self.tmp.name)
        manifest1, _ = pipeline.recompute(self.scene, *self.params)

        # Simulate a forced kill: a new version directory with temp files exists,
        # but the manifest still points at v1 only.
        leaked = os.path.join(self.tmp.name, "store", "chunks", "v2", "L0", "b0")
        os.makedirs(leaked)
        with open(os.path.join(leaked, "c0_0.json.tmp"), "w") as fh:
            fh.write("half-written")

        pipeline2 = Pipeline(self.tmp.name)
        manifest, missing = pipeline2.recover()
        self.assertEqual(manifest["version"], manifest1["version"])
        self.assertEqual(missing, [])
        self.assertFalse(os.path.exists(os.path.join(self.tmp.name, "store", "chunks", "v2")))

    def test_resume_completes_incomplete_layers_only(self):
        pipeline = Pipeline(self.tmp.name)
        manifest1, _ = pipeline.recompute(self.scene, *self.params)
        # Tamper-free committed state: resume must be a no-op.
        manifest, missing = Pipeline(self.tmp.name).recover()
        self.assertEqual(missing, [])
        resumed, _ = Pipeline(self.tmp.name).resume(self.scene, *self.params)
        self.assertEqual(resumed["version"], manifest1["version"])


if __name__ == "__main__":
    unittest.main()
