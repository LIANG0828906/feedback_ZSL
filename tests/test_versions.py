import json
import os
import tempfile
import unittest
from unittest import mock

import numpy as np

from pcwb.errors import E_COMMIT_FAILED, E_UNKNOWN_VERSION, PointCloudError
from pcwb.format import read_cloud
from pcwb.samples import generate_cloud
from pcwb.versions import VersionStore


class VersionStoreTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.store = VersionStore(os.path.join(self.tmp.name, "store"))
        self.xyz, self.rgb = generate_cloud(3, 400)

    def tearDown(self):
        self.tmp.cleanup()

    def test_monotonic_versions_and_manifest(self):
        v1 = self.store.commit(self.xyz, self.rgb, record={"operator": "import"})
        v2 = self.store.commit(self.xyz + 1.0, self.rgb,
                               record={"operator": "shift"})
        self.assertEqual((v1.version, v2.version), (1, 2))
        self.assertEqual(self.store.head_version, 2)
        with open(self.store.manifest_path, "rb") as handle:
            manifest = json.loads(handle.read())
        self.assertEqual(manifest["head"], 2)
        self.assertEqual([e["version"] for e in manifest["versions"]], [1, 2])
        self.assertTrue(
            all(os.path.exists(os.path.join(self.store.versions_dir,
                                            e["filename"]))
                for e in manifest["versions"]))

    def test_committed_file_matches_manifest_hash(self):
        cloud = self.store.commit(self.xyz, self.rgb)
        path = os.path.join(self.store.versions_dir, "v000001.pclb")
        _, _, _, disk_sha = read_cloud(path)
        self.assertEqual(disk_sha, cloud.sha256)
        self.assertEqual(self.store.entry(1).sha256, disk_sha)

    def test_reopen_store_persists_history(self):
        self.store.commit(self.xyz, self.rgb, record={"operator": "import"})
        self.store.commit(self.xyz + 2.0, self.rgb, record={"operator": "shift"})
        reopened = VersionStore(self.store.root)
        self.assertEqual(reopened.head_version, 2)
        cloud = reopened.head()
        self.assertEqual(cloud.count, self.xyz.shape[0])
        self.assertTrue(np.allclose(cloud.xyz, self.xyz + 2.0, atol=1e-6))

    def test_commit_failure_keeps_previous_version(self):
        v1 = self.store.commit(self.xyz, self.rgb, record={"operator": "import"})
        before_files = sorted(os.listdir(self.store.versions_dir))
        with mock.patch.object(
                self.store, "_write_manifest", side_effect=OSError("disk full")):
            with self.assertRaises(PointCloudError) as ctx:
                self.store.commit(self.xyz + 5.0, self.rgb)
        self.assertEqual(ctx.exception.code, E_COMMIT_FAILED)
        self.assertEqual(self.store.head_version, 1)
        self.assertEqual(sorted(os.listdir(self.store.versions_dir)),
                         before_files)
        # previous version still loads and bytes are intact
        recovered = self.store.load(1)
        self.assertEqual(recovered.sha256, v1.sha256)
        self.assertEqual(os.listdir(self.store.tmp_dir), [])

    def test_staging_failure_cleans_tmp(self):
        with mock.patch("builtins.open", side_effect=OSError("io error")):
            with self.assertRaises(PointCloudError) as ctx:
                self.store.commit(self.xyz, self.rgb)
        self.assertEqual(ctx.exception.code, E_COMMIT_FAILED)
        self.assertIsNone(self.store.head_version)

    def test_unknown_version(self):
        with self.assertRaises(PointCloudError) as ctx:
            self.store.load(99)
        self.assertEqual(ctx.exception.code, E_UNKNOWN_VERSION)

    def test_checkout_invalidates_branch_cache(self):
        self.store.commit(self.xyz, self.rgb)
        self.store.commit(self.xyz + 1.0, self.rgb)
        v3 = self.store.commit(self.xyz + 2.0, self.rgb)
        _ = self.store.load(3)  # populate cache
        self.assertIn(3, self.store._cache)
        head = self.store.checkout(1)
        self.assertEqual(head.version, 1)
        self.assertEqual(self.store.head_version, 1)
        self.assertEqual(set(self.store._cache), {1})
        # historical files remain available for diffing
        self.assertTrue(os.path.exists(
            os.path.join(self.store.versions_dir, "v000003.pclb")))
        old_v3 = self.store.load(3)
        self.assertEqual(old_v3.sha256, v3.sha256)

    def test_hash_mismatch_on_tampered_file_detected(self):
        self.store.commit(self.xyz, self.rgb)
        path = os.path.join(self.store.versions_dir, "v000001.pclb")
        with open(path, "r+b") as handle:
            handle.seek(90)
            handle.write(b"\xff\xff\xff")
        self.store.invalidate_cache()
        with self.assertRaises(PointCloudError) as ctx:
            self.store.load(1)
        self.assertEqual(ctx.exception.code, E_COMMIT_FAILED)


if __name__ == "__main__":
    unittest.main()
