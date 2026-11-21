import hashlib
import os
import shutil
import tempfile
import unittest

from hdrworkbench.experiment import ExperimentRunner, TrajectoryLog
from hdrworkbench.generator import SampleSpec, write_sample
from hdrworkbench.hdrformat import CS_REC2020, TF_PQ
from hdrworkbench.pipeline import ConversionConfig
from hdrworkbench.snapshot import CrashInjectedError, recover_snapshot
from hdrworkbench import color


def _file_hash(path):
    with open(path, "rb") as handle:
        return hashlib.sha256(handle.read()).hexdigest()


class ExperimentDeterminismTests(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp()
        self.sample = os.path.join(self.root, "sample.hdr")
        write_sample(
            self.sample,
            SampleSpec(8, 6, 3, 10, CS_REC2020, TF_PQ, 1000, seed=99),
        )

    def tearDown(self):
        shutil.rmtree(self.root, ignore_errors=True)

    def _run(self, work_dir):
        log = TrajectoryLog(os.path.join(work_dir, "trajectory.jsonl"))
        runner = ExperimentRunner(self.sample, work_dir, log)
        cfg = ConversionConfig(
            target_colorspace=1,
            target_transfer=1,
            target_bit_depth=10,
            target_peak_nits=100,
            tone_curve=color.TONE_REINHARD,
            mode="fixed",
            frac_bits=12,
            seed=5,
        )
        runner.run_version(cfg)
        runner.run_version(
            ConversionConfig(
                **{**cfg.factor_dict(), "target_peak_nits": 200}
            )
        )
        runner.run_version(
            ConversionConfig(
                **{**cfg.factor_dict(), "target_peak_nits": 200}
            )
        )
        log.close()
        return runner

    def test_byte_identical_trajectory_and_frames(self):
        dir_a = os.path.join(self.root, "a")
        dir_b = os.path.join(self.root, "b")
        runner_a = self._run(dir_a)
        runner_b = self._run(dir_b)
        self.assertEqual(
            _file_hash(os.path.join(dir_a, "trajectory.jsonl")),
            _file_hash(os.path.join(dir_b, "trajectory.jsonl")),
        )
        self.assertEqual(
            runner_a.store.history(), runner_b.store.history()
        )

    def test_crash_recovery_completes_version(self):
        work = os.path.join(self.root, "work")
        snap = os.path.join(work, "snap")
        log = TrajectoryLog(os.path.join(work, "trajectory.jsonl"))
        runner = ExperimentRunner(self.sample, work, log)
        cfg = ConversionConfig(
            target_colorspace=1,
            target_transfer=1,
            target_bit_depth=10,
            target_peak_nits=100,
            tone_curve=color.TONE_HABLE,
            mode="fixed",
            frac_bits=12,
        )
        with self.assertRaises(CrashInjectedError):
            runner.run_version(cfg, snapshot_dir=snap, crash_after=0)
        recovered = recover_snapshot(snap)
        self.assertEqual(recovered["committed_frames"], [0])
        runner.run_version(cfg, snapshot_dir=snap)
        log.close()
        version = runner.store.current()
        self.assertEqual(len(version.frames), 3)


class GuiSmokeTests(unittest.TestCase):
    def test_offscreen_window_instantiates(self):
        os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
        from PySide6 import QtWidgets

        from hdrworkbench.gui import WorkbenchWindow

        app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
        root = tempfile.mkdtemp()
        sample = os.path.join(root, "sample.hdr")
        write_sample(
            sample,
            SampleSpec(6, 4, 2, 10, CS_REC2020, TF_PQ, 1000, seed=3),
        )
        window = WorkbenchWindow(root, sample)
        window._run()
        self.assertIn("当前版本", window.info.text())
        app.processEvents()


if __name__ == "__main__":
    unittest.main()

