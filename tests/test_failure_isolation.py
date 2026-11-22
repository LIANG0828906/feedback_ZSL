import os
import unittest
from unittest import mock

from afpipe import pipeline as pmod
from afpipe.builder import IndexBuildManager
from afpipe.errors import DecodeError
from afpipe.indexstore import IndexStore

from . import CONFIG, MANIFEST, fresh_dir


class FailureIsolationTest(unittest.TestCase):
    def test_one_bad_file_does_not_pollute_others(self):
        root = fresh_dir("isolation")
        manager = IndexBuildManager(
            os.path.join(root, "w"), os.path.join(root, "idx"),
            CONFIG, MANIFEST,
        )
        target = "gB_long_mp3"
        original = pmod.process_file
        calls = {"n": 0}

        def flaky(media, manifest, cfg):
            if media.file_id == target:
                calls["n"] += 1
                if calls["n"] == 1:
                    raise DecodeError("simulated transient decoder fault")
            return original(media, manifest, cfg)

        with mock.patch.object(pmod, "process_file", flaky), \
                mock.patch("afpipe.builder.process_file", flaky):
            report = manager.build(retries=0)

        # The failing file is recorded, everything else still indexed.
        self.assertIn(target, report.failed_files)
        store = IndexStore(os.path.join(root, "idx")).load(
            CONFIG, manifest_fingerprint=MANIFEST.fingerprint)
        indexed = {e.file_id for e in store.entries}
        self.assertNotIn(target, indexed)
        self.assertIn("gA_long_wav", indexed)
        self.assertIn("gC_long_mp3", indexed)

        # Retry after "fix": the same file now succeeds and joins the index.
        report2 = IndexBuildManager(
            os.path.join(root, "w2"), os.path.join(root, "idx2"),
            CONFIG, MANIFEST,
        ).build(retries=1)
        self.assertEqual(report2.failed_files, [])
        store2 = IndexStore(os.path.join(root, "idx2")).load(
            CONFIG, manifest_fingerprint=MANIFEST.fingerprint)
        self.assertIn(target, {e.file_id for e in store2.entries})


if __name__ == "__main__":
    unittest.main()
