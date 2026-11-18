import hashlib
import json
import os
import signal
import subprocess
import sys
import time
import unittest

from opera_master.errors import EXIT_RESUME_MISMATCH
from opera_master.pipeline import InterruptController, render
from opera_master.snapshot import read_snapshot, snapshot_paths
from opera_master.wavio import read_wav

from .helpers import make_workspace

PROJECT_ROOT = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..")
)


def _subprocess_env():
    env = dict(os.environ)
    existing = env.get("PYTHONPATH", "")
    env["PYTHONPATH"] = (
        PROJECT_ROOT + (os.pathsep + existing if existing else "")
    )
    return env


def _hash(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


class PipelineTests(unittest.TestCase):
    def setUp(self):
        # Small blocks force many snapshot boundaries per render.
        self.tmp, self.cue_path, self.cue = make_workspace(block_samples=12000)

    def _master_path(self):
        return os.path.join(self.tmp, self.cue.output_master)

    def _report_path(self):
        return os.path.join(self.tmp, self.cue.output_report)
    def test_full_render_writes_report_and_master(self):
        result = render(self.cue, base_dir=self.tmp)
        self.assertFalse(result["interrupted"])
        self.assertTrue(os.path.isfile(self._master_path()))
        with open(self._report_path(), encoding="utf-8") as _fh:

            report = json.load(_fh)
        self.assertEqual(
            report["total_samples"],
            read_wav(self._master_path())[0].shape[0],
        )

    def test_report_deterministic_across_runs(self):
        render(self.cue, base_dir=self.tmp)
        with open(self._report_path(), encoding="utf-8") as _fh:

            first = json.load(_fh)
        render(self.cue, base_dir=self.tmp)
        with open(self._report_path(), encoding="utf-8") as _fh:

            second = json.load(_fh)
        first.pop("elapsed_seconds", None)
        second.pop("elapsed_seconds", None)
        self.assertEqual(first, second)

    def test_master_byte_identical_across_runs(self):
        render(self.cue, base_dir=self.tmp)
        first = _hash(self._master_path())
        render(self.cue, base_dir=self.tmp)
        self.assertEqual(first, _hash(self._master_path()))

    def test_sigkill_resume_produces_identical_master(self):
        env = _subprocess_env()
        env["OPERA_KILL_AFTER_BLOCK"] = "3"
        cmd = [sys.executable, "-m", "opera_master", "render", self.cue_path]
        killed = subprocess.Popen(
            cmd, cwd=self.tmp, env=env,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        killed.wait()
        self.assertEqual(killed.returncode, 137)
        snap = os.path.join(
            self.tmp, self.cue.snapshot_dir, "snapshot.json"
        )
        self.assertTrue(os.path.isfile(snap), "no snapshot was committed")
        before = read_snapshot(
            os.path.join(self.tmp, self.cue.snapshot_dir)
        )
        self.assertEqual(before["completed_blocks"], 3)

        # Resume must finish and produce the byte-identical one-shot master.
        render(self.cue, base_dir=self.tmp)
        resumed_hash = _hash(self._master_path())
        render(self.cue, base_dir=self.tmp)
        one_shot_hash = _hash(self._master_path())
        self.assertEqual(resumed_hash, one_shot_hash)

    def test_sigint_completes_block_and_exits_zero(self):
        env = _subprocess_env()
        env["OPERA_INTERRUPT_AFTER_BLOCK"] = "2"
        cmd = [sys.executable, "-m", "opera_master", "render", self.cue_path]
        proc = subprocess.Popen(
            cmd,
            cwd=self.tmp,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        out, err = proc.communicate(timeout=30)
        self.assertEqual(proc.returncode, 0, msg=err)
        self.assertIn("INTERRUPTED", out)
        recovery = os.path.join(
            self.cue.snapshot_dir, "recovery_report.json"
        )
        self.assertTrue(os.path.isfile(os.path.join(self.tmp, recovery)))

        # Resuming after a clean interrupt still matches a full render.
        render(self.cue, base_dir=self.tmp)
        resumed_hash = _hash(self._master_path())
        render(self.cue, base_dir=self.tmp)
        self.assertEqual(resumed_hash, _hash(self._master_path()))

    def test_resume_mismatch_discards_and_exits_with_business_code(self):
        # Render once so a committed snapshot exists.
        render(self.cue, base_dir=self.tmp)
        # Build a second, incompatible cue with its own snapshot directory.
        import copy

        with open(self.cue_path, encoding="utf-8") as _fh:


            raw = json.load(_fh)
        raw["seed"] = raw["seed"] + 1
        raw["snapshot_dir"] = os.path.join("out", "stale_snapshots")
        mismatch_path = os.path.join(self.tmp, "cue_mismatch.json")
        with open(mismatch_path, "w", encoding="utf-8") as fh:
            json.dump(raw, fh, ensure_ascii=False)
        from opera_master.cue import load_cue
        from opera_master.snapshot import write_snapshot

        mismatch_cue = load_cue(mismatch_path)
        # Plant a stale snapshot whose content matches the ORIGINAL cue.
        original = render(self.cue, base_dir=self.tmp)
        snap_dir = os.path.join(self.tmp, mismatch_cue.snapshot_dir)
        os.makedirs(snap_dir, exist_ok=True)
        write_snapshot(
            snap_dir,
            {
                "completed_blocks": 1,
                "total_blocks": 24,
                "block_samples": self.cue.block_samples,
                "total_samples": self.cue.total_samples,
                "cue_digest": self.cue.digest(),
                "cue_summary": self.cue.canonical_dict(),
                "output_master": mismatch_cue.output_master,
                "decoupling": original["analysis"]["decoupling"],
                "offsets": {
                    k: int(v)
                    for k, v in original["analysis"]["offsets"].items()
                },
                "micro_shifts": original["analysis"].get(
                    "micro_shifts", {}
                ),
                "limiter": {
                    "limiter_triggered_samples": original["analysis"][
                        "compliance"
                    ]["limiter_triggered_samples"],
                    "limiter_max_attenuation_db": original["analysis"][
                        "compliance"
                    ]["limiter_max_attenuation_db"],
                    "static_trim_db": original["analysis"]["compliance"][
                        "static_trim_db"
                    ],
                },
            },
        )
        from opera_master.errors import ResumeMismatchError

        with self.assertRaises(ResumeMismatchError) as ctx:
            render(mismatch_cue, base_dir=self.tmp)
        self.assertEqual(ctx.exception.exit_code, EXIT_RESUME_MISMATCH)
        # Old progress must have been discarded entirely.
        self.assertIsNone(read_snapshot(snap_dir))

    def test_interrupt_controller_flags_only_after_block(self):
        controller = InterruptController()
        controller.install()
        self.assertFalse(controller.requested)
        os.kill(os.getpid(), signal.SIGINT)
        self.assertTrue(controller.requested)
        controller.restore()


if __name__ == "__main__":
    unittest.main()
