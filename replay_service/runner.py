"""Authoritative fixed-timestep advance task.

One task per room at a time (lock file with stale-pid detection),
cooperative cancellation at step boundaries, and a crash simulation
that leaves only the last committed snapshot behind.
"""

from __future__ import annotations

import json
import os

from .buffer import OpBuffer
from .errors import BusyError, CancelledError, HardCrash
from .inject import OpInjector
from .model import MODEL_VERSION, Operation, RoomState
from .model import chain_link, genesis_chain, state_doc, state_hash
from .settle import apply_operation
from .snapshot import SnapshotStore

LOCK_NAME = "room.lock"
CANCEL_NAME = "CANCEL"


def load_sample(sample_path):
    with open(sample_path, "r", encoding="utf-8") as fh:
        sample = json.load(fh)
    ops = [Operation.from_dict(doc) for doc in sample["ops"]]
    return sample, ops


class AdvanceTask:
    def __init__(self, room_dir, sample_path, late_window=2):
        self.room_dir = room_dir
        self.sample, self.ops = load_sample(sample_path)
        self.room = self.sample.get("room", "room")
        self.total_steps = int(self.sample["steps"])
        self.late_window = int(late_window)
        self.store = SnapshotStore(room_dir)
        self.trace_path = os.path.join(room_dir, "trace.jsonl")
        self.events_path = os.path.join(room_dir, "events.jsonl")
        self.lock_path = os.path.join(room_dir, LOCK_NAME)
        self.cancel_path = os.path.join(room_dir, CANCEL_NAME)
        os.makedirs(room_dir, exist_ok=True)

    # ---- concurrency guard -------------------------------------------------
    def _acquire_lock(self):
        for _attempt in range(2):
            try:
                fd = os.open(self.lock_path,
                             os.O_CREAT | os.O_EXCL | os.O_WRONLY)
            except FileExistsError:
                if self._lock_holder_alive():
                    raise BusyError("room already has an advance task",
                                    room=self.room, room_dir=self.room_dir)
                os.unlink(self.lock_path)  # stale lock from a killed task
                continue
            with os.fdopen(fd, "w") as fh:
                fh.write(str(os.getpid()))
            return

    def _lock_holder_alive(self):
        try:
            with open(self.lock_path, "r", encoding="utf-8") as fh:
                pid = int(fh.read().strip() or "0")
        except (OSError, ValueError):
            return False
        if pid <= 0:
            return False
        try:
            os.kill(pid, 0)
        except OSError:
            return False
        return True

    def _release_lock(self):
        try:
            os.unlink(self.lock_path)
        except OSError:
            pass

    def _consume_cancel_request(self):
        try:
            os.unlink(self.cancel_path)
        except OSError:
            pass

    # ---- main loop ----------------------------------------------------------
    def run(self, resume=False, cancel_requested=None, crash_at=None,
            injector: OpInjector = None):
        self._acquire_lock()
        trace_fh = None
        events_fh = None
        try:
            state, chain, start_step, arrival_idx, buffer = self._bootstrap(
                resume)
            trace_fh = open(self.trace_path,
                            "a" if resume else "w", encoding="utf-8")
            events_fh = open(self.events_path,
                             "a" if resume else "w", encoding="utf-8")
            events = []
            for step in range(start_step + 1, self.total_steps + 1):
                if cancel_requested is None:
                    cancelled = os.path.exists(self.cancel_path)
                else:
                    cancelled = cancel_requested(step)
                if cancelled:
                    self._consume_cancel_request()
                    raise CancelledError("cancelled at step boundary",
                                         step=step,
                                         last_committed_step=step - 1)
                arrival_idx = self._ingest(step, arrival_idx, buffer,
                                           events, injector)
                applied_refs = []
                for op in buffer.drain(step):
                    err = apply_operation(state, op)
                    if err is not None:
                        events.append({"step": step, **err.to_dict()})
                        continue
                    buffer.mark_applied(op)
                    applied_refs.append(op.ref())
                s_hash = state_hash(state)
                prev = chain[-1]["chain_hash"] if chain else genesis_chain()
                c_hash = chain_link(prev, s_hash)
                chain.append({"step": step, "state_hash": s_hash,
                              "chain_hash": c_hash})
                trace_fh.write(json.dumps(
                    {"step": step, "applied": applied_refs,
                     "state_hash": s_hash, "chain_hash": c_hash,
                     "watermark": buffer.watermark()},
                    sort_keys=True, separators=(",", ":")) + "\n")
                trace_fh.flush()
                self.store.commit({
                    "version": MODEL_VERSION, "room": self.room,
                    "step": step, "state": state_doc(state),
                    "watermark": buffer.watermark(),
                    "arrival_idx": arrival_idx, "chain": list(chain)})
                if crash_at is not None and step == crash_at:
                    raise HardCrash(step)
            for event in events:
                events_fh.write(json.dumps(event, sort_keys=True) + "\n")
            return {"status": "completed", "room": self.room,
                    "steps": self.total_steps, "events": len(events),
                    "tip": chain[-1]["chain_hash"] if chain else None}
        finally:
            for fh in (trace_fh, events_fh):
                if fh is not None:
                    fh.close()
            self._release_lock()

    def _bootstrap(self, resume):
        buffer = OpBuffer(self.late_window)
        if resume:
            doc = self.store.load()  # raises E_RECOVERY_INVALID on any defect
            state = RoomState.from_dict(doc["state"])
            buffer.load_watermark(doc["watermark"])
            return state, list(doc["chain"]), int(doc["step"]), \
                int(doc["arrival_idx"]), buffer
        state = RoomState.from_dict(self.sample["initial_state"])
        return state, [], 0, 0, buffer

    def _ingest(self, step, arrival_idx, buffer, events, injector):
        while arrival_idx < len(self.ops) \
                and self.ops[arrival_idx].tick <= step:
            op = self.ops[arrival_idx].with_arrival(arrival_idx)
            arrival_idx += 1
            offered = injector.transform(op, step) if injector else [op]
            for item in offered:
                buffer.offer(item, step, events)
        return arrival_idx
