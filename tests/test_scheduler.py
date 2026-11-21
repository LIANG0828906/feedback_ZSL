from __future__ import annotations

import time

from inverse_imaging.errors import E_BUDGET_EXCEEDED, AppError
from inverse_imaging.scheduler import BudgetConfig, ResourcePool, estimate_chunk_memory_mb


def test_stable_sequence_and_global_memory_wait() -> None:
    pool = ResourcePool(
        BudgetConfig(max_parallel=2, max_memory_mb=16.1, default_task_quota_mb=10.0)
    )
    events: list[str] = []

    def make(name: str):
        def run() -> str:
            events.append(name)
            return name

        return run

    def slow_b() -> str:
        time.sleep(0.05)
        events.append("b")
        return "b"

    pool.submit(make("a"), "a", chunk_size=2, requested_memory_mb=8.0)
    pool.submit(slow_b, "b", chunk_size=2, requested_memory_mb=8.0)
    pool.submit(make("c"), "c", chunk_size=2, requested_memory_mb=10.0)
    jobs = pool.run_all()
    assert [job.sequence for job in jobs] == [0, 1, 2]
    assert [job.status for job in jobs] == ["completed", "completed", "completed"]
    assert sorted(events) == ["a", "b", "c"]
    kinds = [event["event"] for event in pool.events()]
    assert "waiting" in kinds
    assert pool.reserved_memory_mb == 0.0


def test_chunk_degrades_before_admission() -> None:
    pool = ResourcePool(
        BudgetConfig(max_parallel=1, max_memory_mb=64.0, default_task_quota_mb=9.0)
    )
    job = pool.submit(
        lambda: "ok",
        "degrade",
        chunk_size=32,
        requested_memory_mb=64.0,
        task_quota_mb=9.0,
        degradation_sequence=(32, 16, 8, 4, 2, 1),
    )
    assert job.degraded_from == 32
    assert job.chunk_size < 32
    assert job.memory_mb <= 9.0
    pool.run_all()
    degraded = [event for event in pool.events() if event["event"] == "degraded"]
    assert degraded
    assert degraded[0]["from_chunk_size"] == 32


def test_rejection_preserves_other_jobs() -> None:
    pool = ResourcePool(
        BudgetConfig(max_parallel=1, max_memory_mb=16.0, default_task_quota_mb=8.1)
    )
    first = pool.submit(lambda: "kept", "kept", chunk_size=1)
    try:
        pool.submit(
            lambda: "never",
            "rejected",
            chunk_size=1,
            requested_memory_mb=32.0,
            task_quota_mb=8.1,
        )
    except AppError as error:
        assert error.code == E_BUDGET_EXCEEDED
    else:
        raise AssertionError("expected budget rejection")
    jobs = pool.run_all()
    assert len(jobs) == 1
    assert jobs[0] is first
    assert jobs[0].status == "completed"
    assert jobs[0].result == "kept"
    assert any(event["event"] == "rejected" for event in pool.events())


def test_memory_estimate_is_deterministic() -> None:
    assert estimate_chunk_memory_mb(8) == estimate_chunk_memory_mb(8)
    assert estimate_chunk_memory_mb(16) > estimate_chunk_memory_mb(8)

