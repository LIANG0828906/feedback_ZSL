from __future__ import annotations

import threading
import time

from mesh_repair.budget import BlockTask, BudgetScheduler, TraceWriter, estimate_memory


def test_budget_wait_wakes_and_limits_parallelism(tmp_path) -> None:
    trace = TraceWriter(tmp_path / "trace.jsonl", run_id="budget")
    usage = estimate_memory(100, 100, 150)
    scheduler = BudgetScheduler(
        global_budget_bytes=usage.total_bytes,
        task_budget_bytes=usage.total_bytes,
        parallelism=3,
        trace=trace,
    )
    starts: list[int] = []
    starts_lock = threading.Lock()

    def worker(task, cancel_event):
        with starts_lock:
            starts.append(task.submit_seq)
        time.sleep(0.04)
        return task.submit_seq

    tasks = [BlockTask(f"block-{index}", index, usage) for index in range(4)]
    results = scheduler.execute(tasks, worker)
    assert [result.payload for result in results] == [0, 1, 2, 3]
    assert starts == [0, 1, 2, 3]
    text = (tmp_path / "trace.jsonl").read_text()
    assert '"event":"budget_wait"' in text
    assert '"max_active_blocks":1' in text


def test_per_task_budget_rejects_before_start(tmp_path) -> None:
    usage = estimate_memory(100, 100, 150)
    trace = TraceWriter(tmp_path / "trace.jsonl", run_id="reject")
    scheduler = BudgetScheduler(
        global_budget_bytes=usage.total_bytes * 4,
        task_budget_bytes=usage.total_bytes - 1,
        parallelism=2,
        trace=trace,
    )
    task = BlockTask("block-0", 0, usage)
    errors = scheduler.preflight([task])
    assert errors["block-0"].code == "E_BUDGET_EXCEEDED"
    assert errors["block-0"].location["block_id"] == "block-0"
    started = False

    def worker(task, cancel_event):
        nonlocal started
        started = True

    results = scheduler.execute([task], worker)
    assert not started
    assert not results[0].started
    assert results[0].error.code == "E_BUDGET_EXCEEDED"
