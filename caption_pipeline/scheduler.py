# 并行调度与资源预算控制。
from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass

from .errors import E_PARA, PipelineError


@dataclass(frozen=True)
class Budget:
    max_workers: int = 4
    max_text_bytes: int = 1 << 20     # 全局在飞文本缓冲上限（字节）
    max_sample_bytes: int = 1 << 26   # 全局在飞采样缓冲上限（字节）

    def validate(self):
        if self.max_workers <= 0:
            raise PipelineError(E_PARA, "最大并行度必须为正",
                                field="budget.max_workers")
        if self.max_text_bytes <= 0:
            raise PipelineError(E_PARA, "文本缓冲上限必须为正",
                                field="budget.max_text_bytes")
        if self.max_sample_bytes <= 0:
            raise PipelineError(E_PARA, "采样缓冲上限必须为正",
                                field="budget.max_sample_bytes")


class _BlockingBudget:
    # 计数型预算：耗尽时阻塞等待而非丢弃任务。
    def __init__(self, capacity):
        self._capacity = capacity
        self._used = 0
        self._cond = threading.Condition()

    def acquire(self, amount):
        with self._cond:
            while self._used + amount > self._capacity:
                self._cond.wait()
            self._used += amount

    def release(self, amount):
        with self._cond:
            self._used -= amount
            self._cond.notify_all()


class Scheduler:
    # 工作线程池 + 资源预算；任务结果以 future 形式按提交键取回。
    def __init__(self, budget: Budget):
        budget.validate()
        self.budget = budget
        self._text_budget = _BlockingBudget(budget.max_text_bytes)
        self._sample_budget = _BlockingBudget(budget.max_sample_bytes)
        self._executor = None

    def submit_all(self, tasks):
        # tasks: iterable of (key, text_bytes, sample_bytes, fn)
        # 返回 {key: Future}；预算不足时工作线程阻塞排队，不丢任务。
        futures = {}
        self._executor = ThreadPoolExecutor(
            max_workers=self.budget.max_workers,
            thread_name_prefix="caption-worker")
        for key, text_bytes, sample_bytes, fn in tasks:
            if text_bytes > self.budget.max_text_bytes:
                raise PipelineError(E_PARA, "单任务文本超过文本缓冲预算",
                                    field="budget.max_text_bytes")
            if sample_bytes > self.budget.max_sample_bytes:
                raise PipelineError(E_PARA, "单任务采样超过采样缓冲预算",
                                    field="budget.max_sample_bytes")
            futures[key] = self._executor.submit(
                self._run_guarded, fn, text_bytes, sample_bytes)
        return futures

    def _run_guarded(self, fn, text_bytes, sample_bytes):
        self._text_budget.acquire(text_bytes)
        try:
            self._sample_budget.acquire(sample_bytes)
            try:
                return fn()
            finally:
                self._sample_budget.release(sample_bytes)
        finally:
            self._text_budget.release(text_bytes)

    def shutdown(self):
        if self._executor is not None:
            self._executor.shutdown(wait=True)
            self._executor = None
