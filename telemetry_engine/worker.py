"""Processing-unit worker process.

Each worker executes aggregation tasks. A task flagged with inject_crash
simulates an abnormal processing-unit exit (fault-injection tests).
"""

import os

from .core import aggregate


def worker_main(task_queue, result_queue, window_ms, row_bytes, mem_factor):
    while True:
        task = task_queue.get()
        if task is None:
            break
        if task.get("inject_crash"):
            os._exit(42)
        try:
            partials = aggregate(task["records"], window_ms)
            result_queue.put({
                "task_id": task["task_id"],
                "tenant_id": task["tenant_id"],
                "side": task["side"],
                "vehicle": task["vehicle"],
                "unit": task["unit"],
                "rows": task["rows"],
                "attempts": task["attempts"],
                "peak_mem_bytes": task["rows"] * row_bytes * mem_factor,
                "partials": partials,
            })
        except Exception as exc:
            result_queue.put({
                "task_id": task["task_id"],
                "tenant_id": task["tenant_id"],
                "error": f"{type(exc).__name__}: {exc}",
            })
