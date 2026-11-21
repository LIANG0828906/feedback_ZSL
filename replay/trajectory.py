"""Deterministic JSONL trajectory output."""

from __future__ import annotations

import hashlib
import os
import tempfile

from .engine import RunResult, serialize_log_record


def trajectory_bytes(run: RunResult) -> bytes:
    lines = [serialize_log_record(step.trajectory_record()) for step in run.steps]
    return ("\n".join(lines) + ("\n" if lines else "")).encode("ascii")


def write_trajectory(run: RunResult, path: str) -> dict[str, str]:
    payload = trajectory_bytes(run)
    directory = os.path.dirname(os.path.abspath(path)) or "."
    os.makedirs(directory, exist_ok=True)
    fd, temp_path = tempfile.mkstemp(prefix=".tmp-trajectory-", dir=directory)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
    except Exception:
        try:
            os.unlink(temp_path)
        except OSError:
            pass
        raise
    return {
        "path": path,
        "sha256": hashlib.sha256(payload).hexdigest(),
        "lines": len(run.steps),
        "mode": run.mode,
    }
