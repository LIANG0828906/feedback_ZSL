import threading
import time

from scoreworkbench.constants import RESOURCE_LIMITS
from scoreworkbench.errors import SemanticError
from scoreworkbench.resources import (
    check_event_buffer,
    open_file_slot,
    parallel_map,
    serial_persist_write,
)

import unittest


class ResourceTests(unittest.TestCase):
    def test_open_file_semaphore_bounds_concurrency(self):
        active = {"count": 0, "max": 0}
        lock = threading.Lock()

        def task(_):
            with open_file_slot():
                with lock:
                    active["count"] += 1
                    active["max"] = max(active["max"], active["count"])
                time.sleep(0.02)
                with lock:
                    active["count"] -= 1

        parallel_map(task, list(range(20)), max_workers=12)
        self.assertLessEqual(active["max"], RESOURCE_LIMITS["max_open_files"])

    def test_event_buffer_limit_enforced(self):
        check_event_buffer(RESOURCE_LIMITS["max_event_buffer"])
        with self.assertRaises(SemanticError):
            check_event_buffer(RESOURCE_LIMITS["max_event_buffer"] + 1)

    def test_serial_write_lock_is_reentrant_and_ordered(self):
        order = []

        def writer(index):
            with serial_persist_write():
                order.append(("start", index))
                with serial_persist_write():
                    order.append(("inner", index))
                order.append(("end", index))

        threads = [threading.Thread(target=writer, args=(i,)) for i in range(5)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        for index in range(5):
            self.assertIn(("inner", index), order)
        # No interleaving: each index completes all three steps together.
        self.assertEqual(len(order), 15)


if __name__ == "__main__":
    unittest.main()
