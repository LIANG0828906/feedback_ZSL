"""要求 3/5/6/7/8：修复链、状态机、检查点、三路径一致、并发边界。"""

from __future__ import annotations

import json
import shutil
import tempfile
import unittest
from pathlib import Path

import numpy as np

from audiorest.core.engine import BoundedSemaphore, RestorationEngine
from audiorest.core.metrics import locate_first_diff
from audiorest.core.pcm import read_pcm
from audiorest.core.state import WindowState, WindowStateMachine
from audiorest.errors import WorkbenchError
from audiorest.formats.loader import load_project
from audiorest.generate_samples import generate

SAMPLES = Path(__file__).resolve().parents[1] / "samples"


class EngineTestBase(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        if not (SAMPLES / "manifest.json").exists():
            generate(SAMPLES)
        cls.manifest = json.loads((SAMPLES / "manifest.json").read_text(encoding="utf-8"))

    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp(prefix="audiorest_"))

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _load(self, project_id: str):
        item = next(p for p in self.manifest["projects"] if p["project_id"] == project_id)
        pdir = SAMPLES / project_id
        project = load_project(pdir / item["documents"]["3.0"])
        noisy = read_pcm(pdir / item["noisy_pcm"], project.audio.channels)
        clean = read_pcm(pdir / item["clean_pcm"], project.audio.channels)
        return item, pdir, project, noisy, clean


class PipelineAndParamTests(EngineTestBase):
    def test_para_out_of_range_names_field(self) -> None:
        _, _, project, noisy, _ = self._load("voice_mono")
        bad = project.with_runtime_params(subtraction_strength=99.0)
        with self.assertRaises(WorkbenchError) as ctx:
            RestorationEngine(
                bad, noisy, self.tmp / "ckpt",
                SAMPLES / "voice_mono" / "voice_mono.noisy.pcm",
            )
        self.assertEqual(ctx.exception.code, "E_PARA")
        self.assertIn("strength", ctx.exception.path)

    def test_gate_threshold_out_of_range(self) -> None:
        _, _, project, noisy, _ = self._load("voice_mono")
        bad = project.with_runtime_params(gate_threshold_db=6.0)
        with self.assertRaises(WorkbenchError) as ctx:
            RestorationEngine(
                bad, noisy, self.tmp / "ckpt",
                SAMPLES / "voice_mono" / "voice_mono.noisy.pcm",
            )
        self.assertEqual(ctx.exception.code, "E_PARA")
        self.assertIn("threshold_db", ctx.exception.path)

    def test_truth_bounds_satisfied_every_window(self) -> None:
        from audiorest.core.metrics import segment_stats

        item, pdir, project, noisy, clean = self._load("voice_mono")
        truth = json.loads((pdir / item["truth"]).read_text(encoding="utf-8"))
        engine = RestorationEngine(
            project, noisy, self.tmp / "ckpt", pdir / item["noisy_pcm"], max_workers=1
        )
        out = engine.run_full_recompute()
        for seg in truth["segments"]:
            start, end = seg["start_sample"], seg["end_sample"]
            local_silence = [
                (max(a - start, 0), b - start)
                for a, b in truth["silence_ranges"] if b > start and a < end
            ]
            stats = segment_stats(
                noisy[start:end], out[start:end], clean[start:end],
                local_silence, seg["window_index"],
            )
            self.assertGreaterEqual(stats.snr_gain_db, seg["snr_gain_db_min"])
            self.assertLessEqual(stats.residual_std, seg["residual_std_max"])


class StateMachineTests(unittest.TestCase):
    def test_legal_and_illegal_transitions(self) -> None:
        machine = WindowStateMachine(2)
        machine.transition(0, WindowState.PROCESSING)
        machine.transition(0, WindowState.COMMITTED)
        with self.assertRaises(WorkbenchError) as ctx:
            machine.transition(0, WindowState.PROCESSING)
        self.assertEqual(ctx.exception.code, "E_STATE")
        # 失败可重试回待处理
        machine.transition(1, WindowState.PROCESSING)
        machine.transition(1, WindowState.FAILED)
        machine.transition(1, WindowState.PENDING)
        self.assertEqual(machine.progress, (1, 2))


class CheckpointAndPathTests(EngineTestBase):
    def _engine(self, project, noisy, pcm_path, name, workers):
        return RestorationEngine(
            project, noisy, self.tmp / name, pcm_path,
            max_workers=workers, buffer_capacity_samples=48000,
        )

    def test_three_paths_sample_identical(self) -> None:
        item, pdir, project, noisy, _ = self._load("voice_mono")
        pcm = pdir / item["noisy_pcm"]

        serial = self._engine(project, noisy, pcm, "serial", 1)
        serial_out = serial.run_incremental()

        parallel = self._engine(project, noisy, pcm, "parallel", 3)
        parallel_out = parallel.run_incremental()
        np.testing.assert_array_equal(serial_out, parallel_out)

        full = self._engine(project, noisy, pcm, "full", 1)
        full_out = full.run_full_recompute()
        np.testing.assert_array_equal(serial_out, full_out)

        # 断点路径：提交一半后停止，新引擎加载检查点续跑
        realtime = self._engine(project, noisy, pcm, "realtime", 1)
        half = max(len(realtime.windows) // 2, 1)
        seen = 0

        def stop_half(index, progress):
            nonlocal seen
            seen += 1
            if seen >= half:
                realtime.stop()

        realtime.run_incremental(on_window=stop_half)
        resume_engine = self._engine(project, noisy, pcm, "realtime", 2)
        resume_out = resume_engine.resume()
        np.testing.assert_array_equal(serial_out, resume_out)

    def test_checkpoint_invalidation_on_version_mismatch(self) -> None:
        item, pdir, project, noisy, _ = self._load("sfx_mono")
        pcm = pdir / item["noisy_pcm"]
        engine = self._engine(project, noisy, pcm, "ckpt", 1)
        engine.run_incremental()
        manifest_path = self.tmp / "ckpt" / "manifest.json"
        doc = json.loads(manifest_path.read_text(encoding="utf-8"))
        doc["param_version"] = "0.0.0-tampered"
        manifest_path.write_text(json.dumps(doc), encoding="utf-8")

        reloaded = self._engine(project, noisy, pcm, "ckpt", 1)
        self.assertIsNone(reloaded._store.load_valid(len(reloaded.windows)))
        # 作废后必须全量重算，且结果与首次一致
        first = self._engine(project, noisy, pcm, "fresh", 1).run_full_recompute()
        again = reloaded.run_full_recompute()
        np.testing.assert_array_equal(first, again)

    def test_committed_window_param_freeze(self) -> None:
        item, pdir, project, noisy, _ = self._load("voice_mono")
        pcm = pdir / item["noisy_pcm"]
        engine = self._engine(project, noisy, pcm, "freeze", 1)
        first = engine.windows[0]
        engine.run_incremental()
        frozen = engine.snapshots[0].samples.copy()
        # 已提交后再改参数，窗口 0 的快照不得变化
        engine.update_runtime_params(strength=3.0, gate_threshold_db=-70.0)
        np.testing.assert_array_equal(engine.snapshots[0].samples, frozen)
        self.assertEqual(engine.state_machine.state(first.index), WindowState.COMMITTED)

    def test_e_diff_locates_sample_and_window(self) -> None:
        item, pdir, project, noisy, _ = self._load("voice_mono")
        engine = self._engine(project, noisy, pdir / item["noisy_pcm"], "diff", 1)
        out = engine.run_incremental()
        tampered = out.copy()
        tampered[30000, 0] += 0.25
        located = locate_first_diff(out, tampered, engine.window_starts)
        self.assertEqual(located["sample_index"], 30000)
        self.assertEqual(located["window_index"], 1)

    def test_checkpoint_files_serial_no_interleave(self) -> None:
        item, pdir, project, noisy, _ = self._load("ambience_stereo")
        engine = self._engine(
            project, noisy, pdir / item["noisy_pcm"], "ordered", 4
        )
        engine.run_incremental()
        manifest = json.loads(
            (self.tmp / "ordered" / "manifest.json").read_text(encoding="utf-8")
        )
        indices = [entry["window_index"] for entry in manifest["windows"]]
        self.assertEqual(indices, sorted(indices))
        # 不存在任何 .tmp 残留，说明每次原子替换都完成
        leftovers = list((self.tmp / "ordered").glob("*.tmp*"))
        self.assertEqual(leftovers, [])


class BufferBoundaryTests(unittest.TestCase):
    def test_bounded_semaphore_blocks_instead_of_dropping(self) -> None:
        sem = BoundedSemaphore(capacity_samples=100)
        sem.acquire(60)
        acquired = []

        import threading

        def worker():
            sem.acquire(50)  # 必须阻塞，直到前面释放
            acquired.append(True)
            sem.release(50)

        thread = threading.Thread(target=worker)
        thread.start()
        thread.join(timeout=0.2)
        self.assertEqual(acquired, [])  # 缓冲耗尽时排队等待
        sem.release(60)
        thread.join(timeout=0.5)
        self.assertEqual(acquired, [True])


if __name__ == "__main__":
    unittest.main()
