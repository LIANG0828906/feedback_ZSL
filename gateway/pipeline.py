"""Per-version isolated batching queue and bounded-concurrency inference."""

from __future__ import annotations

import asyncio
import threading
import time
from dataclasses import dataclass, field

import numpy as np

from .config import VersionConfig
from .errors import ErrorCode, GatewayError


@dataclass
class PendingRequest:
    request_id: str
    tenant_id: str
    client_id: str | None
    vector: np.ndarray
    enqueued_at: float
    deadline: float
    future: asyncio.Future
    expired: bool = False


@dataclass
class PipelineMetrics:
    depth: int = 0
    in_flight: int = 0
    total_batches: int = 0
    adaptive_batch_size: int = 0
    adaptive_wait_ms: float = 0.0
    lock: threading.Lock = field(default_factory=threading.Lock, repr=False)

    def snapshot(self) -> dict:
        with self.lock:
            return {
                "adaptive_batch_size": self.adaptive_batch_size,
                "adaptive_wait_ms": round(self.adaptive_wait_ms, 3),
                "depth": self.depth,
                "in_flight": self.in_flight,
                "total_batches": self.total_batches,
            }


class VersionPipeline:
    """Own queue, batcher loop, concurrency slots and slow-call injection.

    Two pipelines share no queue or semaphore, so backpressure on one version
    can never extend enqueue wait on the other.
    """

    def __init__(self, version: str, cfg: VersionConfig, registry):
        self.version = version
        self.cfg = cfg
        self._registry = registry
        # Boundedness is enforced by the admission counter below, which also
        # accounts for requests already dequeued and executing in a batch.
        self._queue: asyncio.Queue = asyncio.Queue()
        self._slots = asyncio.Semaphore(cfg.max_concurrency)
        self._admitted = 0
        self._admission_lock = asyncio.Lock()
        self._metrics = PipelineMetrics(
            adaptive_batch_size=cfg.min_batch_size,
            adaptive_wait_ms=cfg.min_wait_ms,
        )
        self._slow_calls_remaining = 0
        self._slow_delay_ms = 0.0
        self._slow_lock = threading.Lock()
        self._worker_task: asyncio.Task | None = None
        self._stopped = False

    @property
    def metrics(self) -> PipelineMetrics:
        return self._metrics

    def start(self) -> None:
        if self._worker_task is None or self._worker_task.done():
            self._stopped = False
            self._worker_task = asyncio.create_task(self._batch_loop())

    async def stop(self) -> None:
        self._stopped = True
        if self._worker_task:
            self._worker_task.cancel()
            try:
                await self._worker_task
            except (asyncio.CancelledError, Exception):
                pass

    def inject_slow_calls(self, count: int, delay_ms: float) -> None:
        with self._slow_lock:
            self._slow_calls_remaining = max(0, int(count))
            self._slow_delay_ms = max(0.0, float(delay_ms))

    def _consume_slow_delay(self) -> float:
        with self._slow_lock:
            if self._slow_calls_remaining <= 0:
                return 0.0
            self._slow_calls_remaining -= 1
            return self._slow_delay_ms

    async def submit(self, request_id, tenant_id, client_id, vector) -> float:
        """Enqueue one request; raises on full queue or wait timeout."""

        loop = asyncio.get_running_loop()
        # The batcher pulls the head request immediately, so Queue.qsize() alone
        # cannot represent backlog. Admission counts queued + in-flight requests
        # against the configured depth and rejects overflow without blocking.
        async with self._admission_lock:
            if self._admitted >= self.cfg.max_queue_depth:
                raise GatewayError(
                    ErrorCode.QUEUE_FULL,
                    f"version {self.version} queue is full (depth "
                    f"{self.cfg.max_queue_depth})",
                    field=f"queue.{self.version}",
                )
            self._admitted += 1
        future = loop.create_future()
        now = loop.time()
        pending = PendingRequest(
            request_id=request_id,
            tenant_id=tenant_id,
            client_id=client_id,
            vector=np.asarray(vector, dtype=np.float32),
            enqueued_at=now,
            deadline=now + self.cfg.enqueue_timeout_ms / 1000.0,
            future=future,
        )
        try:
            self._queue.put_nowait(pending)
        except asyncio.QueueFull:  # safety net; admission counter bounds depth
            async with self._admission_lock:
                self._admitted = max(0, self._admitted - 1)
            raise GatewayError(
                ErrorCode.QUEUE_FULL,
                f"version {self.version} queue is full (depth "
                f"{self.cfg.max_queue_depth})",
                field=f"queue.{self.version}",
            )
        with self._metrics.lock:
            self._metrics.depth = self._admitted
        try:
            return await asyncio.wait_for(
                future, timeout=self.cfg.enqueue_timeout_ms / 1000.0
            )
        except asyncio.TimeoutError:
            pending.expired = True
            if not future.done():
                future.cancel()
            async with self._admission_lock:
                self._admitted = max(0, self._admitted - 1)
            raise GatewayError(
                ErrorCode.QUEUE_WAIT_TIMEOUT,
                f"version {self.version} request waited longer than "
                f"{self.cfg.enqueue_timeout_ms}ms",
                field=f"queue.{self.version}",
            )
        finally:
            with self._metrics.lock:
                self._metrics.depth = self._admitted

    def _adapt_batch_params(self, depth: int) -> tuple[int, float]:
        cfg = self.cfg
        span = max(1, cfg.max_batch_size - cfg.min_batch_size)
        ratio = min(1.0, depth / max(1, cfg.max_queue_depth))
        batch_size = cfg.min_batch_size + int(round(ratio * span))
        wait_ms = cfg.min_wait_ms + ratio * (cfg.max_wait_ms - cfg.min_wait_ms)
        batch_size = min(cfg.max_batch_size, max(cfg.min_batch_size, batch_size))
        return batch_size, wait_ms

    async def _batch_loop(self) -> None:
        while not self._stopped:
            try:
                first = await self._queue.get()
            except asyncio.CancelledError:
                raise
            batch = [first]
            depth = self._queue.qsize() + 1
            batch_size, wait_ms = self._adapt_batch_params(depth)
            with self._metrics.lock:
                self._metrics.adaptive_batch_size = batch_size
                self._metrics.adaptive_wait_ms = wait_ms
                self._metrics.depth = self._admitted

            deadline = asyncio.get_running_loop().time() + wait_ms / 1000.0
            while len(batch) < batch_size:
                timeout = deadline - asyncio.get_running_loop().time()
                if timeout <= 0:
                    break
                try:
                    item = await asyncio.wait_for(self._queue.get(), timeout=timeout)
                except asyncio.TimeoutError:
                    break
                batch.append(item)

            asyncio.create_task(self._run_batch(batch))

    async def _run_batch(self, batch: list[PendingRequest]) -> None:
        async with self._slots:
            with self._metrics.lock:
                self._metrics.in_flight += 1
            loop = asyncio.get_running_loop()
            active = [item for item in batch if not item.expired]
            try:
                if not active:
                    return
                delay_ms = self._consume_slow_delay()
                if delay_ms > 0:
                    await asyncio.sleep(delay_ms / 1000.0)
                matrix = np.vstack([item.vector.reshape(1, -1) for item in active])
                try:
                    session = await loop.run_in_executor(
                        None, self._registry.get_ready_session, self.version
                    )
                    scores = await loop.run_in_executor(
                        None, self._run_inference, session, matrix
                    )
                except GatewayError as exc:
                    for item in active:
                        self._admitted = max(0, self._admitted - 1)
                        if not item.future.done():
                            item.future.set_exception(exc)
                    return
                except Exception as exc:  # noqa: BLE001
                    error = GatewayError(
                        ErrorCode.INFERENCE_FAILED,
                        f"inference failed on {self.version}: {type(exc).__name__}: {exc}",
                        field=f"model.{self.version}",
                    )
                    for item in active:
                        self._admitted = max(0, self._admitted - 1)
                        if not item.future.done():
                            item.future.set_exception(error)
                    return
                for item, score in zip(active, scores):
                    self._admitted = max(0, self._admitted - 1)
                    if not item.future.done():
                        item.future.set_result(float(score))
            finally:
                with self._metrics.lock:
                    self._metrics.in_flight -= 1
                    self._metrics.total_batches += 1
                    self._metrics.depth = self._admitted

    def _run_inference(self, session, matrix: np.ndarray) -> np.ndarray:
        input_name = session.get_inputs()[0].name
        outputs = session.run(None, {input_name: matrix.astype(np.float32)})
        return np.asarray(outputs[0], dtype=np.float32).reshape(-1)
