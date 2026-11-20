from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from mesh_editor.formats import parse_mesh
from mesh_editor.precision import compare_meshes
from mesh_editor.samplegen import generate_project_samples
from mesh_editor.topology import weld_vertices


class PrecisionSampleTests(unittest.TestCase):
    def test_same_mesh_has_zero_bidirectional_deviation(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            report = generate_project_samples(temp)
            mesh = weld_vertices(parse_mesh(Path(temp) / "data/samples/part_v2.obj"), 1e-6).mesh
            precision = compare_meshes(mesh, mesh)
        self.assertEqual(precision["code"], "OK")
        self.assertLess(precision["max_bidirectional_distance"], 1e-12)
        self.assertEqual(precision["candidate_bad_faces"], [])
        self.assertIn("baseline", report)

    def test_exported_stl_geometry_matches_truth(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            generate_project_samples(temp)
            truth = weld_vertices(parse_mesh(Path(temp) / "data/samples/part_v2.obj"), 1e-6).mesh
            loaded = parse_mesh(Path(temp) / "data/samples/part_binary.stl")
            precision = compare_meshes(truth, loaded, tolerance=1e-5, normal_tolerance_deg=2.0)
        self.assertEqual(precision["code"], "OK")
        self.assertLess(precision["max_bidirectional_distance"], 1e-6)

    def test_sample_generation_is_byte_deterministic(self) -> None:
        with tempfile.TemporaryDirectory() as temp1, tempfile.TemporaryDirectory() as temp2:
            generate_project_samples(temp1)
            generate_project_samples(temp2)
            for relative in (
                "data/samples/part_v2.obj",
                "data/samples/part_v1.obj",
                "data/samples/part_ascii.stl",
                "data/samples/part_binary.stl",
                "logs/sample_generation.log",
                "logs/baseline_trace.jsonl",
                "data/workspace/manifest.json",
                "data/workspace/snapshots/v00000001/mesh.obj",
            ):
                self.assertEqual(
                    (Path(temp1) / relative).read_bytes(),
                    (Path(temp2) / relative).read_bytes(),
                    relative,
                )


if __name__ == "__main__":
    unittest.main()
