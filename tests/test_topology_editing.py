from __future__ import annotations

import unittest

import numpy as np

from mesh_editor.editing import Selection, simplify_edge_collapse, transform_selection
from mesh_editor.model import Face, Mesh
from mesh_editor.samplegen import build_demo_mesh
from mesh_editor.topology import build_halfedge_topology, weld_vertices


class TopologyEditingTests(unittest.TestCase):
    def test_weld_is_deterministic_and_mapping_is_stable(self) -> None:
        mesh = build_demo_mesh()
        first = weld_vertices(mesh, 1e-6)
        second = weld_vertices(mesh, 1e-6)
        self.assertEqual(first.report(), second.report())
        self.assertEqual(first.old_to_new[8], 0)
        self.assertEqual(first.merged_vertices, 1)
        self.assertIsNotNone(first.mesh.normals)
        self.assertEqual(len(first.mesh.normals), len(first.mesh.vertices))
        self.assertTrue(all(f.normals is None for f in first.mesh.faces))

    def test_tolerance_change_changes_report(self) -> None:
        vertices = np.asarray(
            [[0, 0, 0], [1, 0, 0], [0, 1, 0], [1.00001, 0, 0], [1, 1, 0]],
            dtype=np.float64,
        )
        mesh = Mesh(
            vertices,
            [Face((0, 1, 2)), Face((3, 1, 4)), Face((3, 4, 2))],
        )
        tight = weld_vertices(mesh, 1e-9)
        loose = weld_vertices(mesh, 1e-4)
        self.assertEqual(tight.merged_vertices, 0)
        self.assertEqual(loose.merged_vertices, 1)
        self.assertNotEqual(tight.affected_faces, loose.affected_faces)

    def test_halfedge_boundary_loop(self) -> None:
        mesh = Mesh(
            np.asarray([[0, 0, 0], [1, 0, 0], [1, 1, 0], [0, 1, 0]], dtype=np.float64),
            [Face((0, 1, 2)), Face((0, 2, 3))],
        )
        topology = build_halfedge_topology(mesh)
        self.assertEqual(topology.non_manifold_edge_count, 0)
        self.assertEqual(len(topology.boundary_loops), 1)
        self.assertEqual(len(topology.boundary_loops[0]), 4)

    def test_non_manifold_edge_detected(self) -> None:
        mesh = build_demo_mesh()
        topology = build_halfedge_topology(mesh)
        self.assertGreaterEqual(topology.non_manifold_edge_count, 2)

    def test_transform_selection_metrics_are_exact_after_roundtrip(self) -> None:
        mesh = weld_vertices(build_demo_mesh(), 1e-6).mesh
        before = mesh.metrics()
        result = transform_selection(
            mesh,
            Selection(face_indices=(0, 1)),
            translation=(0.1, 0.2, 0.3),
            rotation_degrees=(0, 0, 15),
            scale=0.9,
        )
        self.assertEqual(result.metrics_before, before)
        self.assertNotEqual(result.metrics_after, before)
        self.assertGreater(result.metrics_after["vertices"], 0)
        self.assertIsNotNone(result.mesh.normals)

    def test_edge_collapse_reduces_faces_and_records_metrics(self) -> None:
        mesh = weld_vertices(build_demo_mesh(), 1e-6).mesh
        result = simplify_edge_collapse(mesh, target_ratio=0.7)
        self.assertLess(len(result.mesh.faces), len(mesh.faces))
        self.assertEqual(result.metrics_before, mesh.metrics())
        self.assertEqual(result.metrics_after, result.mesh.metrics())
        self.assertEqual(result.operation["type"], "edge_collapse")

    def test_cancel_does_not_return_partial_mesh(self) -> None:
        mesh = weld_vertices(build_demo_mesh(), 1e-6).mesh
        from mesh_editor.errors import EditCancelled

        with self.assertRaises(EditCancelled):
            simplify_edge_collapse(mesh, target_ratio=0.2, cancelled=lambda: True)
        self.assertEqual(len(mesh.faces), 14)


if __name__ == "__main__":
    unittest.main()
