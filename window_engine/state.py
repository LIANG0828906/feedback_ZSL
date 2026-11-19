"""Exclusive local state backend with atomic snapshots and local spilling.

Layout under ``state_dir`` (one job id owns one directory):

    LOCK                exclusive process lock for the whole job
    meta.json           current pointer, watermark, consumed batch list
    shard-<u>.bin       committed shard snapshot (atomic replace per commit);
                        evicted shards keep only this on-disk copy (spill)
    output.json         emitted window rows, rewritten atomically per commit

Every snapshot carries a SHA-256 checksum. Truncated / tampered files fail
verification and surface as the stable business error ``E_STATE``; the bad
checkpoint is then removed so a later job can never mistake it for committed
state.
"""

from __future__ import annotations

import hashlib
import json
import os
import pickle
import threading
import time
from dataclasses import dataclass, field

from .errors import StateError
from .semantics import Watermark
from .util import FileLock, atomic_write, ensure_dir, read_json, write_json

SNAPSHOT_VERSION = 1


def dumps_snapshot(payload: dict) -> bytes:
    body = pickle.dumps(payload, protocol=5)
    return hashlib.sha256(body).digest() + body


def loads_snapshot(blob: bytes) -> dict:
    if len(blob) <= 32:
        raise StateError("state snapshot truncated")
    checksum, body = blob[:32], blob[32:]
    if hashlib.sha256(body).digest() != checksum:
        raise StateError("state snapshot checksum mismatch")
    try:
        payload = pickle.loads(body)
    except Exception as exc:  # noqa: BLE001 - any decode failure is corruption
        raise StateError(f"state snapshot undecodable: {exc}") from exc
    if not isinstance(payload, dict) or payload.get("version") != SNAPSHOT_VERSION:
        raise StateError("state snapshot version unsupported")
    return payload


@dataclass
class ShardState:
    """Mutable per-unit accumulation state."""

    unit: int
    # window_start -> [event_count, amount_cents]
    windows: dict[int, list[int]] = field(default_factory=dict)
    # dedup keys already applied (open + closed events)
    seen: set[str] = field(default_factory=set)
    # window_start -> late event count
    late: dict[int, int] = field(default_factory=dict)
    # dedup keys of events that arrived after their window closed
    late_seen: set[str] = field(default_factory=set)
    # window_start -> committed marker once the window was emitted
    completed: dict[int, bool] = field(default_factory=dict)
    resident: bool = True

    def estimate_bytes(self) -> int:
        # Approximation used purely for the spill threshold decision.
        total = sum(len(key) + 40 for key in self.seen)
        total += len(self.windows) * 32
        total += len(self.late) * 24
        total += sum(len(key) + 40 for key in self.late_seen)
        total += len(self.completed) * 16
        return total


class StateBackend:
    def __init__(self, state_dir: str, spill_dir: str, memory_bytes: int):
        self.state_dir = ensure_dir(state_dir)
        self.spill_dir = ensure_dir(spill_dir)
        self.memory_bytes = memory_bytes
        self.lock_path = os.path.join(self.state_dir, "LOCK")
        self.meta_path = os.path.join(self.state_dir, "meta.json")
        self.output_path = os.path.join(self.state_dir, "output.json")
        self._lock = FileLock(self.lock_path)
        self.shards: dict[int, ShardState] = {}
        self.watermark = Watermark(0)
        self.consumed_batches: list[int] = []
        self.output_rows: list[dict] = []
        self.dedup_dropped = 0
        self.spill_bytes = 0
        self.recoveries = 0
        self.faults: list[dict] = []
        self.job_done = False
        self.recovery_points: list[str] = []
        # One lock per physical unit: concurrent batches never partially write
        # the same window; the commit lock orders whole-checkpoint publication.
        self._shard_locks: dict[int, threading.Lock] = {}
        self._commit_lock = threading.Lock()

    # ------------------------------------------------------------------ open
    def open(self) -> bool:
        """Acquire the exclusive job lock and restore committed state.

        Returns True when an existing checkpoint was resumed.
        """

        try:
            self._lock.acquire()
        except OSError as exc:
            raise StateError(
                "state backend is locked by another process",
                details={"path": self.lock_path},
            ) from exc
        if not os.path.exists(self.meta_path):
            return False
        try:
            meta = read_json(self.meta_path)
            self.watermark = Watermark.restore(meta["watermark"])
            self.consumed_batches = list(meta["consumed_batches"])
            self.dedup_dropped = int(meta.get("dedup_dropped", 0))
            self.spill_bytes = int(meta.get("spill_bytes", 0))
            self.recoveries = int(meta.get("recoveries", 0)) + 1
            self.faults = list(meta.get("faults", []))
            self.job_done = bool(meta.get("job_done", False))
            self.recovery_points = list(meta.get("recovery_points", []))
            self.recovery_points.append(meta["commit_point"])
            for unit_text, name in meta["shards"].items():
                with open(
                    os.path.join(self.state_dir, name), "rb"
                ) as snapshot_fh:
                    payload = loads_snapshot(snapshot_fh.read())["shard"]
                shard = ShardState(unit=int(unit_text))
                shard.windows = payload["windows"]
                shard.seen = payload["seen"]
                shard.late = payload["late"]
                shard.late_seen = payload.get("late_seen", set())
                shard.completed = payload.get("completed", {})
                self.shards[int(unit_text)] = shard
            if os.path.exists(self.output_path):
                self.output_rows = read_json(self.output_path)["rows"]
        except StateError:
            self._discard_checkpoint()
            raise
        except (OSError, KeyError, pickle.UnpicklingError) as exc:
            self._discard_checkpoint()
            raise StateError(f"state backend corrupted: {exc}") from exc
        return True

    def close(self) -> None:
        self._lock.release()

    def _discard_checkpoint(self) -> None:
        for name in os.listdir(self.state_dir):
            if name == "LOCK":
                continue
            try:
                os.remove(os.path.join(self.state_dir, name))
            except OSError:
                pass

    def initialize_shards(self, partitions: int) -> None:
        for unit in range(partitions):
            self.shards.setdefault(unit, ShardState(unit=unit))
            self._shard_locks.setdefault(unit, threading.Lock())
        for unit in list(self.shards):
            self._shard_locks.setdefault(unit, threading.Lock())

    def shard_lock(self, unit: int) -> threading.Lock:
        return self._shard_locks[unit]

    # ----------------------------------------------------------------- shards
    def shard_path(self, unit: int) -> str:
        return os.path.join(self.state_dir, f"shard-{unit}.bin")

    def shard(self, unit: int) -> ShardState:
        shard = self.shards[unit]
        if not shard.resident:
            self._load_shard(shard)
        return shard

    def _load_shard(self, shard: ShardState) -> None:
        try:
            with open(self.shard_path(shard.unit), "rb") as snapshot_fh:
                payload = loads_snapshot(snapshot_fh.read())["shard"]
        except FileNotFoundError as exc:
            raise StateError(
                f"spill snapshot missing for unit {shard.unit}"
            ) from exc
        shard.windows = payload["windows"]
        shard.seen = payload["seen"]
        shard.late = payload["late"]
        shard.late_seen = payload.get("late_seen", set())
        shard.completed = payload.get("completed", {})
        shard.resident = True

    def _persist_shard(self, shard: ShardState) -> int:
        blob = dumps_snapshot(
            {
                "version": SNAPSHOT_VERSION,
                "shard": {
                    "windows": shard.windows,
                    "seen": shard.seen,
                    "late": shard.late,
                    "late_seen": shard.late_seen,
                    "completed": shard.completed,
                },
            }
        )
        atomic_write(self.shard_path(shard.unit), blob)
        return len(blob)

    def maybe_spill(self) -> None:
        """Evict the largest resident shard when state exceeds the threshold.

        The on-disk shard snapshot doubles as the spill file (same disk per
        the task constraints), so eviction only drops the in-memory copy.
        """

        resident = [s for s in self.shards.values() if s.resident]
        total = sum(s.estimate_bytes() for s in resident)
        while total > self.memory_bytes and len(resident) > 1:
            victim = max(resident, key=lambda s: (s.estimate_bytes(), -s.unit))
            size = self._persist_shard(victim)
            self.spill_bytes += size
            victim.windows = {}
            victim.seen = set()
            victim.late = {}
            victim.late_seen = set()
            victim.completed = {}
            victim.resident = False
            resident.remove(victim)
            total = sum(s.estimate_bytes() for s in resident)

    def cleanup_spill(self) -> None:
        # Spill lives inside the state dir; keep the final checkpoint, drop
        # anything left in the dedicated spill directory.
        if not os.path.isdir(self.spill_dir):
            return
        for name in os.listdir(self.spill_dir):
            if name.endswith(".spill"):
                try:
                    os.remove(os.path.join(self.spill_dir, name))
                except OSError:
                    pass

    # ---------------------------------------------------------------- commit
    def commit(
        self,
        batch_id: int,
        *,
        job_done: bool,
        extra_faults: list[dict] | None = None,
    ) -> str:
        """Atomically persist shards + watermark + consumed batch list.

        Returns the commit point identifier. Meta is replaced last, so a crash
        always leaves exactly one consistent checkpoint.
        """

        with self._commit_lock:
            if extra_faults:
                for item in extra_faults:
                    if item not in self.faults:
                        self.faults.append(item)
            if batch_id not in self.consumed_batches:
                self.consumed_batches.append(batch_id)
            self.consumed_batches.sort()

            shard_pointers: dict[str, str] = {}
            for unit, shard in sorted(self.shards.items()):
                if shard.resident:
                    self._persist_shard(shard)
                shard_pointers[str(unit)] = os.path.basename(self.shard_path(unit))

            atomic_write(
                self.output_path,
                json.dumps({"rows": self.output_rows}, sort_keys=True).encode(
                    "utf-8"
                ),
            )

            self.job_done = job_done
            meta = {
                "version": SNAPSHOT_VERSION,
                "commit_point": f"cp-batch-{batch_id}",
                "committed_at_ms": int(time.time() * 1000),
                "last_batch": batch_id,
                "consumed_batches": self.consumed_batches,
                "shards": shard_pointers,
                "watermark": self.watermark.snapshot(),
                "dedup_dropped": self.dedup_dropped,
                "spill_bytes": self.spill_bytes,
                "recoveries": self.recoveries,
                "recovery_points": self.recovery_points,
                "faults": self.faults,
                "job_done": self.job_done,
            }
            write_json(self.meta_path, meta)

            keep = set(shard_pointers.values()) | {
                "LOCK",
                "meta.json",
                "output.json",
            }
            for name in os.listdir(self.state_dir):
                if name not in keep:
                    try:
                        os.remove(os.path.join(self.state_dir, name))
                    except OSError:
                        pass
            return meta["commit_point"]
