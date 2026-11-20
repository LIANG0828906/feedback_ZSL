from __future__ import annotations

import dataclasses
import hashlib
import json
import sys
import tempfile
import threading
import unittest
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from terrainviz.camera import Camera
from terrainviz.cancel import CancellationToken
from terrainviz.errors import (
    E_BUSY,
    E_CANCELLED,
    E_CRACK_UNRESOLVED,
    E_HEIGHT_INVALID,
    E_SNAPSHOT_INVALID,
    TerrainError,
)
from terrainviz.heightfield import HeightField
from terrainviz.io_utils import canonical_json_bytes
from terrainviz.lod import choose_lod
from terrainviz.material import MaterialMap
from terrainviz.normals import DEFAULT_NORMAL, NormalManager
from terrainviz.render import RenderConfig, Renderer
from terrainviz.samples import (
    SampleTerrain,
    generate_sample_set,
    load_sample,
    sample_camera,
    write_baseline_trajectory,
)
from terrainviz.seams import repair_seams
from terrainviz.service import RenderService
from terrainviz.snapshot import (
    load_snapshot,
    recover_terrain_from_disk,
    save_snapshot,
    snapshot_changes,
)
from terrainviz.terrain import Terrain


def make_plain(
    rows: int = 17,
    cols: int = 21,
    spacing: float = 1.0,
    material: bool = False,
) -> Terrain:
    z, x = np.mgrid[0:rows, 0:cols]
    heights = (3.0 + 0.5 * np.sin(x * 0.4) + 0.3 * np.cos(z * 0.3)).astype(np.float32)
    hf = HeightField(
        heights=heights,
        spacing=spacing,
        quantization_step=0.001,
        valid_min=-20.0,
        valid_max=20.0,
        source_digest="",
    )
    mat = None
    if material:
        indices = np.zeros((rows, cols), dtype=np.uint8)
        indices[:, cols // 2 :] = 3
        mat = MaterialMap(indices=indices, source_digest="")
    return Terrain(hf, mat, block_size=8)


def make_mountain(rows: int = 17, cols: int = 21) -> Terrain:
    z, x = np.mgrid[0:rows, 0:cols]
    heights = (
        2.0
        + 45.0
        * np.exp(-(((x - cols * 0.6) / 5.0) ** 2 + ((z - rows * 0.45) / 4.5) ** 2))
        + 5.0 * np.sin(x * 0.8) * np.cos(z * 0.6)
    ).astype(np.float32)
    hf = HeightField(
        heights=heights,
        spacing=1.0,
        quantization_step=0.01,
        valid_min=-10.0,
        valid_max=80.0,
        source_digest="",
    )
    indices = (np.clip(heights / 80.0 * 10.0, 0, 15)).astype(np.uint8)
    return Terrain(hf, MaterialMap(indices=indices, source_digest=""), block_size=8)


class HeightFieldTests(unittest.TestCase):
    def test_roundtrip_and_invalid_payload(self) -> None:
        terrain = make_plain()
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "terrain.thf"
            terrain.heightfield.save(path)
            loaded = HeightField.load(path)
            np.testing.assert_array_equal(loaded.heights, terrain.heightfield.heights)
            self.assertEqual(loaded.spacing, 1.0)
            self.assertEqual(loaded.quantization_step, 0.001)
            normalized = loaded.normalized()
            self.assertEqual(normalized.dtype, np.float64)
            self.assertGreaterEqual(float(np.min(normalized)), 0.0)
            self.assertLessEqual(float(np.max(normalized)), 1.0)
            raw = bytearray(path.read_bytes())
            raw[-1] ^= 0xFF
            bad = Path(tmp) / "bad.thf"
            bad.write_bytes(raw)
            with self.assertRaises(TerrainError) as caught:
                HeightField.load(bad)
            self.assertEqual(caught.exception.code, E_HEIGHT_INVALID)

    def test_out_of_range_and_bad_header(self) -> None:
        with self.assertRaises(TerrainError) as caught:
            HeightField(
                heights=np.array([[0.0, 2.0], [3.0, 50.0]], dtype=np.float32),
                spacing=1.0,
                quantization_step=0.01,
                valid_min=0.0,
                valid_max=10.0,
                source_digest="",
            )
        self.assertEqual(caught.exception.code, E_HEIGHT_INVALID)
        with self.assertRaises(TerrainError) as caught:
            HeightField.from_bytes(b"not a heightfield")
        self.assertEqual(caught.exception.code, E_HEIGHT_INVALID)


class TreeAndLodTests(unittest.TestCase):
    def test_quadtree_rebuild_report(self) -> None:
        terrain = make_plain(rows=25, cols=33)
        initial_nodes = terrain.node_count
        initial_blocks = len(terrain.blocks)
        self.assertGreater(initial_nodes, initial_blocks)
        report = terrain.rebuild(block_size=5, spacing=2.0, reason="test")
        self.assertEqual(report.old_node_count, initial_nodes)
        self.assertEqual(report.new_node_count, terrain.node_count)
        self.assertGreater(report.new_block_count, initial_blocks)
        self.assertEqual(report.spacing, 2.0)
        self.assertEqual(report.max_height_delta, 0.0)
        self.assertIsNotNone(terrain.quadtree_root)
        self.assertEqual(terrain.quadtree_root.truth_source, "derived_from_heightfield")
        leaf = terrain.node_by_id[
            next(key for key, node in terrain.node_by_id.items() if not node.children)
        ]
        self.assertEqual(leaf.truth_source, "heightfield")
        patch = terrain.heightfield.heights[
            leaf.row0 : leaf.row1 + 1, leaf.col0 : leaf.col1 + 1
        ]
        self.assertAlmostEqual(leaf.min_height, float(np.min(patch)))
        self.assertAlmostEqual(leaf.max_height, float(np.max(patch)))

    def test_lod_is_deterministic_and_reports_budget(self) -> None:
        terrain = make_mountain()
        camera = Camera(
            eye=(10.0, 30.0, 28.0),
            target=(10.0, 15.0, 8.0),
            fov_y_degrees=45.0,
            width=64,
            height=48,
        )
        first = choose_lod(terrain, camera, 5.0, max_level=3)
        second = choose_lod(terrain, camera, 5.0, max_level=3)
        self.assertEqual(first.levels_by_id(), second.levels_by_id())
        for selection in first.selections.values():
            self.assertGreaterEqual(selection.budget_usage, 0.0)
            self.assertGreaterEqual(selection.error_upper_bound, 0.0)
            self.assertTrue(
                selection.projected_error_pixels <= 5.0 + 1e-12
                or selection.fallback
            )
        self.assertGreater(len(set(first.levels_by_id().values())), 1)


class SeamTests(unittest.TestCase):
    def test_matched_vertices_close_all_seams(self) -> None:
        terrain = make_mountain(rows=25, cols=33)
        camera = Camera(
            eye=(12.0, 40.0, 36.0),
            target=(16.0, 15.0, 12.0),
            fov_y_degrees=48.0,
            width=64,
            height=48,
        )
        decision = choose_lod(terrain, camera, 1.0, max_level=3)
        repaired = repair_seams(terrain, decision, tolerance=0.5)
        self.assertEqual(repaired.status, "closed")
        self.assertTrue(all(seam.closed for seam in repaired.seams))
        self.assertTrue(
            all(seam.post_repair_deviation <= 0.5 for seam in repaired.seams)
        )
        self.assertTrue(any(seam.sample_count > 0 for seam in repaired.seams))

    def test_unresolved_path_has_seam_location(self) -> None:
        terrain = make_mountain(rows=17, cols=21)
        decision = choose_lod(
            terrain,
            Camera(
                eye=(8.0, 30.0, 30.0),
                target=(10.0, 15.0, 8.0),
                width=64,
                height=48,
            ),
            0.8,
            max_level=3,
        )
        a = terrain.blocks[0]
        b = terrain.block_by_id[f"B{a.chunk_row:04d}_{a.chunk_col + 1:04d}"]
        selections = dict(decision.selections)
        selections[a.id] = dataclasses.replace(selections[a.id], level=3, stride=8)
        selections[b.id] = dataclasses.replace(selections[b.id], level=0, stride=1)
        forced = dataclasses.replace(decision, selections=selections)
        with self.assertRaises(TerrainError) as caught:
            repair_seams(terrain, forced, tolerance=1e-9, max_refinement_passes=0)
        self.assertEqual(caught.exception.code, E_CRACK_UNRESOLVED)
        self.assertIn("block_a", caught.exception.details)
        self.assertIn("edge_a", caught.exception.details)
        self.assertIn("seam_id", caught.exception.details)


class NormalTests(unittest.TestCase):
    def test_flat_default_and_normalization(self) -> None:
        heights = np.full((9, 9), 4.0, dtype=np.float32)
        hf = HeightField(heights, 1.0, 0.001, 0.0, 10.0, "")
        terrain = Terrain(hf, None, block_size=4)
        manager = NormalManager()
        update = manager.ensure(terrain, [block.id for block in terrain.blocks])
        self.assertEqual(update.reused_blocks, [])
        self.assertEqual(len(update.recomputed_blocks), len(terrain.blocks))
        for item in update.normal_list:
            self.assertTrue(item["normalized"])
            self.assertEqual(item["default_normal_count"], item["count"])
        np.testing.assert_allclose(manager.get(terrain.blocks[0].id).normals[0, 0], DEFAULT_NORMAL)

    def test_material_change_recomputes_only_affected_blocks(self) -> None:
        terrain = make_plain(rows=17, cols=17, material=True)
        block_ids = [block.id for block in terrain.blocks]
        manager = NormalManager()
        first = manager.ensure(terrain, block_ids, terrain.material)
        self.assertEqual(len(first.recomputed_blocks), len(block_ids))
        second = manager.ensure(terrain, block_ids, terrain.material)
        self.assertEqual(second.reused_blocks, sorted(block_ids))
        changed = terrain.material.indices.copy()
        target = terrain.block_by_id[block_ids[0]]
        changed[
            (target.row0 + target.row1) // 2,
            (target.col0 + target.col1) // 2,
        ] = 12
        terrain.material = MaterialMap(changed, "")
        third = manager.ensure(terrain, block_ids, terrain.material)
        self.assertIn(block_ids[0], third.recomputed_blocks)
        self.assertLess(len(third.recomputed_blocks), len(block_ids))
        self.assertGreater(len(third.reused_blocks), 0)
        terrain.rebuild(spacing=2.0)
        fourth = manager.ensure(terrain, block_ids, terrain.material)
        self.assertEqual(fourth.reused_blocks, [])


class RenderAndSnapshotTests(unittest.TestCase):
    def test_render_determinism_and_precision_diff(self) -> None:
        terrain = make_mountain(rows=17, cols=21)
        camera = Camera(
            eye=(10.0, 35.0, 34.0),
            target=(10.0, 15.0, 9.0),
            width=64,
            height=48,
        )
        renderer = Renderer()
        first = renderer.render(
            terrain, camera, 1.0, 0.5, max_level=3, config=RenderConfig("float64")
        )
        second = renderer.render(
            terrain, camera, 1.0, 0.5, max_level=3, config=RenderConfig("float64")
        )
        np.testing.assert_array_equal(first.color, second.color)
        np.testing.assert_array_equal(first.depth, second.depth)
        np.testing.assert_array_equal(first.error_heatmap, second.error_heatmap)
        self.assertEqual(second.report["pixel_diff"]["changed_color_pixels"], 0)
        self.assertEqual(second.report["pixel_diff"]["max_abs_depth_difference"], 0.0)
        third = renderer.render(
            terrain, camera, 1.0, 0.5, max_level=3, config=RenderConfig("float32")
        )
        self.assertTrue(third.report["precision_changed"])
        self.assertTrue(third.report["pixel_diff"]["precision_changed"])
        self.assertGreater(
            third.report["pixel_diff"]["max_abs_depth_difference"], 0.0
        )
        self.assertEqual(first.error_heatmap.shape, (48, 64))
        self.assertGreaterEqual(float(np.max(first.error_heatmap)), 0.0)

    def test_snapshot_hash_validation_and_change_detection(self) -> None:
        terrain = make_plain(material=True)
        camera = Camera(
            eye=(8.0, 25.0, 25.0),
            target=(8.0, 4.0, 8.0),
            width=48,
            height=32,
        )
        result = Renderer().render(terrain, camera, 1.5, 0.5, max_level=2)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "snapshot.json"
            saved = save_snapshot(path, result, terrain)
            loaded = load_snapshot(path)
            self.assertEqual(saved.digest, loaded.digest)
            levels = {
                item["block_id"]: item["level"] for item in result.report["levels"]
            }
            no_change = snapshot_changes(
                loaded, camera, levels, terrain.material.material_version, 1.5
            )
            self.assertFalse(no_change["requires_rerender"])
            changed_levels = dict(levels)
            first_id = sorted(changed_levels)[0]
            changed_levels[first_id] += 1
            changed = snapshot_changes(
                loaded, camera, changed_levels, terrain.material.material_version, 1.5
            )
            self.assertTrue(changed["levels_changed"])
            self.assertIn("levels", changed["reasons"])
            rotated_camera = dataclasses.replace(camera, eye=(1.0, 25.0, 25.0))
            view_changed = snapshot_changes(
                loaded,
                rotated_camera,
                levels,
                terrain.material.material_version,
                1.5,
            )
            self.assertTrue(view_changed["view_changed"])
            content = json.loads(path.read_text())
            content["screen_error_threshold"] = 99.0
            path.write_text(json.dumps(content), encoding="utf-8")
            with self.assertRaises(TerrainError) as caught:
                load_snapshot(path)
            self.assertEqual(caught.exception.code, E_SNAPSHOT_INVALID)

            content = json.loads(path.read_text())
            del content["normal_list"]
            content["content_hash"] = hashlib.sha256(
                canonical_json_bytes(
                    {key: value for key, value in content.items() if key != "content_hash"}
                )
            ).hexdigest()
            path.write_text(json.dumps(content), encoding="utf-8")
            with self.assertRaises(TerrainError) as caught:
                load_snapshot(path)
            self.assertEqual(caught.exception.code, E_SNAPSHOT_INVALID)

    def test_busy_cancellation_and_last_snapshot(self) -> None:
        terrain = make_plain(material=True)
        camera = Camera(
            eye=(8.0, 25.0, 25.0),
            target=(8.0, 4.0, 8.0),
            width=48,
            height=32,
        )
        service = RenderService(terrain)
        other_service = RenderService(terrain)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "snapshot.json"
            service.render(camera, 1.5, 0.5, snapshot_path=path)
            committed = path.read_bytes()
            service._acquire()
            try:
                with self.assertRaises(TerrainError) as caught:
                    other_service.render(camera, 1.5, 0.5)
                self.assertEqual(caught.exception.code, E_BUSY)
            finally:
                service._release()
            token = CancellationToken()
            token.cancel()
            with self.assertRaises(TerrainError) as caught:
                service.render(camera, 1.5, 0.5, snapshot_path=path, cancel_token=token)
            self.assertEqual(caught.exception.code, E_CANCELLED)
            self.assertGreater(token.checkpoint, 0)
            self.assertEqual(path.read_bytes(), committed)
            self.assertIsNotNone(service.last_result)

    def test_recovery_uses_snapshot_and_source_files(self) -> None:
        terrain = make_plain(material=True)
        camera = Camera(
            eye=(8.0, 25.0, 25.0),
            target=(8.0, 4.0, 8.0),
            width=48,
            height=32,
        )
        result = Renderer().render(terrain, camera, 1.5, 0.5, max_level=2)
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            hf_path = root / "terrain.thf"
            mat_path = root / "terrain.matidx"
            snapshot_path = root / "snapshot.json"
            terrain.heightfield.save(hf_path)
            terrain.material.save(mat_path)
            loaded_terrain = Terrain(
                HeightField.load(hf_path), MaterialMap.load(mat_path), block_size=8
            )
            save_snapshot(
                snapshot_path,
                result,
                loaded_terrain,
                heightfield_name=hf_path.name,
                material_name=mat_path.name,
            )
            recovered, snapshot = recover_terrain_from_disk(
                hf_path, snapshot_path, mat_path
            )
            self.assertEqual(recovered.heightfield.height_digest, terrain.heightfield.height_digest)
            self.assertEqual(recovered.node_count, terrain.node_count)
            self.assertEqual(snapshot.digest, load_snapshot(snapshot_path).digest)

    def test_submit_render_is_busy_across_service_instances(self) -> None:
        terrain = make_plain(material=True)
        camera = Camera(
            eye=(8.0, 25.0, 25.0),
            target=(8.0, 4.0, 8.0),
            width=48,
            height=32,
        )
        entered = threading.Event()
        release = threading.Event()

        class BlockingRenderer(Renderer):
            def render(self, *args, **kwargs):
                entered.set()
                if not release.wait(5.0):
                    raise TimeoutError("test renderer was not released")
                return super().render(*args, **kwargs)

        owner = RenderService(terrain, BlockingRenderer())
        contender = RenderService(terrain)
        future, _ = owner.submit_render(camera, 1.5, 0.5, max_level=2)
        self.assertTrue(entered.wait(2.0))
        self.assertTrue(owner.active)
        with self.assertRaises(TerrainError) as caught:
            contender.render(camera, 1.5, 0.5)
        self.assertEqual(caught.exception.code, E_BUSY)
        release.set()
        result = future.result(timeout=5.0)
        self.assertEqual(result.error_heatmap.shape, (32, 48))
        self.assertFalse(owner.active)


class ReproducibilityTests(unittest.TestCase):
    def test_samples_baseline_and_snapshot_are_byte_identical(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            samples_a = generate_sample_set(root / "a", seed=77, rows=17, cols=21)
            samples_b = generate_sample_set(root / "b", seed=77, rows=17, cols=21)
            for left, right in zip(samples_a, samples_b):
                self.assertEqual(left.heightfield_path.read_bytes(), right.heightfield_path.read_bytes())
                self.assertEqual(left.material_path.read_bytes(), right.material_path.read_bytes())
            self.assertEqual(
                (root / "a" / "manifest.json").read_bytes(),
                (root / "b" / "manifest.json").read_bytes(),
            )
            trace_a = write_baseline_trajectory(
                samples_a, root / "a" / "trace.jsonl", seam_tolerance=1.0
            )
            trace_b = write_baseline_trajectory(
                samples_b, root / "b" / "trace.jsonl", seam_tolerance=1.0
            )
            self.assertEqual(trace_a.read_bytes(), trace_b.read_bytes())
            terrain_a = load_sample(samples_a[2])
            terrain_b = load_sample(samples_b[2])
            camera = sample_camera(terrain_a, "overview")
            result_a = Renderer().render(terrain_a, camera, 1.5, 1.0)
            result_b = Renderer().render(terrain_b, camera, 1.5, 1.0)
            self.assertEqual(result_a.combined_digest, result_b.combined_digest)
            snapshot_a = root / "a" / "snapshot.json"
            snapshot_b = root / "b" / "snapshot.json"
            save_snapshot(
                snapshot_a,
                result_a,
                terrain_a,
                heightfield_name=samples_a[2].heightfield_path.name,
                material_name=samples_a[2].material_path.name,
            )
            save_snapshot(
                snapshot_b,
                result_b,
                terrain_b,
                heightfield_name=samples_b[2].heightfield_path.name,
                material_name=samples_b[2].material_path.name,
            )
            self.assertEqual(snapshot_a.read_bytes(), snapshot_b.read_bytes())


if __name__ == "__main__":
    unittest.main()

