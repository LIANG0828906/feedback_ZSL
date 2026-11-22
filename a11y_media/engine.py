"""分段推进引擎: 实时处理 / 断点恢复 / 全量重算三条路径。

- 检测与窗口推导为确定性纯函数, 全工程一次计算并缓存;
- 每个固定时长分段(chunk)独立完成字符排程与误差统计, 提交边界清晰;
- 工作线程池并行, 限制最大并行度(max_workers)与在途缓冲(buffer_limit);
- 单段异常 -> 该段 FAILED, 可重试, 不污染其他段;
- 已提交分段结果冻结并写入检查点, 参数调整只作用于未提交分段;
- 强制终止后重启只能依据已持久化且版本匹配的分段恢复。
"""

from __future__ import annotations

import dataclasses
from concurrent.futures import FIRST_COMPLETED, ThreadPoolExecutor, wait

from . import pipeline, statemachine
from .checkpoint import CheckpointStore
from .loader import load_envelope
from .model import Project


class ProjectResult:
    """不可变结果快照, 供纯函数式查询。"""

    def __init__(self, windows, char_starts, report, progress):
        self.windows = tuple(tuple(w) for w in windows)
        self.char_starts = tuple(tuple(s) for s in char_starts)
        self.report = report
        self.progress = tuple(progress)

    def as_dict(self) -> dict:
        return {
            "windows": [list(w) for w in self.windows],
            "char_starts": [list(s) for s in self.char_starts],
            "report": self.report,
            "progress": list(self.progress),
        }


class _Crash(Exception):
    """模拟强制终止(测试用)。"""


class Engine:
    def __init__(
        self,
        project: Project,
        store: CheckpointStore | None = None,
        max_workers: int = 2,
        buffer_limit: int = 4,
        fault_injector=None,
    ):
        self.project = project
        self.store = store
        self.max_workers = max(1, max_workers)
        self.buffer_limit = max(1, buffer_limit)
        self.fault_injector = fault_injector
        self.tracker = statemachine.SegmentTracker(project.num_chunks)
        self._detected = None
        self._windows = None
        self._chunk_results: dict = {}
        self._chunk_errors: dict = {}
        self._speed = project.assist.speed
        self._weights_override = None
        self._restored = False

    def update_assist(self, speed=None, char_weights=None) -> None:
        """调整语速 / 权重: 只作用于未提交分段, 已提交分段结果冻结。"""
        if speed is not None:
            self._speed = speed
        if char_weights is not None:
            self._weights_override = char_weights

    def _ensure_common(self) -> None:
        if self._detected is None:
            envelope = load_envelope(self.project)
            self._detected = pipeline.compute_detection(self.project, envelope)
            self._windows = pipeline.compute_windows(self.project, self._detected)

    def _effective_project(self) -> Project:
        p = self.project
        if self._weights_override is not None or self._speed != p.assist.speed:
            weights = (
                self._weights_override
                if self._weights_override is not None
                else p.assist.char_weights
            )
            p = dataclasses.replace(
                p,
                assist=dataclasses.replace(
                    p.assist, speed=self._speed, char_weights=weights
                ),
            )
        return p

    def _compute_chunk(self, chunk: int) -> dict:
        if self.fault_injector is not None:
            self.fault_injector(chunk)
        proj = self._effective_project()
        lo = chunk * self.project.segment_duration
        hi = lo + self.project.segment_duration
        subs = [i for i, w in enumerate(self._windows) if lo <= w[0] < hi]
        entries = []
        for i in subs:
            starts = pipeline.compute_sentence_schedule(proj, i, self._windows[i])
            err = pipeline.sentence_error_entry(
                proj, i, self._detected, self._windows[i], starts
            )
            entries.append(
                {
                    "sub": i,
                    "window": list(self._windows[i]),
                    "char_starts": list(starts),
                    "error": err,
                }
            )
        return {"chunk": chunk, "subs": entries}

    def _commit(self, chunk: int, result: dict, persist: bool) -> None:
        self.tracker.transition(chunk, statemachine.COMMITTED)
        self._chunk_results[chunk] = result
        if persist and self.store is not None:
            self.store.save_chunk(self.project.content_hash, chunk, result)

    def restore(self) -> bool:
        """从检查点恢复已提交分段。返回是否命中快照。"""
        if self._restored:
            return False
        self._restored = True
        if self.store is None:
            return False
        snapshot = self.store.load_snapshot(self.project.content_hash)
        if not snapshot:
            return False
        for chunk, result in snapshot.items():
            self.tracker.mark_committed_from_snapshot(chunk)
            self._chunk_results[chunk] = result
        return True

    def run(self, stop_after=None, crash_after=None, persist=True,
            resume=True) -> ProjectResult:
        """推进直到全部提交或达到 stop_after / crash_after。"""
        self._ensure_common()
        if resume:
            self.restore()
        committed_this_run = 0
        pending = [
            c for c in range(self.project.num_chunks)
            if self.tracker.state(c) != statemachine.COMMITTED
        ]
        idx = 0
        with ThreadPoolExecutor(max_workers=self.max_workers) as pool:
            inflight = {}
            while idx < len(pending) or inflight:
                # 在途任务数受 buffer_limit 限制(每线程缓冲上限)
                while idx < len(pending) and len(inflight) < self.buffer_limit:
                    if stop_after is not None and committed_this_run >= stop_after:
                        idx = len(pending)
                        break
                    chunk = pending[idx]
                    idx += 1
                    self.tracker.transition(chunk, statemachine.PROCESSING)
                    inflight[pool.submit(self._compute_chunk, chunk)] = chunk
                if not inflight:
                    break
                done, _ = wait(inflight, return_when=FIRST_COMPLETED)
                for fut in done:
                    chunk = inflight.pop(fut)
                    try:
                        result = fut.result()
                    except Exception as exc:  # 单段异常不污染其他段
                        self.tracker.transition(chunk, statemachine.FAILED)
                        self._chunk_errors[chunk] = str(exc)
                        continue
                    self._commit(chunk, result, persist)
                    committed_this_run += 1
                    if crash_after is not None and committed_this_run >= crash_after:
                        raise _Crash(f"模拟强制终止于 {committed_this_run} 段之后")
        return self._assemble()

    def clear_failed(self) -> list:
        """清除失败记录; FAILED 段会在下一次 run 中经 FAILED->PROCESSING 合法重试。"""
        failed = sorted(self._chunk_errors)
        self._chunk_errors.clear()
        return failed

    def _assemble(self) -> ProjectResult:
        n = len(self.project.subtitles)
        windows: list = [None] * n
        starts: list = [None] * n
        entries: list = [None] * n
        for result in self._chunk_results.values():
            for item in result["subs"]:
                i = item["sub"]
                windows[i] = tuple(item["window"])
                starts[i] = tuple(item["char_starts"])
                entries[i] = item["error"]
        if any(w is None for w in windows):
            missing = [i for i, w in enumerate(windows) if w is None]
            report = {"incomplete": True, "missing_subs": missing}
            return ProjectResult(
                [w if w else (0, 0) for w in windows],
                [s if s else () for s in starts],
                report,
                self.tracker.progress(),
            )
        proj = self._effective_project()
        report = pipeline.finalize_report(proj, entries, windows)
        return ProjectResult(windows, starts, report, self.tracker.progress())


def run_full(project: Project) -> ProjectResult:
    """一次性全量重算路径: 不读不写检查点。"""
    engine = Engine(project, store=None)
    return engine.run(persist=False, resume=False)
