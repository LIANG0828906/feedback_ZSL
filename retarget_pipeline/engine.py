"""后台重定向引擎：单任务并发控制、协作式取消与快照提交。

约束
----
* 解析/重定向/校验在工作线程执行，调用线程（GUI 主线程）不阻塞；
* 同一动画同一时刻只允许一个重定向任务，重复提交返回
  :data:`~retarget_pipeline.errors.E_BUSY`；
* 取消仅在逐帧检查点生效，正在执行的不可中断数学步骤不会被声称终止；
* 取消或异常后关闭文件句柄（``with`` 保证），并保留最后一次已提交快照。
"""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from typing import Callable

from .errors import E_BUSY, E_CANCELLED, PipelineError
from .mapping import JointMapping
from .model import AnimDocument
from .retarget import retarget_frames
from .snapshot import ProjectState, commit_frame, init_project
from .validate import ValidationReport, validate_retarget

ProgressCallback = Callable[["TaskProgress"], None]


@dataclass
class TaskProgress:
    task_id: str
    frame: int
    total_frames: int
    phase: str
    message: str = ""


@dataclass
class TaskHandle:
    task_id: str
    thread: threading.Thread
    cancel_event: threading.Event
    state_holder: dict = field(default_factory=dict)

    def cancel(self) -> bool:
        """请求取消；返回 True 表示已发出信号（不声称步骤已终止）。"""

        if not self.thread.is_alive():
            return False
        self.cancel_event.set()
        return True

    @property
    def done(self) -> bool:
        return not self.thread.is_alive()


class RetargetEngine:
    """每个动画键（默认源文件路径）至多一个运行任务的引擎。"""

    def __init__(self) -> None:
        self._locks: dict[str, threading.Lock] = {}
        self._global = threading.Lock()
        self._tasks: dict[str, TaskHandle] = {}

    def _lock_for(self, animation_key: str) -> threading.Lock:
        with self._global:
            lock = self._locks.get(animation_key)
            if lock is None:
                lock = threading.Lock()
                self._locks[animation_key] = lock
            return lock

    def submit(
        self,
        task_id: str,
        source: AnimDocument,
        target: AnimDocument,
        mapping: JointMapping,
        project_dir: str,
        tolerance_rad: float,
        animation_key: str | None = None,
        on_progress: ProgressCallback | None = None,
        on_done: Callable[[dict], None] | None = None,
    ) -> TaskHandle:
        """提交后台任务；动画忙时抛 :data:`E_BUSY`。"""

        key = animation_key or (source.source_path or task_id)
        lock = self._lock_for(key)
        if not lock.acquire(blocking=False):
            raise PipelineError(
                E_BUSY,
                f"动画 {key} 已有重定向任务在执行",
                [{"animation_key": key, "task_id": task_id}],
            )

        cancel_event = threading.Event()
        holder: dict = {}

        def worker() -> None:
            state: ProjectState | None = None
            try:
                if on_progress:
                    on_progress(TaskProgress(task_id, 0, source.frame_count, "parse"))
                state = init_project(project_dir, source.version, target.version,
                                     mapping.content_hash())

                def cancel_check() -> bool:
                    return cancel_event.is_set()

                if on_progress:
                    on_progress(TaskProgress(task_id, 0, source.frame_count, "retarget"))

                # 逐帧消费生成器：帧边界检查取消，单帧数学求解不可中断；
                # 每帧求解完成立即提交快照，取消时保留此前所有已提交帧。
                collected = []
                for frame_result in retarget_frames(source, target, mapping):
                    if cancel_event.is_set():
                        raise PipelineError(E_CANCELLED, "任务在帧检查点被取消",
                                            [{"frame": frame_result.frame}])
                    collected.append(frame_result)
                    # E_POSE_UNRESOLVED 帧只标记不落盘：不推进快照链。
                    if frame_result.resolved:
                        state, _hash = commit_frame(
                            project_dir, state, frame_result,
                            mapping.content_hash(), source.version, target.version,
                        )
                    if on_progress:
                        on_progress(TaskProgress(
                            task_id, frame_result.frame + 1, source.frame_count,
                            "retarget",
                            "skipped" if not frame_result.resolved else "committed",
                        ))
                from .model import RetargetResult

                result = RetargetResult(
                    target_names=target.names,
                    frames=collected,
                    mapping_hash=mapping.content_hash(),
                    source_version=source.version,
                    target_version=target.version,
                )

                if on_progress:
                    on_progress(TaskProgress(task_id, source.frame_count,
                                             source.frame_count, "validate"))
                report: ValidationReport = validate_retarget(
                    source, target, mapping, result, tolerance_rad
                )
                holder["status"] = "completed"
                holder["result"] = result
                holder["report"] = report
                holder["last_committed_frame"] = state.last_committed_frame
                if on_done:
                    on_done({"status": "completed", "task_id": task_id,
                             "report": report.to_dict()})
            except PipelineError as exc:
                holder["status"] = "cancelled" if exc.code == E_CANCELLED else "error"
                holder["error"] = exc.to_dict()
                holder["last_committed_frame"] = (
                    state.last_committed_frame if state is not None else -1
                )
                if on_done:
                    on_done({"status": holder["status"], "task_id": task_id,
                             "error": exc.to_dict(),
                             "last_committed_frame": holder["last_committed_frame"]})
            except Exception as exc:  # pragma: no cover - 防御性兜底
                holder["status"] = "error"
                holder["error"] = {"code": "E_UNEXPECTED", "message": str(exc),
                                   "locations": []}
                holder["last_committed_frame"] = (
                    state.last_committed_frame if state is not None else -1
                )
                if on_done:
                    on_done({"status": "error", "task_id": task_id,
                             "error": holder["error"],
                             "last_committed_frame": holder["last_committed_frame"]})
            finally:
                lock.release()

        thread = threading.Thread(target=worker, name=f"retarget-{task_id}", daemon=True)
        handle = TaskHandle(task_id=task_id, thread=thread,
                            cancel_event=cancel_event, state_holder=holder)
        self._tasks[task_id] = handle
        thread.start()
        return handle

    def wait(self, handle: TaskHandle, timeout: float | None = None) -> bool:
        """阻塞等待任务结束（测试用），返回是否已结束。"""

        handle.thread.join(timeout)
        return not handle.thread.is_alive()

    def is_busy(self, animation_key: str) -> bool:
        lock = self._lock_for(animation_key)
        return lock.locked()
