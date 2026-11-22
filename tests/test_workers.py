import threading
import time

import pytest

from syncdemo.errors import ParamError
from syncdemo.workers import (STATUS_CANCELLED, STATUS_OK, WindowPool)


def test_invalid_bounds_raise_e_para():
    with pytest.raises(ParamError) as exc:
        WindowPool(max_workers=0)
    assert exc.value.code == "E_PARA"
    with pytest.raises(ParamError):
        WindowPool(queue_size=0)
    with pytest.raises(ParamError):
        WindowPool(result_buffer=0)


def test_failure_isolated_and_retryable():
    attempts = {"n": 0}

    def flaky():
        attempts["n"] += 1
        if attempts["n"] == 1:
            raise ValueError("boom")
        return "recovered"

    with WindowPool(max_workers=2, queue_size=4, result_buffer=4) as pool:
        pool.submit(1, flaky)
        pool.submit(2, lambda: "fine")
        results = [pool.poll(timeout=5), pool.poll(timeout=5)]
        by_window = {r.window: r for r in results}
        assert by_window[1].status == "failed"
        assert "boom" in by_window[1].error
        assert by_window[2].status == STATUS_OK  # peer window unaffected
        pool.submit(1, flaky)  # retry the failed window
        retry = pool.poll(timeout=5)
        assert retry.status == STATUS_OK
        assert retry.payload == "recovered"


def test_submit_blocks_when_queue_full():
    gate = threading.Event()

    def slow():
        gate.wait(timeout=5)
        return 1

    with WindowPool(max_workers=1, queue_size=1, result_buffer=8) as pool:
        pool.submit(1, slow)          # occupies the single worker
        pool.submit(2, slow)          # fills the queue (size 1)
        done = threading.Event()

        def producer():
            pool.submit(3, slow)      # must block until a slot frees
            done.set()

        thread = threading.Thread(target=producer)
        thread.start()
        time.sleep(0.2)
        assert not done.is_set()      # blocked, not dropped
        gate.set()                    # release the workers
        thread.join(timeout=5)
        assert done.is_set()
        for _ in range(3):
            assert pool.poll(timeout=5).status == STATUS_OK


def test_cancel_marks_remaining_windows():
    gate = threading.Event()

    def slow():
        gate.wait(timeout=5)
        return 1

    pool = WindowPool(max_workers=1, queue_size=4, result_buffer=8)
    try:
        pool.submit(1, slow)
        pool.submit(2, slow)
        time.sleep(0.1)
        pool.cancel()
        gate.set()
        results = [pool.poll(timeout=5), pool.poll(timeout=5)]
        statuses = {r.window: r.status for r in results}
        assert statuses[2] == STATUS_CANCELLED
    finally:
        pool.close()
