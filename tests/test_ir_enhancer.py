"""端到端与单元测试：噪声估计、谱域抑制、门限保护、不变性、
量化验证、增量重算、恢复、并发与确定性。"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

from ir_enhancer import wavio
from ir_enhancer.config import (
    ChannelOverride,
    EnhanceConfig,
    GateParams,
    ProtectParams,
    SpectralParams,
)
from ir_enhancer.control import CancelToken, TaskManager
from ir_enhancer.engine import Engine
from ir_enhancer.errors import (
    BusyError,
    CancelledError,
    NoiseEstInvalidError,
    OnsetGainError,
    OnsetProtectedError,
    OnsetShiftError,
    RecoveryInvalidError,
)
from ir_enhancer.gating import apply_gate
from ir_enhancer.noise import detect_onset, estimate_noise
from ir_enhancer.onset_check import enforce_and_verify_onset
from ir_enhancer.samplegen import generate_ir, generate_sample


class TempDirMixin(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="ir_enhancer_test_"))
        self.addCleanup(lambda: shutil.rmtree(self.tmp, ignore_errors=True))

    def make_sample(self, seed=1, channels=6, seconds=1.0):
        return generate_sample(
            seed=seed, out_dir=self.tmp / "samples",
            channels=channels, seconds=seconds,
        )


class TestNoiseEstimation(TempDirMixin):
    def test_noise_floor_and_snr(self):
        data, truth = generate_ir(seed=3)
        config = EnhanceConfig()
        estimates, interval, onsets = estimate_noise(
            data, config.noise, config.metric.signal_window
        )
        self.assertEqual(interval[0], 0)
        self.assertGreater(interval[1], 4000)
        for ch, est in enumerate(estimates):
            self.assertAlmostEqual(est.rms, truth["noise_rms"][ch], delta=2e-4)
            self.assertGreater(est.snr_db, 10.0)
        self.assertTrue(np.all(onsets == truth["onset"]))

    def test_invalid_all_zero(self):
        data, _ = generate_ir(seed=4)
        data[:4800, 2] = 0.0  # 声道 2 预触发区间全为零
        config = EnhanceConfig()
        with self.assertRaises(NoiseEstInvalidError) as ctx:
            estimate_noise(data, config.noise, config.metric.signal_window)
        self.assertEqual(ctx.exception.code, "E_NOISE_EST_INVALID")
        self.assertEqual(ctx.exception.context["channel"], 2)
        self.assertIn("interval", ctx.exception.context)

    def test_invalid_short_interval(self):
        data, _ = generate_ir(seed=5, onset=200)  # 到达过早，预触发区间不足
        config = EnhanceConfig()
        with self.assertRaises(NoiseEstInvalidError) as ctx:
            estimate_noise(data, config.noise, config.metric.signal_window)
        self.assertEqual(ctx.exception.code, "E_NOISE_EST_INVALID")


class TestGateProtection(TempDirMixin):
    def test_protection_window_untouched(self):
        rng = np.random.default_rng(0)
        x = rng.standard_normal(8192) * 0.01
        x[4096] = 0.5
        config = EnhanceConfig()
        out = apply_gate(x, residual_rms=0.001, onset=4096, config=config, channel=0)
        pp = config.protect
        np.testing.assert_array_equal(
            out[4096 - pp.pre : 4096 + pp.post], x[4096 - pp.pre : 4096 + pp.post]
        )
        self.assertEqual(out[0], 0.0)  # 远低于门限的样本被置零

    def test_protection_violation_raises(self):
        rng = np.random.default_rng(1)
        x = rng.standard_normal(4096) * 0.01
        x[2048] = 0.5
        config = EnhanceConfig(protect=ProtectParams(modify_tol=-1.0))
        with self.assertRaises(OnsetProtectedError) as ctx:
            apply_gate(x, 0.001, 2048, config, 3)
        self.assertEqual(ctx.exception.code, "E_ONSET_PROTECTED")
        self.assertEqual(ctx.exception.context["channel"], 3)
        self.assertIn("sample", ctx.exception.context)


class TestOnsetInvariance(TempDirMixin):
    def test_shift_detected(self):
        data, _ = generate_ir(seed=6, channels=1)
        x = data[:, 0]
        shifted = x.copy()
        shifted[4000] = 0.9  # 保护窗口外人为制造提前到达
        config = EnhanceConfig()
        with self.assertRaises(OnsetShiftError) as ctx:
            enforce_and_verify_onset(x, shifted, 4800, 0.004, config, 0)
        self.assertEqual(ctx.exception.code, "E_ONSET_SHIFT")

    def test_gain_detected(self):
        data, _ = generate_ir(seed=7, channels=1)
        x = data[:, 0]
        config = EnhanceConfig(protect=ProtectParams(amp_tol=-1.0))
        with self.assertRaises(OnsetGainError) as ctx:
            enforce_and_verify_onset(x, x.copy(), 4800, 0.004, config, 0)
        self.assertEqual(ctx.exception.code, "E_ONSET_GAIN")


class TestPipelineEndToEnd(TempDirMixin):
    def test_onset_unchanged_and_noise_reduced(self):
        info = self.make_sample(seed=11)
        engine = Engine(self.tmp / "run")
        report = engine.run(info["wav"], EnhanceConfig())
        data_in, _ = wavio.read_wav(info["wav"])
        pub = engine.published.get()
        self.assertIsNotNone(pub)
        out = pub["data"]
        config = EnhanceConfig()
        estimates, interval, _ = estimate_noise(
            data_in, config.noise, config.metric.signal_window
        )
        p0, p1 = interval
        for ch in range(data_in.shape[1]):
            onset_in = detect_onset(data_in[:, ch], estimates[ch].rms)
            onset_out = detect_onset(out[:, ch], estimates[ch].rms)
            self.assertEqual(onset_in, onset_out, f"ch{ch} 直达声位置改变")
            e_in = float(np.sum(data_in[p0:p1, ch] ** 2))
            e_out = float(np.sum(out[p0:p1, ch] ** 2))
            self.assertLess(e_out, e_in, f"ch{ch} 残余噪声未下降")
        self.assertEqual(report["degraded_bands"], [])
        for rep in report["channels"]:
            self.assertEqual(len(rep["bands"]), len(config.bands))
            for band in rep["bands"]:
                self.assertGreaterEqual(band["snr_improvement_db"], 0.0)
                self.assertLessEqual(band["noise_energy_out"], band["noise_energy_in"])
        for rep in report["channels"]:
            self.assertLess(rep["residual_ratio_mean"], 1.0)

    def test_mono_supported(self):
        info = self.make_sample(seed=12, channels=1)
        engine = Engine(self.tmp / "run_mono")
        report = engine.run(info["wav"], EnhanceConfig())
        self.assertEqual(len(report["channels"]), 1)


class TestIncremental(TempDirMixin):
    def test_incremental_recompute_matches_offline(self):
        info = self.make_sample(seed=21)
        config = EnhanceConfig()
        engine = Engine(self.tmp / "inc")
        engine.run(info["wav"], config, trace_name="full.jsonl")
        base = engine.published.get()["data"].copy()

        cfg2 = config.with_channel_override(
            2, ChannelOverride(spectral=SpectralParams(over_subtract=2.0),
                               gate=GateParams(threshold_ratio=4.0))
        )
        cfg2 = cfg2.with_channel_override(
            5, ChannelOverride(protect=ProtectParams(post=512))
        )
        engine.run(info["wav"], cfg2, trace_name="inc.jsonl")
        inc = engine.published.get()["data"].copy()

        actions = {}
        with open(self.tmp / "inc" / "traces" / "inc.jsonl", encoding="utf-8") as fh:
            for line in fh:
                rec = json.loads(line)
                if rec["event"] == "channel":
                    actions[rec["channel"]] = rec["action"]
        self.assertEqual(actions[2], "recompute")
        self.assertEqual(actions[5], "recompute")
        for ch in (0, 1, 3, 4):
            self.assertEqual(actions[ch], "reuse")

        engine2 = Engine(self.tmp / "inc_fresh")
        engine2.run(info["wav"], cfg2, trace_name="fresh.jsonl")
        fresh = engine2.published.get()["data"]
        np.testing.assert_array_equal(inc, fresh)
        for ch in (0, 1, 3, 4):
            np.testing.assert_array_equal(inc[:, ch], base[:, ch])


class TestRecovery(TempDirMixin):
    def _run_subprocess(self, workdir, wav, extra):
        env = dict(os.environ)
        env["PYTHONPATH"] = str(ROOT)
        cmd = [
            sys.executable, "-m", "ir_enhancer", "run",
            "--in", str(wav), "--workdir", str(workdir),
            "--out", str(workdir / "export"),
        ] + extra
        return subprocess.run(cmd, env=env, capture_output=True, text=True)

    def test_crash_and_resume(self):
        info = self.make_sample(seed=31)
        workdir = self.tmp / "crash"
        proc = self._run_subprocess(workdir, info["wav"], ["--crash-after-channel", "2"])
        self.assertNotEqual(proc.returncode, 0)  # 强制终止
        self.assertTrue((workdir / "snapshots" / "snap_000002.json").exists())

        proc2 = self._run_subprocess(workdir, info["wav"], ["--resume"])
        self.assertEqual(proc2.returncode, 0, proc2.stderr)

        workdir2 = self.tmp / "clean"
        proc3 = self._run_subprocess(workdir2, info["wav"], [])
        self.assertEqual(proc3.returncode, 0, proc3.stderr)
        d1, _ = wavio.read_wav(workdir / "export" / "enhanced.wav")
        d2, _ = wavio.read_wav(workdir2 / "export" / "enhanced.wav")
        np.testing.assert_array_equal(d1, d2)

    def test_broken_chain_rejected(self):
        info = self.make_sample(seed=32)
        workdir = self.tmp / "broken"
        proc = self._run_subprocess(workdir, info["wav"], ["--crash-after-channel", "3"])
        self.assertNotEqual(proc.returncode, 0)
        os.remove(workdir / "snapshots" / "snap_000001.json")
        proc2 = self._run_subprocess(workdir, info["wav"], ["--resume"])
        self.assertEqual(proc2.returncode, 2)
        self.assertIn("E_RECOVERY_INVALID", proc2.stderr)

    def test_missing_field_rejected(self):
        info = self.make_sample(seed=33)
        workdir = self.tmp / "missing"
        proc = self._run_subprocess(workdir, info["wav"], ["--crash-after-channel", "1"])
        self.assertNotEqual(proc.returncode, 0)
        snap_path = workdir / "snapshots" / "snap_000001.json"
        snap = json.loads(snap_path.read_text(encoding="utf-8"))
        del snap["config_hash"]
        snap_path.write_text(json.dumps(snap), encoding="utf-8")
        proc2 = self._run_subprocess(workdir, info["wav"], ["--resume"])
        self.assertEqual(proc2.returncode, 2)
        self.assertIn("E_RECOVERY_INVALID", proc2.stderr)


class TestConcurrency(TempDirMixin):
    def test_busy(self):
        mgr = TaskManager()
        token = mgr.try_begin()
        with self.assertRaises(BusyError) as ctx:
            mgr.try_begin()
        self.assertEqual(ctx.exception.code, "E_BUSY")
        mgr.end(token)
        mgr.try_begin()

    def test_cancel_keeps_last_published(self):
        info = self.make_sample(seed=41)
        engine = Engine(self.tmp / "cancel")
        engine.run(info["wav"], EnhanceConfig())
        first = engine.published.get()["data"].copy()

        token = CancelToken()
        token.cancel()
        with self.assertRaises(CancelledError):
            engine.run(info["wav"], EnhanceConfig(), cancel_token=token,
                       trace_name="cancelled.jsonl")
        after = engine.published.get()["data"]
        np.testing.assert_array_equal(first, after)

    def test_concurrent_run_busy(self):
        mgr = TaskManager()
        token = mgr.try_begin()
        errors = []

        def worker():
            try:
                mgr.try_begin()
            except BusyError as exc:
                errors.append(exc.code)

        t = threading.Thread(target=worker)
        t.start()
        t.join()
        mgr.end(token)
        self.assertEqual(errors, ["E_BUSY"])


class TestDeterminism(TempDirMixin):
    def test_sample_and_trace_byte_identical(self):
        s1 = generate_sample(seed=77, out_dir=self.tmp / "a")
        s2 = generate_sample(seed=77, out_dir=self.tmp / "b")
        self.assertEqual(wavio.file_hash(s1["wav"]), wavio.file_hash(s2["wav"]))
        e1 = Engine(self.tmp / "r1")
        e2 = Engine(self.tmp / "r2")
        e1.run(s1["wav"], EnhanceConfig(), trace_name="t.jsonl")
        e2.run(s2["wav"], EnhanceConfig(), trace_name="t.jsonl")
        t1 = (self.tmp / "r1" / "traces" / "t.jsonl").read_bytes()
        t2 = (self.tmp / "r2" / "traces" / "t.jsonl").read_bytes()
        self.assertEqual(t1, t2)
        self.assertEqual(
            wavio.file_hash(self.tmp / "r1" / "published" / "v_000000" / "enhanced.wav"),
            wavio.file_hash(self.tmp / "r2" / "published" / "v_000000" / "enhanced.wav"),
        )


class TestAtomicExport(TempDirMixin):
    def test_export_contents(self):
        info = self.make_sample(seed=51)
        engine = Engine(self.tmp / "exp")
        engine.run(info["wav"], EnhanceConfig())
        paths = engine.export(self.tmp / "out")
        self.assertTrue(Path(paths["wav"]).exists())
        self.assertTrue(Path(paths["report"]).exists())
        report = json.loads(Path(paths["report"]).read_text(encoding="utf-8"))
        for field in ("noise_floor", "channels", "param_version",
                      "input_hash", "output_hash", "degraded_bands"):
            self.assertIn(field, report)
        self.assertFalse(list(Path(self.tmp / "out").glob("*.tmp")))


if __name__ == "__main__":
    unittest.main()
