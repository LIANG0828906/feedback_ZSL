"""并发推进与资源边界。

同一对局同时只允许一个推进任务（重复启动返回 E_BUSY）；
任务在时间步边界检查点响应协作式取消；取消或异常后释放
快照文件句柄（快照按步写后即关），保留最后一次已提交快照。
"""

from __future__ import annotations

import threading

from .engine import ReplayEngine
from .errors import BusyError, CancelledError, ReplayError
from .snapshot import SnapshotStore


class RunHandle:
    def __init__(self, match_id: str):
        self.match_id = match_id
        self.status = "RUNNING"
        self.cancel_event = threading.Event()
        self.error: dict | None = None
        self.frames_done = 0
        self.trace: list[dict] = []
        self.thread: threading.Thread | None = None

    def cancel(self):
        self.cancel_event.set()

    def join(self, timeout: float | None = None):
        if self.thread is not None:
            self.thread.join(timeout)


class MatchRunner:
    def __init__(self):
        self._lock = threading.Lock()
        self._running: dict[str, RunHandle] = {}

    def start(self, match: dict, snapshot_dir: str,
              recover: bool = False) -> RunHandle:
        match_id = str(match["match_id"])
        with self._lock:
            if match_id in self._running:
                raise BusyError(match_id)
            handle = RunHandle(match_id)
            self._running[match_id] = handle

        def _work():
            store = SnapshotStore(snapshot_dir)
            try:
                engine = (ReplayEngine.recover(match, store) if recover
                          else ReplayEngine(match))

                def on_frame(record):
                    store.commit(engine.snapshot_payload())
                    handle.frames_done = record["frame"] + 1

                handle.trace = engine.run(
                    cancel_event=handle.cancel_event, on_frame=on_frame)
                handle.status = "COMPLETED"
            except CancelledError as exc:
                handle.status = "CANCELLED"
                handle.error = exc.to_dict()
            except ReplayError as exc:
                handle.status = "FAILED"
                handle.error = exc.to_dict()
            except Exception as exc:  # noqa: BLE001
                handle.status = "FAILED"
                handle.error = {"error": "E_INTERNAL", "message": str(exc)}
            finally:
                with self._lock:
                    self._running.pop(match_id, None)

        thread = threading.Thread(target=_work, daemon=True,
                                  name=f"replay-{match_id}")
        handle.thread = thread
        thread.start()
        return handle

    def running(self, match_id: str) -> bool:
        with self._lock:
            return match_id in self._running
