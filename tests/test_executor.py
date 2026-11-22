from __future__ import annotations

from formation_workbench.executor import BoundedExecutor


def test_bounded_executor_preserves_order_and_isolates_errors():
    executor = BoundedExecutor(max_workers=2, per_worker_buffer=1)

    def worker(value: int) -> int:
        if value == 3:
            raise RuntimeError("unit failure")
        return value * 10

    outcomes = executor.run_ordered(range(7), worker)
    assert [outcome.item for outcome in outcomes] == list(range(7))
    assert [outcome.value for outcome in outcomes if outcome.error is None] == [
        0,
        10,
        20,
        40,
        50,
        60,
    ]
    assert outcomes[3].error is not None
    assert "unit failure" in str(outcomes[3].error)
