"""Crash-safe render snapshots.

Two generations of the snapshot are kept (``snapshot.2.json`` newest,
``snapshot.1.json`` previous).  Commits are atomic (tmp file + fsync +
rename).  On load the newest fully-committed snapshot wins; a corrupt newest
snapshot falls back to the previous committed one; if both are unusable the
render restarts from zero without any manual cleanup.
"""

from __future__ import annotations

import json
import os
from pathlib import Path

SNAPSHOT_NEW = "snapshot.2.json"
SNAPSHOT_OLD = "snapshot.1.json"


def _atomic_write(path: Path, payload: dict) -> None:
    tmp = path.with_suffix(path.suffix + ".tmp")
    data = json.dumps(payload, sort_keys=True).encode("utf-8")
    with open(tmp, "wb") as fh:
        fh.write(data)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


def commit(work_dir: str | Path, snapshot: dict) -> None:
    """Atomically commit a new snapshot generation."""
    work = Path(work_dir)
    work.mkdir(parents=True, exist_ok=True)
    new = work / SNAPSHOT_NEW
    old = work / SNAPSHOT_OLD
    if new.exists():
        os.replace(new, old)
    snap = dict(snapshot)
    snap["committed"] = True
    _atomic_write(new, snap)


def _load_one(path: Path) -> dict | None:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError, UnicodeDecodeError):
        return None
    if not isinstance(data, dict) or not data.get("committed"):
        return None
    required = ("project_digest", "detection", "completed_blocks", "block_size", "bus_raw_bytes")
    if any(k not in data for k in required):
        return None
    return data


def load_latest(work_dir: str | Path) -> dict | None:
    """Load the newest valid snapshot, falling back to the previous one."""
    work = Path(work_dir)
    for name in (SNAPSHOT_NEW, SNAPSHOT_OLD):
        snap = _load_one(work / name)
        if snap is not None:
            return snap
    return None


def wipe(work_dir: str | Path) -> None:
    """Remove all checkpoint state (used after E_RESUME_MISMATCH)."""
    work = Path(work_dir)
    if not work.exists():
        return
    for entry in sorted(work.rglob("*"), reverse=True):
        if entry.is_file() or entry.is_symlink():
            entry.unlink()
        elif entry.is_dir():
            entry.rmdir()
