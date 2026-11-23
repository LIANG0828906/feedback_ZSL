"""修复处理引擎：窗口状态机 + 检查点 + 有界并发，保证三路径逐样本一致。"""

from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

import numpy as np

from ..formats.common import sha256_file
from ..model import ALGO_VERSION, PARAM_VERSION, RestoreProject
from .checkpoint import CheckpointStore, WindowSnapshot
from .pipeline import process_window, validate_params
from .state import WindowState, WindowStateMachine


@dataclass(frozen=True)
class WindowSlice:
    index: int
    start: int
    end: int


def build_windows(total_samples: int, sample_rate: int, window_seconds: float) -> list[WindowSlice]:
    length = max(int(round(sample_rate * window_seconds)), 1)
    slices: list[WindowSlice] = []
    start = 0
    index = 0
    while start < total_samples:
        end = min(start + length, total_samples)
        slices.append(WindowSlice(index, start, end))
        start = end
        index += 1
    return slices


def _param_snapshot(project: RestoreProject) -> dict:
    return {
        "framing": project.canonical()["framing"],
        "stages": project.canonical()["stages"],
        "seed": project.seed,
    }


class BoundedSemaphore:
    """每线程采样缓冲上限的资源闸门：耗尽时阻塞排队，不丢弃窗口。"""

    def __init__(self, capacity_samples: int):
        self._capacity = capacity_samples
        self._used = 0
        self._condition = threading.Condition()

    def acquire(self, samples: int) -> None:
        with self._condition:
            while self._used + samples > self._capacity:
                self._condition.wait()
            self._used += samples

    def release(self, samples: int) -> None:
        with self._condition:
            self._used -= samples
            self._condition.notify_all()

    @property
    def used(self) -> int:
        return self._used


class RestorationEngine:
    """承载一次工程的窗口状态、检查点与处理生命周期。"""

    def __init__(
        self,
        project: RestoreProject,
        noisy_samples: np.ndarray,
        checkpoint_dir: str | Path,
        audio_path: str | Path,
        *,
        max_workers: int = 2,
        buffer_capacity_samples: int | None = None,
    ):
        validate_params(project)
        if max_workers < 1:
            raise ValueError("max_workers 必须 >= 1")
        self.project = project
        self.noisy = noisy_samples
        self.checkpoint_dir = Path(checkpoint_dir)
        self.max_workers = max_workers
        self.windows = build_windows(
            noisy_samples.shape[0], project.audio.sample_rate, project.window_seconds
        )
        capacity = buffer_capacity_samples or max_workers * self.windows[0].end * 4
        self._buffer = BoundedSemaphore(max(capacity, 1))
        self.state_machine = WindowStateMachine(len(self.windows))
        source_hash = sha256_file(audio_path)
        self._store = CheckpointStore(
            self.checkpoint_dir, project, source_content_hash=source_hash
        )
        self._committed: dict[int, np.ndarray] = {}
        self._snapshots: dict[int, WindowSnapshot] = {}
        self._commit_lock = threading.Lock()
        self._pause = threading.Event()
        self._stop = threading.Event()
        self._next_commit = 0

    # ---- 参数调整：只作用于未提交窗口（已提交窗口冻结参数快照）----
    def update_runtime_params(self, *, strength: float | None = None,
                              gate_threshold_db: float | None = None) -> RestoreProject:
        candidate = self.project.with_runtime_params(
            subtraction_strength=strength, gate_threshold_db=gate_threshold_db
        )
        validate_params(candidate)
        self.project = candidate
        return candidate

    @property
    def progress(self) -> tuple[int, int]:
        return self.state_machine.progress

    def pause(self) -> None:
        self._pause.set()

    def resume_processing(self) -> None:
        self._pause.clear()

    def stop(self) -> None:
        self._stop.set()
        self._pause.clear()

    def _wait_if_paused(self) -> bool:
        """返回 True 表示被要求停止。"""
        while self._pause.is_set() and not self._stop.is_set():
            self._pause.wait(timeout=0.1)
        return self._stop.is_set()

    def _process_one(self, slice_: WindowSlice) -> tuple[WindowSlice, WindowSnapshot]:
        if self._wait_if_paused():
            raise RuntimeError("stopped")
        segment = self.noisy[slice_.start:slice_.end]
        self._buffer.acquire(segment.size)
        try:
            result = process_window(segment, self.project)
        finally:
            self._buffer.release(segment.size)
        snapshot = WindowSnapshot(
            window_index=slice_.index,
            samples=result.samples,
            noise_spectrum=result.noise_spectrum,
            content_hash=result.content_hash,
            source_version=self.project.source_version,
            param_version=PARAM_VERSION,
            algo_version=ALGO_VERSION,
            param_snapshot=_param_snapshot(self.project),
        )
        return slice_, snapshot

    def _commit_in_order(self, ordered_results) -> bool:
        """串行、按窗口编号顺序提交并写检查点。返回 False 表示被停止。"""
        pending: dict[int, WindowSnapshot] = {}
        for slice_, snapshot in ordered_results:
            pending[slice_.index] = snapshot
            while self._next_commit in pending:
                if self._wait_if_paused():
                    return False
                expected = self._next_commit
                snap = pending.pop(expected)
                with self._commit_lock:
                    self.state_machine.transition(expected, WindowState.PROCESSING)
                    self._store.commit_window(snap, len(self.windows))
                    self._snapshots[expected] = snap
                    self._committed[expected] = snap.samples
                    self.state_machine.transition(expected, WindowState.COMMITTED)
                self._next_commit += 1
        return True

    def run_incremental(
        self,
        *,
        on_window: Callable[[int, tuple[int, int]], None] | None = None,
    ) -> np.ndarray:
        """实时/断点路径：逐个提交并落检查点，支持暂停/停止后重启续跑。"""
        if not self._committed:
            self._try_resume()
        remaining = [w for w in self.windows if w.index not in self._committed]
        if not remaining:
            return self._assemble()

        if self.max_workers == 1 or len(remaining) == 1:
            for slice_ in remaining:
                if self._wait_if_paused():
                    return self._assemble()
                _, snapshot = self._process_one(slice_)
                self._commit_in_order(iter([(slice_, snapshot)]))
                if on_window:
                    on_window(slice_.index, self.progress)
        else:
            with ThreadPoolExecutor(max_workers=self.max_workers) as pool:
                futures = [pool.submit(self._process_one, slice_) for slice_ in remaining]
                def generator():
                    for future in futures:
                        yield future.result()
                completed = self._commit_in_order(generator())
                if not completed:
                    return self._assemble()
            for slice_ in remaining:
                if on_window:
                    on_window(slice_.index, self.progress)
        return self._assemble()

    def run_full_recompute(
        self,
        *,
        on_window: Callable[[int, tuple[int, int]], None] | None = None,
    ) -> np.ndarray:
        """一次性全量重算：先整体作废快照，再走与实时路径相同的提交逻辑。"""
        self._store.reset()
        self._committed.clear()
        self._snapshots.clear()
        self._next_commit = 0
        self.state_machine = WindowStateMachine(len(self.windows))
        return self.run_incremental(on_window=on_window)

    def _try_resume(self) -> dict[int, WindowSnapshot] | None:
        snapshots = self._store.load_valid(len(self.windows))
        if snapshots is None:
            return None
        self._next_commit = len(snapshots)
        for index, snapshot in sorted(snapshots.items()):
            self.state_machine.transition(index, WindowState.PROCESSING)
            self._snapshots[index] = snapshot
            self._committed[index] = snapshot.samples
            self.state_machine.transition(index, WindowState.COMMITTED)
        return snapshots

    def resume(self, **kwargs) -> np.ndarray:
        """重启续跑入口：校验旧快照，有效则复用，无效则全量重算。"""
        snapshots = self._store.load_valid(len(self.windows))
        if snapshots is None:
            return self.run_full_recompute(**kwargs)
        # 有效快照：恢复已提交窗口状态并对齐提交游标，再只处理剩余窗口
        self._try_resume()
        return self.run_incremental(**kwargs)

    def _assemble(self) -> np.ndarray:
        if not self._committed:
            return np.empty((0, self.noisy.shape[1]), dtype=np.float64)
        parts = [self._committed[index] for index in sorted(self._committed)]
        return np.concatenate(parts, axis=0)

    def export_restored(self, out_path: str | Path) -> Path:
        from .pcm import write_pcm

        samples = self._assemble()
        if samples.shape[0] != self.noisy.shape[0]:
            raise RuntimeError("尚有窗口未提交，无法按时间轴导出完整音频")
        out_path = Path(out_path)
        out_path.parent.mkdir(parents=True, exist_ok=True)
        write_pcm(out_path, samples)
        return out_path

    @property
    def window_starts(self) -> list[int]:
        return [w.start for w in self.windows]

    @property
    def snapshots(self) -> dict[int, WindowSnapshot]:
        return dict(self._snapshots)
