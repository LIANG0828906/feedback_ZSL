import json
import os
import signal
import subprocess
import sys
import time
import unittest

from afpipe.builder import IndexBuildManager
from afpipe.checkpoint import CheckpointStore
from afpipe.hashing import atomic_write_json

from . import CONFIG, MANIFEST, MANIFEST_PATH, fresh_dir


class CheckpointResumeTest(unittest.TestCase):
    def test_resume_reuses_completed_files_only(self):
        work = fresh_dir("resume-w")
        idx1 = os.path.join(work, "idx1")
        idx2 = os.path.join(work, "idx2")
        manager = IndexBuildManager(work, idx1, CONFIG, MANIFEST)
        first = manager.build()
        self.assertFalse(first.resumed)

        again = IndexBuildManager(work, idx2, CONFIG, MANIFEST).build()
        self.assertTrue(again.resumed)
        self.assertTrue(all(o.status == "reused" for o in again.outcomes))
        with open(os.path.join(idx1, "registry.json"), "rb") as fh:
            bytes_one = fh.read()
        with open(os.path.join(idx2, "registry.json"), "rb") as fh:
            bytes_two = fh.read()
        self.assertEqual(bytes_one, bytes_two)

    def test_tampered_shard_invalidates_whole_snapshot(self):
        work = fresh_dir("tamper-w")
        IndexBuildManager(work, os.path.join(work, "idx"),
                          CONFIG, MANIFEST).build()
        state_path = os.path.join(work, "checkpoint", "state.json")
        with open(state_path) as fh:
            state = json.load(fh)
        fid = next(iter(state["files"]))
        state["files"][fid]["shard_hashes"][0] = "sh-deadbeef"
        atomic_write_json(state_path, state)

        store = CheckpointStore(work, CONFIG, MANIFEST.fingerprint)
        reusable = store.load()
        self.assertEqual(reusable, {})
        self.assertIsNotNone(store.invalidation_record())

    def test_parameter_change_invalidates_snapshot(self):
        work = fresh_dir("param-w")
        IndexBuildManager(work, os.path.join(work, "idx"),
                          CONFIG, MANIFEST).build()
        changed = CONFIG.with_overrides(quant_bits=4)
        store = CheckpointStore(work, changed, MANIFEST.fingerprint)
        self.assertEqual(store.load(), {})
        self.assertEqual(
            store.invalidation_record()["reason"],
            "parameter/algorithm/manifest identity mismatch",
        )

    def test_sigkill_then_resume_completes_index(self):
        work = fresh_dir("kill-w")
        idx = os.path.join(work, "idx")
        proc = subprocess.Popen(
            [sys.executable, "-c",
             f"""
from afpipe.config import Config
from afpipe.manifest import load_manifest
from afpipe.builder import IndexBuildManager
cfg=Config(max_workers=1, shard_frames=64).validate()
m=load_manifest({MANIFEST_PATH!r})
IndexBuildManager({work!r}, {idx!r}, cfg, m).build()
"""],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        time.sleep(0.5)
        proc.send_signal(signal.SIGKILL)
        proc.wait()
        self.assertNotEqual(proc.returncode, 0)

        report = IndexBuildManager(work, idx, CONFIG, MANIFEST).build()
        self.assertEqual(report.failed_files, [])
        with open(os.path.join(idx, "registry.json")) as fh:
            registry = json.load(fh)
        self.assertEqual(len(registry["entries"]),
                         len(MANIFEST.index_files()))


if __name__ == "__main__":
    unittest.main()
