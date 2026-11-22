import filecmp
import json
import os
import unittest

from afpipe.builder import IndexBuildManager

from . import CONFIG, MANIFEST, fresh_dir


class ParallelEquivalenceTest(unittest.TestCase):
    def test_parallel_and_serial_builds_are_byte_identical(self):
        serial_root = fresh_dir("eq-serial")
        parallel_root = fresh_dir("eq-parallel")
        serial_cfg = CONFIG.with_overrides(max_workers=1)
        parallel_cfg = CONFIG.with_overrides(max_workers=4)

        IndexBuildManager(
            os.path.join(serial_root, "w"),
            os.path.join(serial_root, "idx"),
            serial_cfg, MANIFEST,
        ).build()
        IndexBuildManager(
            os.path.join(parallel_root, "w"),
            os.path.join(parallel_root, "idx"),
            parallel_cfg, MANIFEST,
        ).build()

        reg_s = os.path.join(serial_root, "idx", "registry.json")
        reg_p = os.path.join(parallel_root, "idx", "registry.json")
        with open(reg_s, "rb") as fh:
            bytes_s = fh.read()
        with open(reg_p, "rb") as fh:
            bytes_p = fh.read()
        self.assertEqual(bytes_s, bytes_p)

        with open(reg_s) as fh:
            registry = json.load(fh)
        for entry in registry["entries"]:
            self.assertTrue(filecmp.cmp(
                os.path.join(serial_root, "idx", "postings",
                             f"{entry['entry_id']}.json"),
                os.path.join(parallel_root, "idx", "postings",
                             f"{entry['entry_id']}.json"),
                shallow=False,
            ))

    def test_manifest_order_is_preserved(self):
        root = fresh_dir("eq-order")
        IndexBuildManager(
            os.path.join(root, "w"), os.path.join(root, "idx"),
            CONFIG.with_overrides(max_workers=4), MANIFEST,
        ).build()
        with open(os.path.join(root, "idx", "registry.json")) as fh:
            registry = json.load(fh)
        self.assertEqual(
            [e["file_id"] for e in registry["entries"]],
            [f.file_id for f in MANIFEST.index_files()],
        )


if __name__ == "__main__":
    unittest.main()
