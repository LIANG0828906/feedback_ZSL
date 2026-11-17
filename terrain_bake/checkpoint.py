"""Crash-safe, resumable bake progress snapshots.

The snapshot is a single JSON document written atomically (tmp file +
os.replace) after each chunk is fully committed. It records the input
parameter digest, the heightfield digest, the seam-repair outcome and every
completed chunk with the digests of its committed outputs.

On startup the snapshot is validated: a corrupt file, a structural problem,
an input-digest mismatch or a committed output whose bytes no longer match
its recorded digest all cause the snapshot to be discarded wholesale and the
bake to restart from scratch. Stale partial state is never reused.
"""

import hashlib
import json
import os

SNAPSHOT_NAME = ".bake_checkpoint.json"
SNAPSHOT_VERSION = 1


def input_digest(params):
    """Canonical digest of every parameter that affects the bake output."""
    blob = json.dumps(params, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(blob.encode("utf-8")).hexdigest()


def _snapshot_path(out_dir):
    return os.path.join(out_dir, SNAPSHOT_NAME)


def _atomic_write_json(path, payload):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, indent=2)
        fh.write("\n")
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


class Checkpoint:
    def __init__(self, out_dir, digest, heights_digest):
        self.out_dir = out_dir
        self.payload = {
            "version": SNAPSHOT_VERSION,
            "status": "in_progress",
            "input_digest": digest,
            "heights_digest": heights_digest,
            "seam_repair": {"done": False, "repaired_boundary_vertices": 0},
            "resume_count": 0,
            "completed_chunks": [],
        }

    @property
    def completed(self):
        return {(c["coord"][0], c["coord"][1]) for c in
                self.payload["completed_chunks"]}

    def record_chunk(self, coord, digests):
        self.payload["completed_chunks"].append({
            "coord": [coord[0], coord[1]],
            "digests": digests,
        })

    def set_seam_repair(self, repaired_count):
        self.payload["seam_repair"] = {
            "done": True,
            "repaired_boundary_vertices": int(repaired_count),
        }

    def mark_complete(self):
        self.payload["status"] = "complete"

    def commit(self):
        _atomic_write_json(_snapshot_path(self.out_dir), self.payload)


def load_checkpoint(out_dir, expected_input_digest, verify_outputs=None):
    """Load and validate a snapshot; returns a Checkpoint or None.

    None means "no usable snapshot: start over". `verify_outputs` is an
    optional callable(coord, digests) -> bool re-checking committed files.
    """
    path = _snapshot_path(out_dir)
    if not os.path.exists(path):
        return None
    try:
        with open(path, "r", encoding="utf-8") as fh:
            payload = json.load(fh)
    except (OSError, ValueError):
        return None
    try:
        valid = (
            payload["version"] == SNAPSHOT_VERSION
            and payload["input_digest"] == expected_input_digest
            and isinstance(payload["completed_chunks"], list)
            and isinstance(payload["seam_repair"]["repaired_boundary_vertices"], int)
            and isinstance(payload["resume_count"], int)
        )
    except (KeyError, TypeError):
        return None
    if not valid:
        return None
    if verify_outputs is not None:
        for entry in payload["completed_chunks"]:
            coord = (entry["coord"][0], entry["coord"][1])
            if not verify_outputs(coord, entry["digests"]):
                return None
    checkpoint = Checkpoint.__new__(Checkpoint)
    checkpoint.out_dir = out_dir
    checkpoint.payload = payload
    checkpoint.payload["resume_count"] += 1
    return checkpoint
