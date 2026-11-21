from __future__ import annotations

import dataclasses
import unittest

import numpy as np

from surface_viewer.artifacts import decode_mesh
from surface_viewer.engine import IncrementalSurfaceEngine
from surface_viewer.errors import ErrorCode, SurfaceViewerError
from surface_viewer.geometry import SurfaceGeometry, seam_position_error
from surface_viewer.graph import artifact_node_id
from surface_viewer.model import parse_assembly
from surface_viewer.samplegen import (
    CASE_SPECS,
    generate_case,
    operation_changes,
    operations_for_case,
)


class IncrementalRecomputeTests(unittest.TestCase):
    def setUp(self) -> None:
        self.assembly = parse_assembly(generate_case(CASE_SPECS[1]))
        self.engine = IncrementalSurfaceEngine(self.assembly)
        self.initial = self.engine.initialize(verify_full=True)

    def test_control_point_update_reuses_unrelated_products(self) -> None:
        old_hashes = self.initial.content_hashes
        operation = operations_for_case(CASE_SPECS[1])[0]
        patch_id, changes = operation_changes(self.engine.assembly, operation)
        update = self.engine.update(patch_id, changes, verify_full=True)
        self.assertEqual(update.affected_patches, ("p0001",))
        self.assertEqual(
            set(update.recomputed_ids),
            {
                artifact_node_id("p0001", "mesh"),
                artifact_node_id("p0001", "normals"),
                artifact_node_id("p0001", "preview"),
            },
        )
        for artifact_id, content_hash in update.content_hashes.items():
            if artifact_id not in update.recomputed_ids:
                self.assertEqual(old_hashes[artifact_id], content_hash)
        self.assertNotEqual(
            old_hashes[artifact_node_id("p0001", "normals")],
            update.content_hashes[artifact_node_id("p0001", "normals")],
        )
        self.assertTrue(update.verification["byte_exact"])
        self.assertEqual(update.screen_preview.dirty_rects, ((64, 0, 64, 64),))

    def test_precision_change_is_transitive_but_local(self) -> None:
        operation = operations_for_case(CASE_SPECS[1])[1]
        patch_id, changes = operation_changes(self.engine.assembly, operation)
        update = self.engine.update(patch_id, changes, verify_full=True)
        self.assertEqual(update.affected_patches, ("p0002",))
        for patch in self.engine.assembly.patches:
            if patch.patch_id == "p0002":
                continue
            for kind in ("mesh", "normals", "density", "preview"):
                self.assertIn(
                    artifact_node_id(patch.patch_id, kind), update.reused_ids
                )
        self.assertTrue(update.verification["byte_exact"])
        self.assertEqual(len(update.screen_preview.dirty_rects), 1)
        self.assertEqual(update.screen_preview.dirty_rects[0], (0, 64, 64, 64))

    def test_all_operations_match_full_recompute_byte_for_byte(self) -> None:
        current = self.engine
        for operation in operations_for_case(CASE_SPECS[2]):
            patch_id, changes = operation_changes(current.assembly, operation)
            update = current.update(patch_id, changes, verify_full=True)
            self.assertTrue(update.verification["byte_exact"])

    def test_shared_boundary_has_zero_position_error(self) -> None:
        meshes = {
            artifact.patch_id: decode_mesh(artifact.data)[0]
            for artifact in self.initial.artifacts
            if artifact.kind == "mesh"
        }
        self.assertEqual(
            seam_position_error(self.engine.assembly, meshes), 0.0
        )

    def test_boundary_control_point_change_is_propagated_to_neighbor(self) -> None:
        patch = self.engine.assembly.patch("p0000")
        control = [
            [list(point) for point in row] for row in patch.control_points
        ]
        control[2][-1][2] += 0.23
        update = self.engine.update(
            "p0000", {"control_points": control}, verify_full=True
        )
        neighbor = self.engine.assembly.patch("p0001")
        self.assertAlmostEqual(neighbor.control_points[2][0][2], control[2][-1][2])
        self.assertEqual(
            update.affected_patches, ("p0000", "p0001")
        )
        meshes = {
            artifact.patch_id: decode_mesh(artifact.data)[0]
            for artifact in update.artifacts
            if artifact.kind == "mesh"
        }
        self.assertEqual(seam_position_error(self.engine.assembly, meshes), 0.0)

    def test_mismatch_error_identifies_first_different_vertex(self) -> None:
        update = self.engine.update(
            "p0001",
            {"precision_level": 3},
            verify_full=True,
        )
        artifacts = dict(update.artifact_map)
        mesh_id = artifact_node_id("p0001", "mesh")
        artifact = artifacts[mesh_id]
        corrupted = bytearray(artifact.data)
        corrupted[16] ^= 0x01
        artifacts[mesh_id] = dataclasses.replace(
            artifact, data=bytes(corrupted)
        )
        with self.assertRaises(SurfaceViewerError) as raised:
            self.engine._verify_full(
                update.assembly, artifacts, update.screen_preview
            )
        self.assertEqual(raised.exception.code, ErrorCode.INCR_MISMATCH)
        self.assertEqual(raised.exception.field, mesh_id)
        self.assertIn("first_difference_vertex", raised.exception.details)

    def test_nurbs_rational_boundary_sampling_is_supported(self) -> None:
        raw = {
            "schema_version": 1,
            "assembly_id": "nurbs",
            "patches": [
                {
                    "id": "p0",
                    "block_id": 0,
                    "surface_type": "nurbs",
                    "precision_level": 1,
                    "flip_normal": False,
                    "control_points": [
                        [
                            [0.0, 0.0, 0.0, 1.0],
                            [0.0, 0.5, 0.1, 1.5],
                            [0.0, 1.0, 0.0, 1.0],
                        ],
                        [
                            [0.5, 0.0, 0.2, 1.2],
                            [0.5, 0.5, 0.4, 2.0],
                            [0.5, 1.0, 0.2, 1.2],
                        ],
                        [
                            [1.0, 0.0, 0.0, 1.0],
                            [1.0, 0.5, 0.1, 1.5],
                            [1.0, 1.0, 0.0, 1.0],
                        ],
                    ],
                }
            ],
        }
        assembly = parse_assembly(raw)
        geometry = SurfaceGeometry()
        mesh = geometry.compute_mesh(assembly.patches[0])
        normals = geometry.compute_normals(assembly.patches[0])
        self.assertEqual(mesh.shape, (9, 9, 3))
        self.assertEqual(normals.shape, (9, 9, 3))
        self.assertTrue(np.isfinite(mesh).all())
        self.assertTrue(np.isfinite(normals).all())


if __name__ == "__main__":
    unittest.main()
