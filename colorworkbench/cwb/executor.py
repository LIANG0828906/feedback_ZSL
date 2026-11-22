"""像素批并发执行与资源边界。

- 线程池限制最大并行度 ``max_workers``。
- 每线程像素缓冲通过全局信号量配额限制，缓冲上限耗尽时工作线程阻塞
  （而不是静默丢弃像素批）。
- 共享快照写入由专用锁串行化，批提交顺序按批编号稳定排序，
  因此并行与串行运行产生相同的批提交序列。
"""

from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor

from .pipeline import BatchResult, ConversionPipeline


class BoundedBuffer:
    """按像素数计的缓冲配额；耗尽时 acquire 阻塞。"""

    def __init__(self, pixel_capacity: int):
        self._cap = max(1, pixel_capacity)
        self._remaining = self._cap
        self._cond = threading.Condition()
        self.peak_usage = 0

    def acquire(self, pixels: int) -> None:
        with self._cond:
            while self._remaining < min(pixels, self._cap):
                self._cond.wait()
            self._remaining -= min(pixels, self._cap)
            self.peak_usage = max(self.peak_usage,
                                  self._cap - self._remaining)

    def release(self, pixels: int) -> None:
        with self._cond:
            self._remaining += min(pixels, self._cap)
            self._remaining = min(self._remaining, self._cap)
            self._cond.notify_all()


class BatchExecutor:
    def __init__(self, pipeline: ConversionPipeline, max_workers: int = 4,
                 pixel_buffer_cap: int = 4 * 1024 * 1024):
        self.pipeline = pipeline
        self.max_workers = max(1, max_workers)
        self.buffer = BoundedBuffer(pixel_buffer_cap)
        self._snapshot_lock = threading.Lock()
        self.commit_sequence: list[int] = []
        self._failed: dict[int, str] = {}

    def run(self, image, batch_ids: list[int] | None = None,
            parallel: bool = True,
            snapshot_writer=None) -> list[BatchResult]:
        ids = (batch_ids if batch_ids is not None
               else [b.batch_id for b in self.pipeline.batches])
        width = image.shape[1]
        results: dict[int, BatchResult] = {}

        def work(bid: int) -> BatchResult:
            batch = self.pipeline.batches[bid]
            rows = batch.row_end - batch.row_start
            pixels = rows * width
            self.buffer.acquire(pixels)
            try:
                raw = image[batch.row_start:batch.row_end]
                res = self.pipeline.run_batch(bid, raw)
                # 共享快照写入串行化，并保持批序稳定
                with self._snapshot_lock:
                    self.commit_sequence.append(bid)
                    if snapshot_writer is not None:
                        snapshot_writer(bid, res)
                return res
            finally:
                self.buffer.release(pixels)

        if parallel and self.max_workers > 1:
            with ThreadPoolExecutor(max_workers=self.max_workers) as pool:
                future_to_bid = {pool.submit(work, bid): bid for bid in ids}
                for fut, bid in future_to_bid.items():
                    try:
                        results[bid] = fut.result()
                    except Exception as exc:  # 单批失败不污染其他批
                        self._failed[bid] = getattr(exc, "code", "E_BATCH")
        else:
            for bid in ids:
                try:
                    results[bid] = work(bid)
                except Exception as exc:
                    self._failed[bid] = getattr(exc, "code", "E_BATCH")

        # 提交序列按批编号归一（并行完成顺序可能不同，落盘顺序保持稳定）
        self.commit_sequence = sorted(self.commit_sequence)
        return [results[bid] for bid in sorted(results)]

    @property
    def failed_batches(self) -> dict[int, str]:
        return dict(self._failed)

    def retry(self, image, batch_ids: list[int], parallel: bool = True,
              snapshot_writer=None) -> list[BatchResult]:
        for bid in batch_ids:
            self._failed.pop(bid, None)
        return self.run(image, batch_ids=batch_ids, parallel=parallel,
                        snapshot_writer=snapshot_writer)
