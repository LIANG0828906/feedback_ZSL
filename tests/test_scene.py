import os
import tempfile
import unittest

from npr.canonical import read_json, write_json_atomic
from npr.errors import E_SCENE_INVALID, PipelineError
from npr.scene import load_scene, parse_scene


def square_scene(zone="z1", x0=0.0, y0=0.0, size=4.0):
    return {
        "format": "npr-scene/1",
        "paths": [
            {
                "zone": zone,
                "order": 0,
                "closed": True,
                "fill": [10, 20, 30],
                "segments": [
                    {"type": "line", "p0": [x0, y0], "p1": [x0 + size, y0]},
                    {"type": "line", "p0": [x0 + size, y0], "p1": [x0 + size, y0 + size]},
                    {"type": "line", "p0": [x0 + size, y0 + size], "p1": [x0, y0 + size]},
                    {"type": "line", "p0": [x0, y0 + size], "p1": [x0, y0]},
                ],
            }
        ],
    }


class SceneValidationTests(unittest.TestCase):
    def test_parses_closed_path_and_builds_zone_index(self):
        scene = parse_scene(square_scene())
        self.assertEqual(scene.zone_index, {"z1": [0]})
        self.assertEqual(scene.bbox, [0.0, 0.0, 4.0, 4.0])
        self.assertEqual(len(scene.scene_hash), 64)

    def test_quadratic_segments_are_flattened(self):
        doc = square_scene()
        doc["paths"][0]["segments"][0] = {
            "type": "quad",
            "p0": [0, 0],
            "c": [2, -1],
            "p1": [4, 0],
        }
        scene = parse_scene(doc)
        # QUAD_SUBDIV interior samples are added to the flattened ring.
        self.assertGreater(len(scene.paths[0].points), 5)

    def test_missing_field_reports_path_number(self):
        doc = square_scene()
        del doc["paths"][0]["zone"]
        with self.assertRaises(PipelineError) as ctx:
            parse_scene(doc)
        self.assertEqual(ctx.exception.code, E_SCENE_INVALID)
        self.assertIn("path#0", str(ctx.exception))

    def test_unclosed_path_is_rejected(self):
        doc = square_scene()
        doc["paths"][0]["segments"].pop()
        with self.assertRaises(PipelineError) as ctx:
            parse_scene(doc)
        self.assertEqual(ctx.exception.code, E_SCENE_INVALID)
        self.assertIn("closed", ctx.exception.message)

    def test_disconnected_segment_chain_is_rejected(self):
        doc = square_scene()
        doc["paths"][0]["segments"][1]["p0"] = [5.0, 0.0]
        with self.assertRaises(PipelineError) as ctx:
            parse_scene(doc)
        self.assertEqual(ctx.exception.code, E_SCENE_INVALID)

    def test_non_finite_coordinates_rejected(self):
        doc = square_scene()
        doc["paths"][0]["segments"][0]["p1"] = [float("nan"), 0.0]
        with self.assertRaises(PipelineError):
            parse_scene(doc)

    def test_overlapping_zones_rejected(self):
        doc = square_scene("a")
        doc["paths"].append(square_scene("b", x0=2.0, y0=2.0, size=4.0)["paths"][0])
        with self.assertRaises(PipelineError) as ctx:
            parse_scene(doc)
        self.assertEqual(ctx.exception.code, E_SCENE_INVALID)
        self.assertIn("overlap", ctx.exception.message)

    def test_touching_zones_sharing_edge_are_allowed(self):
        doc = square_scene("a")
        doc["paths"].append(square_scene("b", x0=4.0, y0=0.0, size=4.0)["paths"][0])
        scene = parse_scene(doc)
        self.assertEqual(sorted(scene.zone_index), ["a", "b"])

    def test_roundtrip_via_disk(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "scene.json")
            write_json_atomic(path, square_scene())
            scene = load_scene(path)
            self.assertEqual(scene.zone_index, {"z1": [0]})
            self.assertEqual(read_json(path)["format"], "npr-scene/1")


if __name__ == "__main__":
    unittest.main()
