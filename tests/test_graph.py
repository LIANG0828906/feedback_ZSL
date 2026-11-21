from __future__ import annotations

import unittest

from surface_viewer.errors import ErrorCode, SurfaceViewerError
from surface_viewer.graph import (
    DependencyGraph,
    artifact_node_id,
    changed_parameter_node_ids,
    parameter_node_id,
)
from surface_viewer.model import parse_assembly
from surface_viewer.samplegen import CASE_SPECS, generate_case


class DependencyGraphTests(unittest.TestCase):
    def setUp(self) -> None:
        self.assembly = parse_assembly(generate_case(CASE_SPECS[1]))
        self.graph = DependencyGraph.build(self.assembly)

    def test_explicit_graph_shape_and_versions(self) -> None:
        self.assertEqual(self.graph.node_count, 32)
        self.assertEqual(self.graph.edge_count, 64)
        self.graph.assert_publishable()
        for edge in self.graph.edges:
            self.assertEqual(len(edge.param_version), 64)

    def test_control_point_change_uses_dependency_closure(self) -> None:
        patch = self.assembly.patch("p0001")
        control = [
            [list(point) for point in row] for row in patch.control_points
        ]
        control[1][1][2] += 0.2
        new_assembly = self.assembly.apply_changes(
            "p0001", {"control_points": control}
        )
        report = self.graph.invalidate(
            changed_parameter_node_ids(self.assembly, new_assembly)
        )
        self.assertEqual(
            set(report.affected_ids),
            {
                artifact_node_id("p0001", "mesh"),
                artifact_node_id("p0001", "normals"),
                artifact_node_id("p0001", "preview"),
            },
        )
        self.assertIn(
            parameter_node_id("p0001", "control_points"),
            report.dependency_closure,
        )
        self.assertIn(artifact_node_id("p0000", "mesh"), report.reused)

    def test_precision_change_does_not_touch_unrelated_patch(self) -> None:
        new_assembly = self.assembly.apply_changes(
            "p0002", {"precision_level": 3}
        )
        report = self.graph.invalidate(
            changed_parameter_node_ids(self.assembly, new_assembly)
        )
        self.assertEqual(report.affected_patches, ("p0002",))
        self.assertEqual(
            set(report.affected_ids),
            {
                artifact_node_id("p0002", kind)
                for kind in ("mesh", "normals", "density", "preview")
            },
        )
        for patch in self.assembly.patches:
            if patch.patch_id != "p0002":
                for kind in ("mesh", "normals", "density", "preview"):
                    self.assertIn(
                        artifact_node_id(patch.patch_id, kind), report.reused
                    )

    def test_missing_dependency_edge_refuses_publication(self) -> None:
        incomplete = self.graph.remove_edge(
            parameter_node_id("p0001", "flip_normal"),
            artifact_node_id("p0001", "normals"),
        )
        with self.assertRaises(SurfaceViewerError) as raised:
            incomplete.assert_publishable()
        self.assertEqual(
            raised.exception.code, ErrorCode.DEP_GRAPH_INCOMPLETE
        )
        self.assertEqual(
            raised.exception.field, artifact_node_id("p0001", "normals")
        )

    def test_graph_diff_reports_counts_and_changes(self) -> None:
        incomplete = self.graph.remove_edge(
            parameter_node_id("p0001", "flip_normal"),
            artifact_node_id("p0001", "normals"),
        )
        diff = incomplete.diff(self.graph)
        self.assertTrue(diff.changed)
        self.assertEqual(
            diff.removed_edges,
            (
                (
                    parameter_node_id("p0001", "flip_normal"),
                    artifact_node_id("p0001", "normals"),
                ),
            ),
        )
        summary = incomplete.summary(self.graph)
        self.assertEqual(summary["node_count"], 32)
        self.assertEqual(summary["edge_count"], 63)

    def test_parameter_change_is_reported_as_node_and_edge_version_diff(
        self,
    ) -> None:
        changed = self.assembly.apply_changes(
            "p0001", {"precision_level": 3}
        )
        new_graph = DependencyGraph.build(changed)
        diff = new_graph.diff(self.graph)
        self.assertIn(
            parameter_node_id("p0001", "precision_level"),
            {item[0] for item in diff.changed_nodes},
        )
        self.assertIn(
            (
                parameter_node_id("p0001", "precision_level"),
                artifact_node_id("p0001", "mesh"),
            ),
            {item[:2] for item in diff.changed_edges},
        )

    def test_unproven_cache_validity_cascades_conservative_recompute(self) -> None:
        cached = {
            node.node_id: node.version for node in self.graph.artifact_nodes()
        }
        mesh_id = artifact_node_id("p0001", "mesh")
        cached[mesh_id] = "not-the-current-signature"
        report = self.graph.invalidate([], cached_signatures=cached)
        self.assertIn(mesh_id, report.affected_ids)
        self.assertIn(
            artifact_node_id("p0001", "preview"), report.affected_ids
        )
        self.assertIn(
            artifact_node_id("p0001", "mesh"),
            {item.artifact_id for item in report.conservative},
        )
        self.assertIn(
            artifact_node_id("p0001", "preview"),
            {item.artifact_id for item in report.conservative},
        )


if __name__ == "__main__":
    unittest.main()
