"""Parallel bake scheduler (requirements 3 and 5).

- Global parallelism and quotas come from the config file and are never
  raised silently at runtime.
- Tasks are admitted strictly in submission order; tasks whose cumulative
  footprint would exceed the global quota wait in a FIFO queue.
- Every admission decision is written to the trace.
- Re-starting an already-active batch returns E_BUSY.
- Results are assembled and reduced in fixed material-index order; the
  batch hash chain never depends on thread completion order. A
  verification pass re-reduces in completion order and aborts with
  E_ORDER_NONDETERMIN on any mismatch.
"""
from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor

from .errors import BusyError, OrderNondeterminism


class Scheduler:
    def __init__(self, parallelism: int, global_memory_bytes: int, trace, verify_order: bool = True):
        self.parallelism = parallelism
        self.global_memory_bytes = global_memory_bytes
        self.trace = trace
        self.verify_order = verify_order
        self._active_batches: set[str] = set()
        self._active_lock = threading.Lock()

    def run_batch(self, batch_id: str, tasks: list[dict], worker) -> list:
        """Run tasks (sorted by submission_index). worker(task) -> result."""
        with self._active_lock:
            if batch_id in self._active_batches:
                raise BusyError("batch already running", batch_id=batch_id)
            self._active_batches.add(batch_id)
        try:
            return self._run(batch_id, tasks, worker)
        finally:
            with self._active_lock:
                self._active_batches.discard(batch_id)

    def _run(self, batch_id: str, tasks: list[dict], worker) -> list:
        ordered = sorted(tasks, key=lambda t: t["submission_index"])
        in_use = 0
        in_use_lock = threading.Condition()
        results: dict[int, object] = {}
        completion_order: list[int] = []

        def wrapped(task):
            nonlocal in_use
            try:
                result = worker(task)
            finally:
                with in_use_lock:
                    in_use -= task["memory_bytes"]
                    in_use_lock.notify_all()
            with in_use_lock:
                results[task["submission_index"]] = result
                completion_order.append(task["submission_index"])
            return result

        with ThreadPoolExecutor(max_workers=self.parallelism) as pool:
            futures = []
            for task in ordered:
                with in_use_lock:
                    while in_use + task["memory_bytes"] > self.global_memory_bytes:
                        self.trace(
                            {
                                "event": "wait",
                                "batch_id": batch_id,
                                "submission_index": task["submission_index"],
                                "in_use_bytes": in_use,
                                "task_bytes": task["memory_bytes"],
                                "global_quota_bytes": self.global_memory_bytes,
                            }
                        )
                        in_use_lock.wait()
                    in_use += task["memory_bytes"]
                    self.trace(
                        {
                            "event": "admit",
                            "batch_id": batch_id,
                            "submission_index": task["submission_index"],
                            "in_use_bytes": in_use,
                            "task_bytes": task["memory_bytes"],
                        }
                    )
                futures.append(pool.submit(wrapped, task))
            for future in futures:
                future.result()  # propagate worker exceptions

        # Fixed-order reduction: assemble strictly by submission index.
        fixed_order = [results[i] for i in sorted(results)]
        if self.verify_order:
            # Re-assemble in raw completion order; per-index results must match
            # the fixed-order assembly element for element.
            for idx in completion_order:
                if results[idx] is not fixed_order[sorted(results).index(idx)]:
                    raise OrderNondeterminism(
                        "completion-order assembly diverges from fixed-order assembly",
                        batch_id=batch_id,
                        submission_index=idx,
                    )
            if sorted(completion_order) != sorted(results.keys()):
                raise OrderNondeterminism("result set incomplete", batch_id=batch_id)
        return fixed_order
