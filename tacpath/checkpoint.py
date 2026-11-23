"""Persistent checkpoints with atomic, serialized writes.

One JSON snapshot per (map, pair, params). Writes go to a temp file and
are committed with os.replace under a lock, so concurrent or
interrupted writers never interleave or leave a torn snapshot.

On load, the map content hash, the fixed-point parameter version and
the algorithm version must all match; any mismatch invalidates the
whole snapshot (it is deleted) and forces a full recompute. Partial
mixing of stale segments is impossible by construction: segments are
only ever read from the single fully-matched snapshot.
"""

import hashlib
import json
import os
import threading

ALGO_VERSION = "tacpath-algo/1.0.0"


def param_version(params):
    """Version string derived from every fixed-point/behaviour parameter."""
    payload = {
        "fp_scale": params["fp_scale"],
        "neighborhood": params["neighborhood"],
        "weight_num": params["weight_num"],
        "weight_den": params["weight_den"],
        "tolerance_fp": params["tolerance_fp"],
        "step_fp": params["step_fp"],
        "turn_mrad": params["turn_mrad"],
    }
    blob = json.dumps(payload, sort_keys=True).encode()
    return hashlib.sha256(blob).hexdigest()[:16]


def segment_hash(payload):
    blob = json.dumps(payload, sort_keys=True).encode()
    return hashlib.sha256(blob).hexdigest()


class CheckpointStore:
    def __init__(self, path):
        self.path = path
        self._lock = threading.Lock()

    def load(self, map_hash, p_version, algo_version=ALGO_VERSION):
        """Return the snapshot if every version matches, else None."""
        if not os.path.exists(self.path):
            return None
        try:
            with open(self.path, "r") as fh:
                data = json.load(fh)
        except (OSError, json.JSONDecodeError):
            self.discard()
            return None
        if (data.get("map_hash") != map_hash
                or data.get("param_version") != p_version
                or data.get("algo_version") != algo_version):
            self.discard()  # stale snapshot: full recompute, no mixing
            return None
        return data

    def save(self, snapshot):
        """Atomically and serially commit a snapshot."""
        os.makedirs(os.path.dirname(os.path.abspath(self.path)),
                    exist_ok=True)
        tmp = self.path + ".tmp"
        with self._lock:
            with open(tmp, "w") as fh:
                json.dump(snapshot, fh)
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(tmp, self.path)

    def discard(self):
        try:
            os.remove(self.path)
        except OSError:
            pass
