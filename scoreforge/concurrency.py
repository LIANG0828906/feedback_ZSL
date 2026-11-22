"""并发与资源边界.

小节提交的"准备"阶段（校验 + 片段构建 + 哈希）在工作线程池中并行推进；
"应用"阶段（写回模型 + 刷新索引 + 写检查点）在主线程按稳定顺序串行执行。

资源边界:
  - max_workers 限制最大并行度;
  - buffer_size 限制在途任务缓冲上限, 耗尽时阻塞排队而非静默丢弃;
  - 检查点写入由 CheckpointStore 单锁串行化, 小节顺序由提交顺序保证;
  - 单小节异常仅置该小节为 FAILED, 可修复后重试, 不污染其他小节;
  - 并行与单线程串行对同一编辑序列产出相同提交序列与相同结果。
"""

from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass

from .errors import BatchError, ScoreError
from .session import CommitRecord, EditSession


@dataclass
class CommitResult:
    part_id: str
    measure_index: int
    status: str               # "committed" | "failed"
    record: CommitRecord | None = None
    error: ScoreError | None = None


class ParallelCommitter:
    def __init__(self, session: EditSession, max_workers: int = 4, buffer_size: int = 8):
        if max_workers < 1 or buffer_size < 1:
            raise ValueError("max_workers and buffer_size must be >= 1")
        self.session = session
        self.max_workers = max_workers
        self.buffer_size = buffer_size

    def commit(self, keys: list[tuple[str, int]]) -> list[CommitResult]:
        """按 keys 给定顺序提交；返回与 keys 等长同序的结果列表."""
        session = self.session
        semaphore = threading.Semaphore(self.buffer_size)

        def guarded_prepare(key):
            try:
                return ("ok", session.prepare_commit(*key))
            except ScoreError as exc:
                return ("err", exc)
            finally:
                semaphore.release()

        results: list[CommitResult] = []
        with ThreadPoolExecutor(max_workers=self.max_workers) as pool:
            futures = []
            for key in keys:
                semaphore.acquire()  # 缓冲耗尽时阻塞排队，不丢弃
                futures.append((key, pool.submit(guarded_prepare, key)))
            for key, future in futures:
                status, payload = future.result()
                if status == "err":
                    results.append(CommitResult(key[0], key[1], "failed", error=payload))
                    continue
                try:
                    record = session.apply_commit(payload)
                except ScoreError as exc:
                    results.append(CommitResult(key[0], key[1], "failed", error=exc))
                except Exception as exc:  # pragma: no cover
                    results.append(CommitResult(
                        key[0], key[1], "failed",
                        error=BatchError(f"apply failed: {exc}"),
                    ))
                else:
                    results.append(CommitResult(key[0], key[1], "committed", record=record))
        return results

    def commit_all_pending(self) -> list[CommitResult]:
        return self.commit(self.session.pending_keys())


def commit_sequence(results: list[CommitResult]) -> list[tuple]:
    """提取提交序列（用于并行 / 串行等价性比对）."""
    return [
        (r.record.seq, r.part_id, r.measure_index, r.record.content_hash)
        for r in results if r.status == "committed"
    ]
