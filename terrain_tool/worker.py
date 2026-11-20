"""后台重算任务管理。

同一地形同一时刻只允许一个重算任务，重复启动返回 ``E_BUSY``；
任务在 QThread 中运行，通过取消令牌协作式中断，取消后释放离屏缓冲，
但磁盘上最后一次已提交快照保持不变。
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Callable, Optional

from PySide6.QtCore import QObject, QThread, Signal

from .errors import ErrorCode, TerrainError


class CancellationToken:
    def __init__(self) -> None:
        self._cancelled = False

    def cancel(self) -> None:
        self._cancelled = True

    @property
    def cancelled(self) -> bool:
        return self._cancelled

    def raise_if_cancelled(self) -> None:
        if self._cancelled:
            raise TerrainError(ErrorCode.E_CANCELLED, "重算任务已被协作式取消")


@dataclass
class JobRequest:
    kind: str  # "incremental" / "full"
    seq: int
    payload: dict


class RecomputeWorker(QObject):
    finished_ok = Signal(object)   # RenderSummary / 结果字典
    failed = Signal(object)        # TerrainError
    progress = Signal(int, int)    # done, total

    def __init__(
        self,
        job: Callable[[CancellationToken, Callable[[int, int], None]], object],
        token: CancellationToken,
    ) -> None:
        super().__init__()
        self._job = job
        self._token = token

    def run(self) -> None:
        try:
            result = self._job(self._token, lambda d, t: self.progress.emit(d, t))
        except TerrainError as exc:
            self.failed.emit(exc)
            return
        except Exception as exc:  # pragma: no cover - 防御性
            self.failed.emit(TerrainError(ErrorCode.E_COMMIT_FAILED, str(exc)))
            return
        self.finished_ok.emit(result)


class RecomputeManager(QObject):
    """单任务调度器：运行中再次提交直接 E_BUSY。"""

    job_finished = Signal(object)
    job_failed = Signal(object)
    job_progress = Signal(int, int)
    busy_changed = Signal(bool)

    def __init__(self) -> None:
        super().__init__()
        self._thread: Optional[QThread] = None
        self._worker: Optional[RecomputeWorker] = None
        self._token: Optional[CancellationToken] = None

    @property
    def busy(self) -> bool:
        return self._thread is not None and self._thread.isRunning()

    def submit(
        self,
        job: Callable[[CancellationToken, Callable[[int, int], None]], object],
    ) -> CancellationToken:
        if self.busy:
            raise TerrainError(
                ErrorCode.E_BUSY,
                "已有重算任务运行中，同一地形同时只允许一个重算任务",
            )
        self._token = CancellationToken()
        self._thread = QThread()
        self._worker = RecomputeWorker(job, self._token)
        self._worker.moveToThread(self._thread)
        self._thread.started.connect(self._worker.run)
        self._worker.finished_ok.connect(self._on_ok)
        self._worker.failed.connect(self._on_failed)
        self._worker.progress.connect(self.job_progress)
        self.busy_changed.emit(True)
        self._thread.start()
        return self._token

    def cancel(self) -> bool:
        """请求协作式取消，返回是否存在可取消任务。"""
        if not self.busy or self._token is None:
            return False
        self._token.cancel()
        return True

    def _cleanup(self) -> None:
        if self._thread is not None:
            self._thread.quit()
            self._thread.wait()
            self._thread = None
            self._worker = None
            self._token = None
            self.busy_changed.emit(False)

    def _on_ok(self, result: object) -> None:
        self._cleanup()
        self.job_finished.emit(result)

    def _on_failed(self, exc: TerrainError) -> None:
        self._cleanup()
        self.job_failed.emit(exc)

    def shutdown(self) -> None:
        if self.busy and self._token is not None:
            self._token.cancel()
        self._cleanup()
