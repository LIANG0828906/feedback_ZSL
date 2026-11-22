"""Checkpoint persistence.

One state file per (sample, policy-version) run.  Every committed play
window triggers one atomic, serialized rewrite of the state file:
write to ``state.json.tmp`` then ``os.replace`` (atomic on POSIX and
Windows).  A module-level lock plus a single writer per manager
guarantees writes never interleave.

On load, the sample content hash and the policy parameter version must
both match; otherwise the whole snapshot is discarded and the run
restarts from zero - mixing old buffer state is forbidden.
"""
from __future__ import annotations

import json
import os
import threading


class CheckpointManager:
    def __init__(self, ckpt_dir):
        self.dir = ckpt_dir
        self.path = os.path.join(ckpt_dir, "state.json")
        self.tmp_path = self.path + ".tmp"
        self._lock = threading.Lock()
        os.makedirs(ckpt_dir, exist_ok=True)

    def write(self, state):
        """Atomically and serially persist ``state``."""
        blob = json.dumps(state, sort_keys=True)
        with self._lock:
            with open(self.tmp_path, "w") as fh:
                fh.write(blob)
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(self.tmp_path, self.path)

    def load(self, *, content_hash, policy_version):
        """Return the persisted state, or None if unusable.

        A corrupt file and any hash/version mismatch both discard the
        snapshot; partial reuse of old buffer state is forbidden.
        """
        try:
            with open(self.path, "r") as fh:
                state = json.load(fh)
        except (OSError, json.JSONDecodeError):
            return None
        if state.get("content_hash") != content_hash:
            self.discard()
            return None
        if state.get("policy_version") != policy_version:
            self.discard()
            return None
        return state

    def discard(self):
        with self._lock:
            for p in (self.path, self.tmp_path):
                try:
                    os.unlink(p)
                except FileNotFoundError:
                    pass

    def exists(self):
        return os.path.exists(self.path)
