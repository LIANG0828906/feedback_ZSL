import os
import tempfile
import unittest

from npr.canonical import read_json
from npr.params import OutlineParams, ShadowParams, SmearParams
from npr.pipeline import Pipeline
from npr.scene import parse_scene
from tests.test_scene import square_scene


def scene():
    doc = square_scene("a", 0.0, 0.0, 6.0)
    doc["paths"].append(square_scene("b", 6.0, 0.0, 6.0)["paths"][0])
    return parse_scene(doc)


class HashChainTests(unittest.TestCase):
    def test_every_version_links_to_immutable_parent_snapshot(self):
        with tempfile.TemporaryDirectory() as workdir:
            pipeline = Pipeline(workdir)
            sc = scene()
            m1, _ = pipeline.recompute(sc, OutlineParams(), ShadowParams(), SmearParams())
            m2, _ = pipeline.recompute(
                sc, OutlineParams(width=0.5), ShadowParams(), SmearParams()
            )
            m3, _ = pipeline.recompute(
                sc, OutlineParams(width=0.5), ShadowParams(spacing=1.0), SmearParams()
            )
            self.assertEqual((m1["version"], m2["version"], m3["version"]), (1, 2, 3))

            store = pipeline.store
            current = store.load_manifest()
            chain = []
            while True:
                chain.append(current)
                if current["parent_version"] is None:
                    break
                parent = read_json(
                    store._historical_manifest_path(current["parent_version"])
                )
                self.assertEqual(parent["manifest_hash"], current["parent_hash"])
                current = parent
            self.assertEqual([m["version"] for m in chain], [3, 2, 1])

            # Recovery walks the full chain and validates every chunk.
            recovered = store.recover()
            self.assertEqual(recovered["manifest_hash"], m3["manifest_hash"])

            # Deleting a historical manifest breaks the chain explicitly.
            os.unlink(store._historical_manifest_path(2))
            from npr.errors import E_RECOVERY_INVALID, PipelineError

            with self.assertRaises(PipelineError) as ctx:
                store.recover()
            self.assertEqual(ctx.exception.code, E_RECOVERY_INVALID)


if __name__ == "__main__":
    unittest.main()
