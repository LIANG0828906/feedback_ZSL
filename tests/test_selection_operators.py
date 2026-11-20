import unittest

import numpy as np

from pcwb.cloud import PointCloud
from pcwb.errors import E_SELECTION_STALE, PointCloudError
from pcwb.format import encode_cloud, content_hash
from pcwb.operators import apply_operator, crop, downsample, smooth, color_cover
from pcwb.samples import generate_cloud
from pcwb.selection import (
    box_select, screen_rect_select, sphere_select, normalize_indices,
)
from pcwb.render.camera import OrbitCamera


def make_cloud(version: int = 1, seed: int = 11, size: int = 900):
    xyz, rgb = generate_cloud(seed, size)
    sha = content_hash(encode_cloud(xyz, rgb))
    return PointCloud.from_arrays(xyz, rgb, sha, version)


class SelectionTests(unittest.TestCase):
    def setUp(self):
        self.cloud = make_cloud()

    def test_sphere_and_box_selection(self):
        sphere = sphere_select(self.cloud, (0.0, 0.0, 0.0), 2.0)
        box = box_select(self.cloud, (-2, -2, -2), (2, 2, 2))
        self.assertEqual(sphere.kind, "sphere")
        self.assertEqual(box.kind, "box")
        self.assertEqual(sphere.indices.tolist(),
                         sorted(sphere.indices.tolist()))
        self.assertTrue(np.all(np.diff(sphere.indices) > 0))
        # sphere of radius 2 is contained in the +/-2 box
        self.assertLessEqual(sphere.count, box.count)
        bbox = sphere.bbox(self.cloud)
        self.assertEqual(bbox.shape, (6,))

    def test_selection_does_not_mutate_cloud(self):
        before = self.cloud.xyz.copy()
        sel = sphere_select(self.cloud, (0, 0, 0), 2.5)
        _ = color_cover(self.cloud, sel, rgb=(9, 9, 9))
        self.assertTrue(np.array_equal(before, self.cloud.xyz))
        self.assertFalse(self.cloud.xyz.flags.writeable)

    def test_stale_selection_rejected(self):
        sel = sphere_select(self.cloud, (0, 0, 0), 2.0)
        other = make_cloud(version=2, seed=12)
        with self.assertRaises(PointCloudError) as ctx:
            sel.check(other)
        self.assertEqual(ctx.exception.code, E_SELECTION_STALE)

    def test_screen_rect_select_uses_camera(self):
        camera = OrbitCamera()
        camera.fit(self.cloud.bbox)
        vp = camera.view_proj(640, 480)
        sel = screen_rect_select(self.cloud, vp, (-1.0, -1.0), (1.0, 1.0))
        self.assertGreater(sel.count, 0)
        self.assertLessEqual(sel.count, self.cloud.count)

    def test_normalize_indices_unique_sorted(self):
        out = normalize_indices(np.array([5, 1, 5, 2, 2]))
        self.assertEqual(out.tolist(), [1, 2, 5])


class OperatorTests(unittest.TestCase):
    def setUp(self):
        self.cloud = make_cloud()
        self.sel = sphere_select(self.cloud, (0, 0, 0), 2.2)

    def test_crop_keep_and_delete_complement(self):
        deleted = crop(self.cloud, self.sel, mode="delete")
        kept = crop(self.cloud, self.sel, mode="keep")
        self.assertEqual(
            deleted.xyz.shape[0] + kept.xyz.shape[0], self.cloud.count)
        self.assertEqual(deleted.record["operator"], "crop")
        self.assertEqual(deleted.record["input_version"], self.cloud.version)
        self.assertEqual(deleted.record["input_hash"], self.cloud.sha256)

    def test_color_cover_only_touches_selection(self):
        result = color_cover(self.cloud, self.sel, rgb=(7, 77, 177))
        self.assertTrue(
            np.all(result.rgb[self.sel.indices] == np.array([7, 77, 177])))
        untouched = np.ones(self.cloud.count, dtype=bool)
        untouched[self.sel.indices] = False
        self.assertTrue(
            np.array_equal(result.rgb[untouched], self.cloud.rgb[untouched]))

    def test_downsample_reduces_points_and_centroids(self):
        dense = sphere_select(self.cloud, (-2.5, -2.5, -2.5), 3.0)
        result = downsample(self.cloud, dense, voxel_size=1.0)
        self.assertLessEqual(result.xyz.shape[0], self.cloud.count)
        self.assertEqual(result.record["params"]["voxel_size"], 1.0)

    def test_smooth_moves_only_selected_points(self):
        result = smooth(self.cloud, self.sel, radius=0.9, iterations=1)
        moved = np.any(result.xyz != self.cloud.xyz, axis=1)
        moved_idx = np.nonzero(moved)[0]
        self.assertTrue(set(moved_idx.tolist()).issubset(
            set(self.sel.indices.tolist())))

    def test_operators_are_pure_and_byte_deterministic(self):
        for name, params in [
            ("crop", {"mode": "delete"}),
            ("color_cover", {"rgb": [200, 10, 10]}),
            ("downsample", {"voxel_size": 0.7}),
            ("smooth", {"radius": 0.7, "iterations": 1}),
        ]:
            r1 = apply_operator(name, self.cloud, self.sel, params)
            r2 = apply_operator(name, self.cloud, self.sel, params)
            self.assertTrue(np.array_equal(r1.xyz, r2.xyz), name)
            self.assertTrue(np.array_equal(r1.rgb, r2.rgb), name)
            self.assertEqual(r1.record, r2.record, name)
            # serialized output bytes must match exactly
            b1 = encode_cloud(r1.xyz, r1.rgb)
            b2 = encode_cloud(r2.xyz, r2.rgb)
            self.assertEqual(content_hash(b1), content_hash(b2), name)

    def test_different_params_change_output(self):
        r1 = downsample(self.cloud, self.sel, voxel_size=0.5)
        r2 = downsample(self.cloud, self.sel, voxel_size=1.5)
        self.assertNotEqual(
            content_hash(encode_cloud(r1.xyz, r1.rgb)),
            content_hash(encode_cloud(r2.xyz, r2.rgb)))


if __name__ == "__main__":
    unittest.main()
