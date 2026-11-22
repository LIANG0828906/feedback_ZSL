"""Concurrent block scheduler with resource bounds and stable commit order.

Workers run in a bounded thread pool.  A counting semaphore enforces the
per-thread pixel-buffer budget: acquiring a block's pixel budget blocks (never
drops work) when the budget is exhausted.  Checkpoint writes and state commits
are serialised through one lock and results are published in ascending block
id order, so parallel and single-threaded runs produce identical per-block
results and an identical commit sequence.
"""

import threading
from concurrent.futures import ThreadPoolExecutor

import numpy as np

from .checkpoint import CheckpointStore
from .engine import process_block, whole_image_hash
from .errors import ForensicsError, E_VER
from .states import BlockState, PENDING, RUNNING, COMMITTED, FAILED


class Scheduler:
    def __init__(
        self,
        sample,
        snapshot,
        checkpoint_root,
        max_workers=1,
        max_pixel_buffers=None,
        fault_injection=None,
    ):
        self.sample = sample
        self.snapshot = snapshot
        self.params = snapshot.params
        self.max_workers = max(1, max_workers)
        n_blocks = len(sample.block_index)
        # Default budget: enough for one block per worker at a time.
        self.max_pixel_buffers = (
            max_pixel_buffers if max_pixel_buffers is not None else self.max_workers
        )
        self._buffer_sem = threading.BoundedSemaphore(self.max_pixel_buffers)
        self._commit_lock = threading.Lock()
        self._pause_event = threading.Event()
        self._pause_event.set()
        self._cancel = threading.Event()
        self.fault_injection = fault_injection or {}

        self.states = [BlockState(i) for i in range(n_blocks)]
        self.results = {}
        self.committed_order = []
        self.failed = {}

        self.image_hash = whole_image_hash(
            sample.pixels, self.params.hash_algorithm
        )
        self.store = CheckpointStore(checkpoint_root, sample.name)

    # -- checkpoint / resume -------------------------------------------------

    def restore(self):
        """Load matching persisted blocks; invalid snapshot raises E_VER."""
        records = self.store.load(
            self.image_hash,
            self.snapshot.signature,
            len(self.states),
        )
        for block_id, record in records.items():
            state = self.states[block_id]
            state.state = COMMITTED
            self.results[block_id] = record
            self.committed_order.append(block_id)
        self.committed_order.sort()
        return len(records)

    def write_header(self):
        self.store.write_header(
            self.image_hash, self.snapshot.signature, self.snapshot.algorithm_version
        )

    # -- controls ------------------------------------------------------------

    def pause(self):
        self._pause_event.clear()

    def resume(self):
        self._pause_event.set()

    def cancel(self):
        self._cancel.set()
        self._pause_event.set()

    @property
    def cancelled(self):
        return self._cancel.is_set()

    # -- worker --------------------------------------------------------------

    def _maybe_inject_fault(self, block_id):
        """Raise via parameter/input injection only -- never by editing code."""
        injection = self.fault_injection
        if injection.get("window_zero") and block_id == injection.get("at_block", 0):
            # Simulate a zero window reaching the numeric kernel.
            from .hashing import median_filter2d

            median_filter2d(np.zeros((4, 4)), 0)
        if injection.get("bad_granularity") and block_id == injection.get(
            "at_block", 0
        ):
            from .hashing import fingerprint

            fingerprint(self.sample.block_pixels(block_id), 0)

    def _process_one(self, block_id):
        # Bound pixel buffers: block while budget exhausted, never drop.
        self._buffer_sem.acquire()
        try:
            self._pause_event.wait()
            if self._cancel.is_set():
                return block_id, None, "cancelled"
            state = self.states[block_id]
            with self._commit_lock:
                state.transition(RUNNING)
            try:
                self._maybe_inject_fault(block_id)
                block = self.sample.block_pixels(block_id)
                result = process_block(block, self.params)
                return block_id, result, None
            except ForensicsError as exc:
                return block_id, None, exc
            except Exception as exc:  # defensive: isolate to one block
                return block_id, None, ForensicsError("E_STATE", str(exc))
        finally:
            self._buffer_sem.release()

    def _commit(self, block_id, result):
        record = {
            "block_id": block_id,
            "bounds": list(self.sample.block_index.bounds(block_id)),
            "image_content_hash": self.image_hash,
            "version": self.snapshot.version,
            "version_signature": self.snapshot.signature,
            "algorithm_version": self.snapshot.algorithm_version,
            "content_hash": result["content_hash"],
            "fingerprint": result["fingerprint"],
            "analysis": result["analysis"],
        }
        with self._commit_lock:
            self.states[block_id].transition(COMMITTED)
            # Serial atomic checkpoint write inside the same serial section.
            self.store.write_block(record)
            self.results[block_id] = record
            self.committed_order.append(block_id)

    def run(self):
        """Process all non-committed blocks; returns a run summary."""
        self.write_header()
        pending = [
            i
            for i, st in enumerate(self.states)
            if st.state == PENDING or st.state == FAILED
        ]
        # Stable submission order by block id.
        pending.sort()
        if not pending:
            return self._summary()

        with ThreadPoolExecutor(max_workers=self.max_workers) as pool:
            futures = [pool.submit(self._process_one, bid) for bid in pending]
            for future in futures:
                block_id, result, error = future.result()
                if error == "cancelled":
                    continue
                if error is not None:
                    with self._commit_lock:
                        if self.states[block_id].state == RUNNING:
                            self.states[block_id].transition(FAILED)
                            self.states[block_id].last_error = error
                    self.failed[block_id] = error
                    continue
                self._commit(block_id, result)

        # Preserve a globally stable commit sequence regardless of thread timing.
        with self._commit_lock:
            self.committed_order.sort()
        return self._summary()

    def retry_failed(self):
        """Retry failed blocks; other blocks stay untouched."""
        failed_ids = sorted(self.failed.keys())
        for bid in failed_ids:
            # Legal path back into the pipeline: FAILED -> RUNNING -> PENDING.
            self.states[bid].transition(RUNNING)
            self.states[bid].state = PENDING
        self.failed = {}
        return self.run()

    def require_version(self, snapshot):
        """Reject mixing a committed block with a different parameter version."""
        if snapshot.version != self.snapshot.version:
            raise ForensicsError(
                E_VER,
                "cannot apply version %d to blocks frozen at version %d"
                % (snapshot.version, self.snapshot.version),
                field="version",
            )

    def _summary(self):
        return {
            "total": len(self.states),
            "committed": sum(1 for s in self.states if s.state == COMMITTED),
            "failed": [i for i, s in enumerate(self.states) if s.state == FAILED],
            "cancelled": self._cancel.is_set(),
            "committed_order": list(self.committed_order),
        }
