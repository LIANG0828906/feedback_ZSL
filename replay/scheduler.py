"""回合切片并行调度器。

- 每场对局是一个 ReplayTask，任务内部回合严格顺序执行，任务之间并行；
- 全局资源池以 max_concurrency 限制同时运行的任务数（槽位即并发配额）；
- 可运行任务按提交序号 seq 稳定排序后分配槽位；
- 维护 WAITING / RUNNING / PAUSED / COMPLETED / CANCELLED / FAILED 状态；
- 重复提交同一 match_id 返回 E_BUSY；
- 全局轨迹日志在 shutdown 时按确定性键排序落盘，保证逐字节一致。
"""

from __future__ import annotations

import threading
from pathlib import Path

from .config import FrozenConfig
from .errors import E_BUSY, E_NOT_FOUND, OK, ReplayError
from .hashing import write_text_atomic
from .model import MatchDescription
from .task import (
    CANCELLED,
    COMPLETED,
    FAILED,
    PAUSED,
    RUNNING,
    TERMINAL_STATES,
    WAITING,
    ReplayTask,
)


class ReplayScheduler:
    def __init__(
        self,
        config: FrozenConfig,
        output_root: str | Path,
        turn_pause_seconds: float = 0.02,
    ) -> None:
        self.config = config
        self.output_root = Path(output_root)
        self.output_root.mkdir(parents=True, exist_ok=True)
        self._tasks: dict[str, ReplayTask] = {}
        self._order: list[str] = []
        self._slots = threading.BoundedSemaphore(config.max_concurrency)
        self._lock = threading.RLock()
        self._threads: dict[str, threading.Thread] = {}
        self._shutdown = False
        self._submit_seq = 0
        self._turn_pause_seconds = turn_pause_seconds

    # ---- 提交与查询 ----
    def submit(
        self, desc: MatchDescription, recover: bool = False
    ) -> tuple[str, ReplayTask | None]:
        with self._lock:
            if self._shutdown:
                raise ReplayError(E_BUSY, "调度器已关闭，拒绝新任务")
            existing = self._tasks.get(desc.match_id)
            if existing is not None and not existing.is_terminal():
                return E_BUSY, existing
            if existing is not None and existing.is_terminal():
                # 终态任务允许重新提交（恢复/重跑），释放旧记录占位。
                self._tasks.pop(desc.match_id, None)
                self._threads.pop(desc.match_id, None)
            seq = self._submit_seq
            self._submit_seq += 1
            task = ReplayTask(seq, desc, self.config, self.output_root)
            task.turn_pause_seconds = self._turn_pause_seconds
            self._tasks[desc.match_id] = task
            if desc.match_id not in self._order:
                self._order.append(desc.match_id)
            thread = threading.Thread(
                target=self._worker,
                args=(task, recover),
                name=f"replay-{desc.match_id}",
                daemon=True,
            )
            self._threads[desc.match_id] = thread
            thread.start()
            return OK, task

    def get_task(self, match_id: str) -> ReplayTask:
        task = self._tasks.get(match_id)
        if task is None:
            raise ReplayError(E_NOT_FOUND, f"任务不存在: {match_id}")
        return task

    def statuses(self) -> list[dict]:
        with self._lock:
            tasks = [self._tasks[mid] for mid in self._order if mid in self._tasks]
        return [task.snapshot_status() for task in tasks]

    def transitions(self, match_id: str) -> dict:
        """状态转换查询：返回当前状态与允许的下一状态集合。"""
        task = self.get_task(match_id)
        current = task.status
        allowed: dict[str, list[str]] = {
            WAITING: [RUNNING, CANCELLED],
            RUNNING: [PAUSED, RUNNING, CANCELLED, COMPLETED, FAILED],
            PAUSED: [RUNNING, CANCELLED],
            COMPLETED: [],
            CANCELLED: [],
            FAILED: [RUNNING, CANCELLED],
        }
        return {
            "match_id": match_id,
            "current": current,
            "allowed_next": allowed.get(current, []),
            "terminal": current in TERMINAL_STATES,
        }

    # ---- 控制 ----
    def pause(self, match_id: str) -> None:
        self.get_task(match_id).request_pause()

    def resume_task(self, match_id: str) -> None:
        self.get_task(match_id).resume()

    def cancel(self, match_id: str) -> None:
        self.get_task(match_id).request_cancel()

    def wait_all(self) -> None:
        with self._lock:
            threads = list(self._threads.values())
        for thread in threads:
            thread.join()

    def shutdown(self) -> None:
        """停止调度器并释放文件句柄；已提交磁盘快照保持不变。"""
        with self._lock:
            if self._shutdown:
                return
            self._shutdown = True
            tasks = list(self._tasks.values())
        for task in tasks:
            task.request_cancel()
            task.resume()
        for task in tasks:
            task.flush_trace()
        with self._lock:
            threads = list(self._threads.values())
        for thread in threads:
            thread.join(timeout=5)
        self.write_global_trace()

    # ---- 内部执行 ----
    def _worker(self, task: ReplayTask, recover: bool) -> None:
        # 全局资源池：获取不到槽位即在队列等待；公平信号量按 FIFO 放行，
        # 任务选择由 submit 的稳定序号决定，保证调度顺序确定。
        self._slots.acquire()
        try:
            task.execute(recover=recover)
        finally:
            self._slots.release()

    def write_global_trace(self) -> Path:
        """汇总各任务轨迹，按 (match_id, turn, observation) 排序后落盘。"""
        lines = [
            "# match_id|observation|source_of_truth|turn|status|"
            "compute_total|memory_total|state_hash|event_prefix_hash\n"
        ]
        entries: list[tuple[str, object]] = []
        for match_id in self._order:
            task = self._tasks[match_id]
            for record in task.trace:
                entries.append((match_id, record))
        entries.sort(key=lambda item: (item[0], item[1].turn, item[1].observation))
        for match_id, record in entries:
            base = record.to_line().rstrip("\n")
            lines.append(f"{match_id}|{base}\n")
        return write_text_atomic(self.output_root / "global_trace.log", "".join(lines))
