import json
import os
import tempfile
import unittest

import numpy as np

from afpipe.config import Config
from afpipe.ffmpeg import decode
from afpipe.samples import DEFAULT_SEED, generate


class CorpusGenerationTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp(prefix="afpipe-corpus-")
        cls.manifest = generate(cls.tmp, seed=DEFAULT_SEED)
        cls.second_root = tempfile.mkdtemp(prefix="afpipe-corpus2-")
        cls.second_manifest = generate(cls.second_root, seed=DEFAULT_SEED)
        cls.cfg = Config()

    def _decoded(self, root, rel_path):
        return decode(
            os.path.join(root, rel_path),
            target_rate=self.cfg.sample_rate,
            channel_mode=self.cfg.channel_mode,
            max_channels=self.cfg.max_channels,
            decode_buf_frames=self.cfg.decode_buf_frames,
        )

    def test_deterministic_content(self):
        for group_a, group_b in zip(self.manifest["groups"].values(),
                                    self.second_manifest["groups"].values()):
            for fa, fb in zip(group_a["files"], group_b["files"]):
                if fa["container"] == "ogg":
                    # libvorbis container headers are not byte-stable across
                    # runs, but the decoded audio content is identical.
                    pa = self._decoded(self.tmp, fa["path"])
                    pb = self._decoded(self.second_root, fb["path"])
                    np.testing.assert_array_equal(pa.samples, pb.samples)
                else:
                    self.assertEqual(fa["sha256"], fb["sha256"])

    def test_every_group_has_pcm_and_lossy(self):
        for group in self.manifest["groups"].values():
            kinds = {f["format_kind"] for f in group["files"]}
            self.assertEqual(kinds, {"pcm", "lossy"})

    def test_markers_and_segments_present(self):
        for group in self.manifest["groups"].values():
            self.assertTrue(group["segments"])
            for f in group["files"]:
                self.assertTrue(f["markers"])
                for marker in f["markers"]:
                    self.assertLess(marker["start_s"], marker["end_s"])

    def test_manifest_file_written(self):
        path = os.path.join(self.tmp, "manifest.json")
        self.assertTrue(os.path.isfile(path))
        with open(path, "rb") as fh:
            json.loads(fh.read())

    def test_short_query_and_long_master_relationship(self):
        for query in self.manifest["queries"].values():
            group = self.manifest["groups"][query["group_id"]]
            target = next(s for s in group["segments"]
                          if s["segment_id"] == query["target_segment"])
            self.assertLess(target["start_s"], target["end_s"])
            for f in query["files"]:
                self.assertLessEqual(f["duration_s"],
                                     group["files"][0]["duration_s"])


if __name__ == "__main__":
    unittest.main()
