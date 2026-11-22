"""Frame ingress: offline (single-threaded) and threaded receivers.

Both implementations deliver, for every virtual tick ``t``, exactly the
set of arrival records with ``arrival_ts <= t`` in deterministic
(arrival_ts, seq, dup) order.  The threaded receiver uses worker
threads feeding one bounded queue; a watermark (minimum not-yet-pushed
arrival_ts across workers) tells the engine when the set for tick ``t``
is complete, so thread interleaving can never change the admitted set.

Back-pressure policy when the bounded queue is full:
  * ``block`` (default, deterministic): workers wait - output identical
    to the single-threaded run;
  * ``drop_oldest``: the oldest queued record is discarded and a drop
    record is emitted (explainable, but intentionally lossy).
"""
from __future__ import annotations

import heapq
import queue
import threading


class OfflineIngress:
    """Single-threaded ingress over the sorted arrival list."""

    def __init__(self, arrivals, skip=0):
        self._arrivals = arrivals
        self._pos = skip
        self._pending = []

    @property
    def exhausted(self):
        return self._pos >= len(self._arrivals) and not self._pending

    def poll(self, t_ms):
        while (self._pos < len(self._arrivals)
               and self._arrivals[self._pos].arrival_ts <= t_ms):
            self._pending.append(self._arrivals[self._pos])
            self._pos += 1
        ready, self._pending = self._pending, []
        return ready


class ThreadedIngress:
    """Multi-worker receiver with bounded queue and watermark gating."""

    def __init__(self, arrivals, *, n_workers=3, max_pending_frames=64,
                 max_bytes_per_worker=1 << 20, skip=0, policy="block"):
        self._arrivals = list(arrivals[skip:])
        self._n_workers = max(1, n_workers)
        self._policy = policy
        self._queue = queue.Queue(maxsize=max_pending_frames)
        self._heap = []
        self._done_workers = 0
        self.drop_records = []
        self._max_bytes = max_bytes_per_worker
        self._threads = []
        # round-robin partition keeps each worker's stream ordered
        parts = [self._arrivals[i::self._n_workers]
                 for i in range(self._n_workers)]
        self._wm_values = [p[0].arrival_ts if p else None for p in parts]
        self._wm_lock = threading.Lock()
        for wid, part in enumerate(parts):
            th = threading.Thread(target=self._worker, args=(wid, part),
                                  daemon=True, name="rx-%d" % wid)
            self._threads.append(th)
            th.start()

    def _worker(self, wid, part):
        bytes_used = 0
        try:
            for idx, rec in enumerate(part):
                bytes_used += 4 * 320  # frame payload estimate
                if bytes_used > self._max_bytes:
                    self.drop_records.append({
                        "seq": rec.seq, "arrival_ts": rec.arrival_ts,
                        "reason": "worker_memory_limit",
                    })
                    continue
                key = (rec.arrival_ts, rec.seq, rec.dup, idx)
                if self._policy == "block":
                    self._queue.put((key, rec))      # blocks when full
                else:
                    try:
                        self._queue.put_nowait((key, rec))
                    except queue.Full:
                        try:
                            dropped = self._queue.get_nowait()
                            self.drop_records.append({
                                "seq": dropped[1].seq,
                                "arrival_ts": dropped[1].arrival_ts,
                                "reason": "buffer_full_drop_oldest",
                            })
                        except queue.Empty:
                            pass
                        self._queue.put((key, rec))
                with self._wm_lock:
                    nxt = part[idx + 1].arrival_ts if idx + 1 < len(part) \
                        else None
                    self._wm_values[wid] = nxt
        finally:
            with self._wm_lock:
                self._done_workers += 1

    def _watermark_value(self):
        with self._wm_lock:
            vals = [v for v in self._wm_values if v is not None]
            if self._done_workers == self._n_workers and not vals:
                return None
            return min(vals) if vals else None

    @property
    def exhausted(self):
        return (self._done_workers == self._n_workers
                and self._queue.empty() and not self._heap)

    def poll(self, t_ms):
        # wait until every record with arrival_ts <= t has been pushed
        while True:
            self._drain()
            wm = self._watermark_value()
            workers_done = self._done_workers == self._n_workers
            if wm is None:
                if workers_done:
                    break
            elif wm > t_ms:
                break
            if workers_done and self._queue.empty():
                break
            threading.Event().wait(0.0005)
        self._drain()
        ready = []
        while self._heap and self._heap[0][0] <= t_ms:
            ready.append(heapq.heappop(self._heap)[3])
        return ready

    def _drain(self):
        while True:
            try:
                key, rec = self._queue.get_nowait()
            except queue.Empty:
                return
            heapq.heappush(self._heap, (key[0], key[1], key[2], rec))
