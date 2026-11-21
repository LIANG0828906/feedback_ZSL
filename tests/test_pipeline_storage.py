import glob
import json
import os
import tempfile
import unittest

from npr import LAYER_OUTLINE, LAYER_SHADOW, LAYER_SMEAR
from npr.canonical import canonical_bytes, read_json
from npr.errors import E_COMMIT_FAILED, E_RECOVERY_INVALID, PipelineError
from npr.params import OutlineParams, ShadowParams, SmearParams
from npr.pipeline import Pipeline, plan_invalidation
from npr.scene import parse_scene
from npr.storage import Store, route_strokes
from tests.test_scene import square_scene


def scene():
    doc = square_scene("a", 0.0, 0.0, 6.0)
    doc["paths"].append(square_scene("b", 6.0, 0.0, 6.0)["paths"][0])
    return parse_scene(doc)


class RoutingTests(unittest.TestCase):
    def test_routes_by_fixed_grid_and_batches(self):
        strokes = [
            {"id": 0, "x": 1.0, "y": 2.0},
            {"id": 47, "x": 99.0, "y": -5.0},
            {"id": 48, "x": 1.0, "y": 2.0},
        ]
        batches = route_strokes(strokes)
        self.assertEqual(set(batches), {0, 1})
        self.assertEqual(list(batches[0][(0, 0)]), [strokes[0]])
        self.assertEqual(list(batches[0][(9, -1)]), [strokes[1]])
        self.assertEqual(list(batches[1][(0, 0)]), [strokes[2]])


class InvalidationTests(unittest.TestCase):
    def _factors(self, variant=0):
        return {
            "scene": f"s{variant}",
            "outline": f"o{variant}",
            "shadow": f"h{variant}",
            "smear": f"m{variant}",
        }

    def test_outline_change_invalidates_all_via_culling_closure(self):
        old = self._factors()
        new = self._factors()
        new["outline"] = "ox"
        affected, reason = plan_invalidation(old, new)
        self.assertEqual(affected, {LAYER_OUTLINE, LAYER_SHADOW, LAYER_SMEAR})
        self.assertIn("culling closure", reason)

    def test_shadow_change_invalidates_shadow_and_smear(self):
        old = self._factors()
        new = self._factors()
        new["shadow"] = "hx"
        affected, _ = plan_invalidation(old, new)
        self.assertEqual(affected, {LAYER_SHADOW, LAYER_SMEAR})

    def test_smear_only_change_invalidates_smear(self):
        old = self._factors()
        new = self._factors()
        new["smear"] = "mx"
        affected, _ = plan_invalidation(old, new)
        self.assertEqual(affected, {LAYER_SMEAR})

    def test_unknown_factor_change_is_conservative(self):
        old = self._factors()
        new = dict(old)
        del new["smear"]
        affected, _ = plan_invalidation(old, new)
        self.assertEqual(affected, {LAYER_OUTLINE, LAYER_SHADOW, LAYER_SMEAR})


class CommitAndRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.workdir = self.tmp.name
        self.scene = scene()
        self.pipeline = Pipeline(self.workdir)
        self.outline_p = OutlineParams()
        self.shadow_p = ShadowParams()
        self.smear_p = SmearParams()

    def tearDown(self):
        self.tmp.cleanup()

    def test_full_commit_hashes_chunks_and_manifest(self):
        manifest, stats = self.pipeline.recompute(
            self.scene, self.outline_p, self.shadow_p, self.smear_p
        )
        self.assertEqual(manifest["version"], 1)
        self.assertEqual({e["layer"] for e in manifest["entries"]}, {0, 1, 2})
        chunk_files = glob.glob(
            os.path.join(self.workdir, "store", "chunks", "v1", "**", "*.json"),
            recursive=True,
        )
        self.assertTrue(chunk_files)
        for path in chunk_files:
            self.assertFalse(os.path.exists(path + ".tmp"))
        reloaded = Store(os.path.join(self.workdir, "store")).recover()
        self.assertEqual(reloaded["manifest_hash"], manifest["manifest_hash"])

    def test_noop_recompute_reuses_every_batch(self):
        self.pipeline.recompute(self.scene, self.outline_p, self.shadow_p, self.smear_p)
        _, stats = self.pipeline.recompute(
            self.scene, self.outline_p, self.shadow_p, self.smear_p
        )
        for record in stats:
            self.assertEqual(record["recomputed_batches"], 0)
            self.assertGreater(record["reused_batches"], 0)

    def test_shadow_param_change_reuses_outline_batches(self):
        self.pipeline.recompute(self.scene, self.outline_p, self.shadow_p, self.smear_p)
        shadow2 = ShadowParams(**{**self.shadow_p.__dict__, "spacing": 1.0})
        _, stats = self.pipeline.recompute(
            self.scene, self.outline_p, shadow2, self.smear_p
        )
        by_layer = {r["layer"]: r for r in stats}
        self.assertEqual(by_layer[LAYER_OUTLINE]["recomputed_batches"], 0)
        self.assertGreater(by_layer[LAYER_SHADOW]["recomputed_batches"], 0)
        self.assertGreater(by_layer[LAYER_SMEAR]["recomputed_batches"], 0)

    def test_commit_failure_keeps_previous_version(self):
        manifest1, _ = self.pipeline.recompute(
            self.scene, self.outline_p, self.shadow_p, self.smear_p
        )
        original = Store.write_chunk_file
        failures = {"n": 0}

        def flaky(self_obj, path, strokes, layer, batch_id, chunk, factor_version):
            failures["n"] += 1
            if failures["n"] == 1:
                raise OSError("simulated disk fault")
            return original(self_obj, path, strokes, layer, batch_id, chunk, factor_version)

        Store.write_chunk_file = flaky
        try:
            with self.assertRaises(PipelineError) as ctx:
                self.pipeline.recompute(
                    self.scene,
                    OutlineParams(**{**self.outline_p.__dict__, "width": 0.9}),
                    self.shadow_p,
                    self.smear_p,
                )
            self.assertEqual(ctx.exception.code, E_COMMIT_FAILED)
        finally:
            Store.write_chunk_file = original
        recovered = Store(os.path.join(self.workdir, "store")).recover()
        self.assertEqual(recovered["version"], manifest1["version"])
        self.assertFalse(
            glob.glob(
                os.path.join(self.workdir, "store", "chunks", "**", "*.tmp"),
                recursive=True,
            )
        )

    def test_uncommitted_version_dir_is_pruned_on_recovery(self):
        manifest, _ = self.pipeline.recompute(
            self.scene, self.outline_p, self.shadow_p, self.smear_p
        )
        leaked = os.path.join(
            self.workdir, "store", "chunks", "v99", "L0", "b0"
        )
        os.makedirs(leaked)
        with open(os.path.join(leaked, "c0_0.json.tmp"), "w") as fh:
            fh.write("partial")
        recovered = Store(os.path.join(self.workdir, "store")).recover()
        self.assertEqual(recovered["version"], manifest["version"])
        self.assertFalse(os.path.exists(os.path.join(self.workdir, "store", "chunks", "v99")))

    def test_broken_hash_chain_aborts_with_layer_location(self):
        self.pipeline.recompute(
            self.scene, self.outline_p, self.shadow_p, self.smear_p
        )
        entry = self.pipeline.store.load_manifest()["entries"][0]
        path = os.path.join(
            self.workdir,
            "store",
            "chunks",
            "v1",
            f"L{entry['layer']}",
            f"b{entry['batch']}",
            f"c{entry['chunk'][0]}_{entry['chunk'][1]}.json",
        )
        doc = read_json(path)
        doc["strokes"][0]["width"] = 999.0
        with open(path, "w") as fh:
            fh.write(canonical_bytes(doc).decode("utf-8"))
        with self.assertRaises(PipelineError) as ctx:
            Store(os.path.join(self.workdir, "store")).recover()
        self.assertEqual(ctx.exception.code, E_RECOVERY_INVALID)
        self.assertIn(f"layer={entry['layer']}", str(ctx.exception))

    def test_manifest_tamper_detected(self):
        self.pipeline.recompute(
            self.scene, self.outline_p, self.shadow_p, self.smear_p
        )
        manifest_path = os.path.join(self.workdir, "store", "manifest.json")
        manifest = read_json(manifest_path)
        manifest["version"] = 42
        from npr.canonical import write_json_atomic

        write_json_atomic(manifest_path, manifest)
        with self.assertRaises(PipelineError) as ctx:
            Store(os.path.join(self.workdir, "store")).recover()
        self.assertEqual(ctx.exception.code, E_RECOVERY_INVALID)

    def test_trace_log_is_deterministic_and_contains_required_fields(self):
        self.pipeline.recompute(self.scene, self.outline_p, self.shadow_p, self.smear_p)
        shadow2 = ShadowParams(**{**self.shadow_p.__dict__, "spacing": 1.1})
        self.pipeline.recompute(
            self.scene, self.outline_p, shadow2, self.smear_p
        )
        with open(os.path.join(self.workdir, "trace.jsonl")) as fh:
            lines = [line.rstrip("\n") for line in fh if line.strip()]
        self.assertEqual(len(lines), 6)
        required = {
            "version",
            "layer",
            "affected",
            "recomputed_batches",
            "reused_batches",
            "chunk_count",
            "culled_count",
            "layer_hash",
        }
        for line in lines:
            _tag, payload = line.split(" ", 1)
            record = json.loads(payload)
            self.assertTrue(required.issubset(record))


if __name__ == "__main__":
    unittest.main()
