"""端到端编排：并行调度、全局归一屏障、检查点与导出拼接。

两条执行路径共用同一套 DSP 与归一代码：

* ``parallel``：ThreadPoolExecutor 并行做单段 DSP，归一在顺序屏障内串行
* ``serial``：max_workers=1 的同一调度循环（全量重算）

确定性来源：DSP 为纯函数、float64 计算、无随机量、归一带顺序锁，
因此两条路径与恢复路径产出逐样本一致。
"""
from __future__ import annotations
import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass

import numpy as np

from ..audio.io import content_hash
from ..core.config import PipelineConfig
from ..core.errors import BenchError, CancelledError
from ..core.segment import ProgramSegment
from ..core.states import SegmentState
from .checkpoint import CheckpointStore, SegmentCheckpoint
from .normalizer import GlobalNormalizer, SegmentGainRecord
from .processor import ProcessedSegment, process_segment_dsp


@dataclass
class SegmentOutput:
    index: int
    fs: int
    output_samples: np.ndarray
    gain_record: SegmentGainRecord
    loudness: dict
    gain_curve: np.ndarray
    input_hash: str


class PipelineOrchestrator:
    def __init__(self, segments: list[ProgramSegment],
                 config: PipelineConfig, checkpoint_dir: str):
        self.segments = segments
        self.config = config
        self.checkpoints = CheckpointStore(checkpoint_dir, config)
        frame_cfg = config.frame
        self.normalizer = GlobalNormalizer(
            config.normalize, frame_cfg, config.limiter.ceiling_db)
        self._cancel = threading.Event()
        self._cp_lock = threading.Lock()       # 检查点串行提交
        # 资源预算：在途 DSP 占用样本数不得超过
        # max_workers * per_worker_buffer_samples
        self._buffer_slots = threading.BoundedSemaphore(
            self.config.scheduler.max_workers)
        self._buffer_budget = (
            self.config.scheduler.per_worker_buffer_samples)
        self.outputs: dict[int, SegmentOutput] = {}
        self.processed: dict[int, ProcessedSegment] = {}

    # ---- 进度 --------------------------------------------------------
    def progress(self) -> tuple[int, int]:
        committed = sum(1 for s in self.segments
                        if s.state is SegmentState.COMMITTED)
        return committed, len(self.segments)

    def cancel(self) -> None:
        self._cancel.set()

    def _check_cancel(self) -> None:
        if self._cancel.is_set():
            raise CancelledError("用户取消处理")

    # ---- 恢复 --------------------------------------------------------
    def restore(self) -> bool:
        expected = [content_hash(s.samples) for s in self.segments]
        restored = self.checkpoints.restore_or_reset(expected)
        if not restored:
            return False
        for cp in restored:
            seg = self.segments[cp.segment_index]
            seg.state_machine.transition(SegmentState.PROCESSING)
            seg.state_machine.transition(SegmentState.COMMITTED)
            record = SegmentGainRecord(**cp.gain_record)
            self.normalizer.submit_restored(cp.segment_index, record)
            self.outputs[cp.segment_index] = SegmentOutput(
                index=cp.segment_index, fs=seg.sample_rate,
                output_samples=cp.output_samples, gain_record=record,
                loudness=cp.loudness, gain_curve=cp.gain_curve,
                input_hash=cp.input_hash)
        manifest = self.checkpoints.read_manifest()
        self.normalizer.state.accumulated_energy = float(
            manifest.get("accumulated_energy", 0.0))
        self.normalizer.state.accumulated_weight = int(
            manifest.get("accumulated_weight", 0))
        return True

    # ---- 单段任务 ----------------------------------------------------
    def _run_dsp(self, seg: ProgramSegment) -> ProcessedSegment:
        self._check_cancel()
        # 每线程缓冲上限：按段样本数占一个预算槽位，超限段在获取槽位前
        # 即被 process_segment_dsp 的显式校验拦截为 E_PARA
        if seg.samples.shape[0] > self._buffer_budget:
            from ..core.errors import ParameterError
            raise ParameterError(
                f"段 {seg.index} 样本数 {seg.samples.shape[0]} 超过每线程"
                f"缓冲上限 {self._buffer_budget}",
                field="per_worker_buffer_samples")
        self._buffer_slots.acquire()
        # 依赖条件：仅 PENDING 可进入 PROCESSING
        try:
            if seg.state is not SegmentState.PENDING:
                raise BenchError(f"段 {seg.index} 不处于待处理状态")
            seg.state_machine.transition(SegmentState.PROCESSING)
            result = process_segment_dsp(
                seg.index, seg.samples, seg.sample_rate, self.config)
            return result
        except Exception as exc:
            self._fail(seg, str(exc))
            raise
        finally:
            self._buffer_slots.release()

    @staticmethod
    def _fail(seg: ProgramSegment, reason: str) -> None:
        if seg.state is SegmentState.PROCESSING:
            seg.state_machine.transition(SegmentState.FAILED, reason)
            seg.error = reason

    def _commit(self, processed: ProcessedSegment) -> SegmentOutput:
        """全局归一屏障：仅在主线程按段序调用，天然满足依赖顺序。"""
        seg = self.segments[processed.index]
        if self.normalizer.submitted_count != processed.index:
            raise BenchError(
                f"归一依赖未满足：期望提交段 "
                f"{self.normalizer.submitted_count}，实际 {processed.index}")
        out_audio, record = self.normalizer.submit(
            processed.index, processed.pre_norm_samples, processed.fs)

        loudness = {
            "integrated_lufs": processed.input_loudness.integrated_lufs,
            "true_peak_db": processed.input_loudness.true_peak_db,
            "limited_tp_db": processed.limited_tp_db,
            "frame_count": processed.input_loudness.frame_count,
            "gated_frame_count":
                processed.input_loudness.gated_frame_count,
        }
        cp = SegmentCheckpoint(
            segment_index=processed.index,
            input_hash=processed.input_hash,
            output_samples=out_audio,
            loudness=loudness,
            gain_record=record.__dict__.copy(),
            gain_curve=processed.total_dsp_gain_curve,
        )
        # 检查点串行原子提交
        with self._cp_lock:
            self.checkpoints.save_segment(cp)
            committed = sorted(self.outputs.keys() | {processed.index})
            hashes = [content_hash(s.samples) for s in self.segments]
            self.checkpoints.write_manifest(
                hashes, committed,
                accumulated_energy=self.normalizer.state.accumulated_energy,
                accumulated_weight=self.normalizer.state.accumulated_weight)

        seg.state_machine.transition(SegmentState.COMMITTED)
        output = SegmentOutput(
            index=processed.index, fs=processed.fs,
            output_samples=out_audio, gain_record=record,
            loudness=loudness,
            gain_curve=processed.total_dsp_gain_curve,
            input_hash=processed.input_hash)
        self.outputs[processed.index] = output
        self.processed.pop(processed.index, None)
        return output

    # ---- 执行入口 ----------------------------------------------------
    def run(self, parallel: bool = True) -> list[SegmentOutput]:
        workers = self.config.scheduler.max_workers if parallel else 1
        pending = [s for s in self.segments
                   if s.state is SegmentState.PENDING]
        if not pending:
            return self._ordered_outputs()

        with ThreadPoolExecutor(max_workers=workers) as pool:
            # 阶段一：单段 DSP 在线程池并行（受 max_workers 与缓冲预算约束）
            futures = {pool.submit(self._run_dsp, seg): seg
                       for seg in pending}
            index_to_future = {seg.index: f for f, seg in futures.items()}
            # 阶段二：仅对本轮调度的段按段序串行归一提交；
            # 某段失败时 future.result() 抛异常，后续段保持 PROCESSING，
            # 不污染已提交结果，可在 retry_failed 中继续。
            for index in sorted(index_to_future):
                future = index_to_future[index]
                processed = future.result()  # 段失败在此传播并隔离
                # 阶段二：全局归一严格按段序串行提交
                self.processed[index] = processed
                self._commit(processed)
        return self._ordered_outputs()

    def retry_failed(self, parallel: bool = True) -> list[SegmentOutput]:
        """重试失败段及失败时滞留的后续段，不触碰已提交段。"""
        for seg in self.segments:
            if seg.state is SegmentState.FAILED:
                seg.state_machine.transition(SegmentState.PROCESSING)
                seg.state_machine.transition(SegmentState.PENDING)
                seg.error = None
            elif seg.state is SegmentState.PROCESSING:
                # 失败发生时尚未提交的后续段，回退到 PENDING 重新调度
                seg.state_machine.transition(SegmentState.PENDING)
        return self.run(parallel=parallel)

    def _ordered_outputs(self) -> list[SegmentOutput]:
        return [self.outputs[i] for i in sorted(self.outputs)]

    # ---- 报告与导出 --------------------------------------------------
    def build_report(self) -> dict:
        ok, overall_lufs = self.normalizer.verify_target()
        return {
            "segments": [
                {
                    "index": o.index,
                    **o.loudness,
                    "gain": o.gain_record.__dict__,
                }
                for o in self._ordered_outputs()
            ],
            "overall_integrated_lufs": overall_lufs,
            "target_lufs": self.config.normalize.target_lufs,
            "within_tolerance": ok,
            "committed": self.progress()[0],
            "total": self.progress()[1],
        }

    def concatenated_output(self) -> tuple[int, np.ndarray]:
        """按时间轴顺序拼接。

        混合采样率统一重采样到最高采样率；单声道上混为立体声
        （段内输出仍严格保持原始声道数，仅最终拼接导出统一布局）。
        """
        ordered = self._ordered_outputs()
        target_fs = max(o.fs for o in ordered)
        blocks = []
        for o in ordered:
            block = o.output_samples
            if o.fs != target_fs:
                from scipy import signal
                block = signal.resample_poly(
                    block.astype(np.float64), target_fs, o.fs, axis=0)
            if block.ndim == 1:
                block = np.stack([block, block], axis=1)
            elif block.shape[1] == 1:
                block = np.repeat(block, 2, axis=1)
            blocks.append(block)
        return target_fs, np.concatenate(blocks, axis=0)
