"""Atomic block-level progress snapshots and resume handling."""

from __future__ import annotations

import json
import os

from .errors import E_RESUME_MISMATCH, ResumeMismatchError, make_error

MANIFEST_NAME = "snapshot.json"


def snapshot_paths(snapshot_dir):
    return {
        "dir": snapshot_dir,
        "manifest": os.path.join(snapshot_dir, MANIFEST_NAME),
        "tmp": os.path.join(snapshot_dir, MANIFEST_NAME + ".tmp"),
        "master": os.path.join(snapshot_dir, "master.partial.wav"),
    }


def _atomic_json(path, payload):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, ensure_ascii=False, sort_keys=True, indent=2)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


def write_snapshot(snapshot_dir, payload):
    os.makedirs(snapshot_dir, exist_ok=True)
    _atomic_json(snapshot_paths(snapshot_dir)["manifest"], payload)


def read_snapshot(snapshot_dir):
    path = snapshot_paths(snapshot_dir)["manifest"]
    if not os.path.isfile(path):
        return None
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


def discard_progress(snapshot_dir):
    """Remove every committed artefact of an incompatible old run."""
    paths = snapshot_paths(snapshot_dir)
    for key in ("manifest", "tmp", "master"):
        p = paths[key]
        if os.path.isfile(p):
            os.remove(p)


def verify_resume(snapshot, cue, analysis_payload):
    """Compare a committed snapshot against the current deterministic inputs."""
    mismatches = []
    expected = {
        "cue_digest": cue.digest(),
        "total_samples": cue.total_samples,
        "block_samples": cue.block_samples,
        "output_master": cue.output_master,
        "decoupling": analysis_payload["decoupling"],
        "offsets": analysis_payload["offsets"],
        "limiter": analysis_payload["limiter"],
    }
    for key, value in expected.items():
        if snapshot.get(key) != value:
            mismatches.append(key)
    if mismatches:
        raise ResumeMismatchError(
            "snapshot no longer matches current inputs",
            code=E_RESUME_MISMATCH,
            details=make_error(
                E_RESUME_MISMATCH,
                "committed cue summary or decoupling parameters differ",
                details={"mismatched_fields": mismatches},
            )["details"],
        )


def committed_byte_offset(snapshot, channels=2, bits=16):
    blocks = int(snapshot["completed_blocks"])
    bytes_per_sample = bits // 8
    return blocks * int(snapshot["block_samples"]) * channels * bytes_per_sample
