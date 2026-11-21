from __future__ import annotations

import hashlib
import json
import struct
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"
if str(SRC) not in sys.path:
    sys.path.insert(0, str(SRC))

from tactical_pick.camera import Camera
from tactical_pick.editor import SceneEditor
from tactical_pick.errors import ToolError
from tactical_pick.generator import CASES, _build_initial_scene, generate_samples
from tactical_pick.injection import (
    EXPECTED_INJECTION_CODES,
    INJECTION_CASES,
    injection_selfcheck,
    trigger_injection,
)
from tactical_pick.math3d import Ray
from tactical_pick.query import PickRequest, Picker, intersect_snapshot
from tactical_pick.region import PlacementRequest, RegionJudge, judge_snapshot
from tactical_pick.regression import run_regression
from tactical_pick.scene import (
    CoverTriangle,
    HEADER,
    TRIANGLE_RECORD,
    UNIT_RECORD,
    VERTEX_RECORD,
    SceneData,
    Unit,
    decode_scene,
    encode_scene,
    make_passable_mask,
)
from tactical_pick.snapshot import SceneManager, SceneSnapshot
from tactical_pick.trace import TraceLog, replay_trace
from tactical_pick.util import f32


def base_scene(version: int = 1, width: int = 12, height: int = 10) -> SceneData:
    cell = 1.0
    blocked = {(width - 3, height - 2)}
    return SceneData(
        version=version,
        grid_width=width,
        grid_height=height,
        cell_size=f32(cell),
        ground_y=f32(0.0),
        units=(
            Unit.box(1, tuple(f32(value) for value in (2.0, 0.1, 2.0)), tuple(f32(value) for value in (3.2, 2.0, 3.0))),
            Unit.capsule(2, tuple(f32(value) for value in (7.0, 0.2, 6.0)), tuple(f32(value) for value in (7.0, 1.7, 6.0)), f32(0.4)),
        ),
        cover_triangles=(
            # A vertical cover at z=4.
            CoverTriangle(
                90,
                tuple(f32(value) for value in (5.0, 0.0, 4.0)),
                tuple(f32(value) for value in (6.4, 0.0, 4.0)),
                tuple(f32(value) for value in (5.7, 1.4, 4.0)),
            ),
        ),
        passable_mask=make_passable_mask(width, height, blocked),
    )


def center_pick(target: tuple[float, float, float], offset=(1.0, 6.0, 6.0)) -> PickRequest:
    eye = (target[0] + offset[0], target[1] + offset[1], target[2] + offset[2])
    camera = Camera.look_at(eye, target)
    return PickRequest((camera.width * 0.5, camera.height * 0.5), camera)


class SceneTests(unittest.TestCase):
    def test_binary_roundtrip_preserves_hash_and_mask(self) -> None:
        scene = base_scene()
        data = encode_scene(scene)
        decoded = decode_scene(data)
        self.assertEqual(scene.compute_hash(), decoded.compute_hash())
        self.assertTrue(decoded.is_walkable(0, 0))
        self.assertFalse(decoded.is_walkable(9, 8))
        self.assertEqual(decoded.version, 1)

    def test_degenerate_bounds_and_first_conflict(self) -> None:
        scene = base_scene()
        broken = Unit.box(1, (2.0, 1.0, 3.0), (2.0, 1.0, 3.0))
        data = encode_scene(
            SceneData(
                version=scene.version,
                grid_width=scene.grid_width,
                grid_height=scene.grid_height,
                cell_size=scene.cell_size,
                ground_y=scene.ground_y,
                units=(broken, *scene.units[1:]),
                cover_triangles=scene.cover_triangles,
                passable_mask=scene.passable_mask,
            )
        )
        with self.assertRaises(ToolError) as caught:
            decode_scene(data)
        self.assertEqual(caught.exception.code, "E_SCENE_INVALID")
        self.assertIn("scene.units[0]", caught.exception.injection_point)

    def test_illegal_triangle_index_and_mask_size(self) -> None:
        scene = base_scene()
        data = bytearray(encode_scene(scene))
        triangle_offset = (
            HEADER.size
            + len(scene.units) * UNIT_RECORD.size
            + len(scene.cover_triangles) * 3 * VERTEX_RECORD.size
        )
        struct.pack_into("<I", data, triangle_offset + 4, 99)
        with self.assertRaises(ToolError) as caught:
            decode_scene(bytes(data))
        self.assertEqual(caught.exception.code, "E_SCENE_INVALID")
        self.assertEqual(
            caught.exception.injection_point,
            "scene.cover_triangles[0].indices[0]",
        )

        short_mask = base_scene()
        short_mask = SceneData(
            version=short_mask.version,
            grid_width=short_mask.grid_width,
            grid_height=short_mask.grid_height,
            cell_size=short_mask.cell_size,
            ground_y=short_mask.ground_y,
            units=short_mask.units,
            cover_triangles=short_mask.cover_triangles,
            passable_mask=short_mask.passable_mask[:-1],
        )
        with self.assertRaises(ToolError) as caught:
            decode_scene(encode_scene(short_mask))
        self.assertEqual(caught.exception.code, "E_SCENE_INVALID")
        self.assertIn("payload", caught.exception.injection_point)


class QueryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.scene = base_scene()
        self.snapshot = SceneSnapshot.from_scene(self.scene)
        self.picker = Picker(SceneManager(self.scene))

    def test_hits_aabb_capsule_cover_and_ground(self) -> None:
        aabb = self.picker.pick(center_pick((2.6, 1.0, 2.5)))
        self.assertEqual(aabb.hit.entity, "unit:1")
        self.assertTrue(2.0 <= aabb.hit.point[0] <= 3.2)
        self.assertGreater(aabb.hit.distance, 0.0)

        capsule = self.picker.pick(center_pick((7.0, 0.95, 6.0)))
        self.assertEqual(capsule.hit.entity, "unit:2")

        cover = self.picker.pick(center_pick((5.7, 0.5, 4.0)))
        self.assertEqual(cover.hit.entity, "cover:90")

        ground = self.picker.pick(center_pick((1.0, 0.0, 1.0)))
        self.assertEqual(ground.hit.entity, "ground")
        self.assertAlmostEqual(ground.hit.point[1], 0.0, places=7)

    def test_nearest_hit_and_version_attribution(self) -> None:
        near = Unit.box(3, (2.3, 0.4, 2.2), (2.9, 1.5, 2.8))
        scene = SceneData(
            version=2,
            grid_width=self.scene.grid_width,
            grid_height=self.scene.grid_height,
            cell_size=self.scene.cell_size,
            ground_y=self.scene.ground_y,
            units=(*self.scene.units, near),
            cover_triangles=self.scene.cover_triangles,
            passable_mask=self.scene.passable_mask,
        )
        picker = Picker(SceneManager(scene))
        result = picker.pick(center_pick((2.6, 1.0, 2.5)))
        self.assertEqual(result.scene_version, 2)
        self.assertEqual(result.scene_hash, scene.compute_hash())
        self.assertEqual(result.hit.entity_kind, "unit")

    def test_invalid_ray_camera_and_screen_do_not_return_hits(self) -> None:
        with self.assertRaises(ToolError) as caught:
            self.picker.pick_ray(Ray((0.0, 0.0, 0.0), (0.0, 0.0, 0.0)))
        self.assertEqual(caught.exception.code, "E_RAY_INVALID")

        bad_camera = Camera.look_at((4.0, 8.0, 8.0), (2.6, 1.0, 2.5))
        bad_camera = Camera(
            bad_camera.width,
            bad_camera.height,
            0.0,
            bad_camera.fy,
            bad_camera.cx,
            bad_camera.cy,
            bad_camera.camera_to_world,
        )
        with self.assertRaises(ToolError) as caught:
            self.picker.pick(PickRequest((10.0, 10.0), bad_camera))
        self.assertEqual(caught.exception.code, "E_RAY_INVALID")

        good_camera = Camera.look_at((4.0, 8.0, 8.0), (2.6, 1.0, 2.5))
        with self.assertRaises(ToolError) as caught:
            self.picker.pick(PickRequest((-1.0, 10.0), good_camera))
        self.assertEqual(caught.exception.code, "E_SCREEN_OUT_OF_RANGE")

        with self.assertRaises(ToolError) as caught:
            self.picker.pick_ray(Ray((0.0, 0.0, 0.0), (float("nan"), 0.0, 1.0)))
        self.assertEqual(caught.exception.code, "E_RAY_INVALID")

    def test_bvh_candidates_are_complete(self) -> None:
        camera = Camera.look_at((3.0, 6.0, 10.0), (4.0, 0.0, 4.0))
        ray = camera.screen_to_ray((camera.width * 0.5, camera.height * 0.5))
        self.assertIsNotNone(intersect_snapshot(self.snapshot, ray))
        for index in self.snapshot.unit_bvh.candidate_indices(ray):
            self.assertLess(index, len(self.scene.units))


class VersionTests(unittest.TestCase):
    def test_stale_query_archive_and_released_handle(self) -> None:
        manager = SceneManager(base_scene())
        old_handle = manager.acquire(1)
        old_hash = manager.current.content_hash
        editor = SceneEditor(manager)
        editor.apply_batch([{"op": "move_unit", "unit_id": 1, "delta": [0.25, 0.0, 0.0]}])
        self.assertEqual(manager.current.version, 2)
        self.assertEqual(old_handle.snapshot().content_hash, old_hash)

        with self.assertRaises(ToolError) as caught:
            manager.acquire(1)
        self.assertEqual(caught.exception.code, "E_VERSION_STALE")

        archived = manager.acquire_archived(1)
        self.assertEqual(archived.snapshot().content_hash, old_hash)
        archived.release()
        with self.assertRaises(ToolError) as caught:
            archived.snapshot()
        self.assertEqual(caught.exception.code, "E_STALE_HANDLE")
        old_handle.release()

    def test_old_handle_remains_complete_during_publication(self) -> None:
        manager = SceneManager(base_scene())
        handle = manager.acquire()
        request = center_pick((2.6, 1.0, 2.5))
        before = Picker(manager).pick(request, handle=handle)
        editor = SceneEditor(manager)
        editor.apply_batch([{"op": "move_unit", "unit_id": 1, "delta": [30.0, 0.0, 0.0]}])
        after = Picker(manager).pick(request, handle=handle)
        self.assertEqual(before.scene_version, 1)
        self.assertEqual(after.scene_version, 1)
        self.assertEqual(after.scene_hash, before.scene_hash)
        handle.release()

    def test_corrupt_leased_snapshot(self) -> None:
        manager = SceneManager(base_scene())
        handle = manager.acquire()
        object.__setattr__(manager.current, "content_hash", "broken")
        with self.assertRaises(ToolError) as caught:
            Picker(manager).pick(center_pick((2.6, 1.0, 2.5)), handle=handle)
        self.assertEqual(caught.exception.code, "E_SNAPSHOT_CORRUPT")


class RegionTests(unittest.TestCase):
    def setUp(self) -> None:
        self.scene = base_scene(width=16, height=14)
        self.snapshot = SceneSnapshot.from_scene(self.scene)
        self.manager = SceneManager(self.scene)
        self.judge = RegionJudge(self.manager)

    def test_legal_region_is_reproducible(self) -> None:
        request = PlacementRequest(((0.2, 0.2), (0.8, 0.2), (0.8, 0.8), (0.2, 0.8)))
        first = self.judge.judge(request)
        second = self.judge.judge(request)
        self.assertTrue(first.valid)
        self.assertEqual(first.reason, "OK")
        self.assertEqual(first.decision_hash, second.decision_hash)

    def test_self_intersection_and_out_of_bounds(self) -> None:
        with self.assertRaises(ToolError) as caught:
            self.judge.judge(PlacementRequest(((1.0, 1.0), (3.0, 3.0), (1.0, 3.0), (3.0, 1.0))))
        self.assertEqual(caught.exception.code, "E_REGION_INVALID")
        self.assertEqual(caught.exception.injection_point, "region.vertices[2]")

        with self.assertRaises(ToolError) as caught:
            self.judge.judge(PlacementRequest(((20.0, 1.0), (21.0, 1.0), (21.0, 2.0), (20.0, 2.0))))
        self.assertEqual(caught.exception.code, "E_REGION_INVALID")
        self.assertEqual(caught.exception.injection_point, "region.vertices[0]")

    def test_walkability_unit_and_cover_conflicts(self) -> None:
        blocked = PlacementRequest(((13.1, 12.1), (13.9, 12.1), (13.9, 12.9), (13.1, 12.9)))
        verdict = self.judge.judge(blocked)
        self.assertFalse(verdict.valid)
        self.assertEqual(verdict.reason, "IMPASSABLE_CELL")
        self.assertIsNotNone(verdict.failure_vertex)
        self.assertTrue(verdict.conflict_entity.startswith("grid:"))

        unit = PlacementRequest(((2.1, 2.1), (3.0, 2.1), (3.0, 2.9), (2.1, 2.9)))
        verdict = self.judge.judge(unit)
        self.assertFalse(verdict.valid)
        self.assertEqual(verdict.reason, "UNIT_OVERLAP")
        self.assertEqual(verdict.conflict_entity, "unit:1")

        cover = PlacementRequest(((5.0, 3.9), (6.4, 3.9), (6.4, 4.1), (5.0, 4.1)))
        verdict = self.judge.judge(cover)
        self.assertFalse(verdict.valid)
        self.assertEqual(verdict.reason, "COVER_CLIP")
        self.assertEqual(verdict.conflict_entity, "cover:90")

    def test_region_version_is_stale_after_publish(self) -> None:
        editor = SceneEditor(self.manager)
        editor.apply_batch([{"op": "set_walkable", "x": 0, "z": 0, "walkable": False}])
        with self.assertRaises(ToolError) as caught:
            self.judge.judge(
                PlacementRequest(((0.2, 0.2), (0.8, 0.2), (0.8, 0.8), (0.2, 0.8)), requested_version=1)
            )
        self.assertEqual(caught.exception.code, "E_VERSION_STALE")


class TraceTests(unittest.TestCase):
    def _trace_fixture(self):
        scene = base_scene()
        manager = SceneManager(scene)
        picker = Picker(manager)
        judge = RegionJudge(manager)
        trace = TraceLog()
        request = center_pick((2.6, 1.0, 2.5))
        trace.append_pick(request, picker.pick(request))
        region_request = PlacementRequest(((0.2, 0.2), (0.8, 0.2), (0.8, 0.8), (0.2, 0.8)))
        trace.append_region(region_request, judge.judge(region_request))
        return manager, trace

    def test_trace_replay_and_no_timestamps(self) -> None:
        manager, trace = self._trace_fixture()
        data = trace.to_bytes()
        self.assertNotIn(b"timestamp", data.lower())
        parsed = TraceLog.from_bytes(data)
        replay = replay_trace(parsed, manager)
        self.assertEqual(replay["records"], 2)
        self.assertEqual(replay["picks"], 1)
        self.assertEqual(replay["regions"], 1)

    def test_truncation_and_hash_chain_break_are_rejected(self) -> None:
        _manager, trace = self._trace_fixture()
        data = trace.to_bytes()
        with self.assertRaises(ToolError) as caught:
            TraceLog.from_bytes(data[: len(data) // 2])
        self.assertEqual(caught.exception.code, "E_TRACE_INVALID")

        with self.assertRaises(ToolError) as caught:
            TraceLog.from_bytes(b"\n".join(data.splitlines()[:-1]) + b"\n")
        self.assertEqual(caught.exception.code, "E_TRACE_INVALID")

        lines = data.splitlines()
        record = json.loads(lines[0])
        record["result"]["hit"]["distance"] += 1.0
        lines[0] = json.dumps(record, sort_keys=True, separators=(",", ":")).encode()
        with self.assertRaises(ToolError) as caught:
            TraceLog.from_bytes(b"\n".join(lines) + b"\n")
        self.assertEqual(caught.exception.code, "E_TRACE_INVALID")


class InjectionAndDeterminismTests(unittest.TestCase):
    def test_all_injections_have_distinct_categories_and_points(self) -> None:
        result = injection_selfcheck()
        self.assertTrue(result["ok"])
        codes = {}
        for case in INJECTION_CASES:
            error = trigger_injection(case)
            self.assertEqual(error.code, EXPECTED_INJECTION_CODES[case])
            self.assertTrue(error.injection_point)
            codes[case] = error.code
        self.assertEqual(
            {codes["degenerate_bounds"], codes["zero_length_ray"], codes["out_of_bounds_screen"],
             codes["self_intersecting_region"], codes["corrupt_snapshot"]},
            {"E_SCENE_INVALID", "E_RAY_INVALID", "E_SCREEN_OUT_OF_RANGE", "E_REGION_INVALID", "E_SNAPSHOT_CORRUPT"},
        )

    def test_generation_and_regression_are_byte_deterministic(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            first = root / "samples1"
            second = root / "samples2"
            generate_samples(first, seed=777)
            generate_samples(second, seed=777)
            self.assertEqual(_directory_digest(first), _directory_digest(second))

            out1 = root / "out1"
            out2 = root / "out2"
            report1 = run_regression(first, out1, seed=777)
            report2 = run_regression(second, out2, seed=777)
            self.assertEqual(report1["totals"], report2["totals"])
            self.assertEqual(_directory_digest(out1), _directory_digest(out2))

    def test_cli_injection_exit_code_and_no_output(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            out = Path(temp) / "must-not-exist"
            completed = subprocess.run(
                [
                    sys.executable,
                    str(ROOT / "run_regression.py"),
                    "--inject",
                    "zero_length_ray",
                    "--out",
                    str(out),
                ],
                cwd=ROOT,
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(completed.returncode, 11)
            self.assertEqual(json.loads(completed.stdout)["business_code"], "E_RAY_INVALID")
            self.assertFalse(out.exists())


def _directory_digest(root: Path) -> dict[str, str]:
    result = {}
    for path in sorted(item for item in root.rglob("*") if item.is_file()):
        result[path.relative_to(root).as_posix()] = hashlib.sha256(path.read_bytes()).hexdigest()
    return result


if __name__ == "__main__":
    unittest.main()

