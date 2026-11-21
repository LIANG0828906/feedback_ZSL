"""后台编辑与渲染线程隔离。

Workbench 持有当前已提交版本的不可变快照。渲染线程通过 acquire()
获取句柄并读取快照；编辑在后台线程执行纯函数算子并原子提交，
完成后通过一次整体替换发布新快照。替换前渲染读取旧版本，
不会观察到部分更新的块；句柄在发布后失效，读取抛出 StaleHandle。
"""

from __future__ import annotations

import queue
import threading
from dataclasses import dataclass

from .errors import StaleHandle
from .ops import apply_operator
from .store import VersionStore


@dataclass(frozen=True)
class Snapshot:
    """不可变版本快照：发布后内容不再变化。"""

    version: int
    points: object
    colors: object
    block_index: object
    content_hash: str


class SnapshotHandle:
    def __init__(self, workbench: "Workbench", snapshot: Snapshot, generation: int):
        self._workbench = workbench
        self._snapshot = snapshot
        self._generation = generation

    @property
    def version(self) -> int:
        return self._snapshot.version

    def read(self) -> Snapshot:
        """读取快照；句柄已废弃（有更新版本发布）时抛出 StaleHandle。"""
        if self._generation != self._workbench.generation:
            raise StaleHandle(
                "快照句柄已废弃",
                handle_generation=self._generation,
                current_generation=self._workbench.generation,
            )
        return self._snapshot


@dataclass
class EditRequest:
    op: str
    params: dict
    selection: object


@dataclass
class EditResult:
    record: object = None
    error: Exception | None = None

    @property
    def ok(self) -> bool:
        return self.error is None


class Workbench:
    def __init__(self, store: VersionStore):
        self.store = store
        self._lock = threading.Lock()
        self._snapshot = self._make_snapshot()
        self._generation = 0
        self._queue: queue.Queue = queue.Queue()
        self._results: queue.Queue = queue.Queue()
        self._worker = threading.Thread(target=self._edit_loop, daemon=True,
                                        name="pcwb-editor")
        self._worker.start()

    def _make_snapshot(self) -> Snapshot:
        cloud = self.store.current_cloud
        return Snapshot(
            version=self.store.current_version,
            points=cloud.points,
            colors=cloud.colors,
            block_index=self.store.current_index,
            content_hash=cloud.content_hash(),
        )

    @property
    def generation(self) -> int:
        with self._lock:
            return self._generation

    def acquire(self) -> SnapshotHandle:
        """渲染线程每帧调用：获取当前已提交版本的句柄。"""
        with self._lock:
            return SnapshotHandle(self, self._snapshot, self._generation)

    def _publish(self):
        """一次整体替换发布新版本快照。"""
        snapshot = self._make_snapshot()
        with self._lock:
            self._snapshot = snapshot
            self._generation += 1

    def submit_edit(self, op: str, params: dict, selection) -> None:
        """把编辑请求投入后台队列，立即返回，不阻塞巡览。"""
        self._queue.put(EditRequest(op=op, params=dict(params),
                                    selection=selection))

    def _edit_loop(self):
        while True:
            req = self._queue.get()
            if req is None:
                return
            try:
                if req.op == "rollback":
                    record = self.store.rollback(
                        int(req.params["target_version"]))
                else:
                    cloud = self.store.current_cloud
                    new_points, new_colors = apply_operator(
                        req.op, cloud.points, cloud.colors,
                        req.selection, req.params)
                    record = self.store.commit(req.op, req.params,
                                               new_points, new_colors)
                self._publish()
                self._results.put(EditResult(record=record))
            except Exception as exc:
                self._results.put(EditResult(error=exc))

    def wait_result(self, timeout: float = None) -> EditResult:
        """等待一次后台编辑完成（演示与测试用）。"""
        return self._results.get(timeout=timeout)

    def render_frame(self) -> Snapshot:
        """渲染线程读取路径：每帧获取新句柄并读取快照。"""
        return self.acquire().read()

    def shutdown(self):
        self._queue.put(None)
        self._worker.join(timeout=5)
