"""Atomic snapshots and crash recovery.

Block measurements, two-stage gating intermediates, the parameter version
and a hash chain are written as JSON via a temp file plus atomic rename in
the same directory. Loading validates the version and the hash chain and
resumes metering of unfinished blocks without rewriting committed ones. A
broken chain or missing field aborts with E_REPORT_MISMATCH. Recovery does
not promise to produce an exit report.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
from pathlib import Path

from .errors import ReportMismatchError

REQUIRED_FIELDS = (
    "format_version",
    "params",
    "committed_blocks",
    "gating",
    "prev_hash",
    "hash",
)
FORMAT_VERSION = 1
GENESIS_HASH = "0" * 64
_SNAP_RE = re.compile(r"snap_(\d{6})\.json$")


def _content_hash(state: dict) -> str:
    body = {k: state[k] for k in state if k != "hash"}
    canonical = json.dumps(body, sort_keys=True, separators=(",", ":"),
                           ensure_ascii=False)
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


class SnapshotStore:
    def __init__(self, directory: str | Path):
        self.dir = Path(directory)
        self.dir.mkdir(parents=True, exist_ok=True)

    def _snapshots(self) -> list[Path]:
        snaps = [p for p in self.dir.iterdir() if _SNAP_RE.search(p.name)]
        return sorted(snaps, key=lambda p: p.name)

    def save(self, state: dict) -> Path:
        """Persist ``state`` (without ``hash``/``prev_hash``) atomically."""
        snaps = self._snapshots()
        prev_hash = GENESIS_HASH
        if snaps:
            prev = json.loads(snaps[-1].read_text(encoding="utf-8"))
            prev_hash = prev["hash"]
        full = dict(state)
        full["format_version"] = FORMAT_VERSION
        full["prev_hash"] = prev_hash
        full["hash"] = _content_hash(full)
        seq = int(_SNAP_RE.search(snaps[-1].name).group(1)) + 1 if snaps else 0
        tmp = self.dir / f".snap_{seq:06d}.json.tmp"
        final = self.dir / f"snap_{seq:06d}.json"
        tmp.write_text(json.dumps(full, ensure_ascii=False, indent=2,
                                  allow_nan=False), encoding="utf-8")
        with open(tmp, "rb") as fh:
            os.fsync(fh.fileno())
        os.replace(tmp, final)  # atomic rename within the same directory
        return final

    def load_latest(self) -> dict | None:
        """Validate the whole chain and return the latest valid state.

        Returns None when no snapshot exists. Any broken link, hash
        mismatch or missing field aborts with E_REPORT_MISMATCH.
        """
        snaps = self._snapshots()
        if not snaps:
            return None
        prev_hash = GENESIS_HASH
        latest = None
        for path in snaps:
            try:
                state = json.loads(path.read_text(encoding="utf-8"))
            except (json.JSONDecodeError, UnicodeDecodeError) as exc:
                raise ReportMismatchError(f"unreadable snapshot {path.name}: {exc}")
            for field_name in REQUIRED_FIELDS:
                if field_name not in state:
                    raise ReportMismatchError(
                        f"snapshot {path.name} missing field {field_name!r}")
            if state["format_version"] != FORMAT_VERSION:
                raise ReportMismatchError(
                    f"snapshot {path.name} has unknown format version")
            if state["prev_hash"] != prev_hash:
                raise ReportMismatchError(
                    f"snapshot {path.name} breaks the hash chain")
            if state["hash"] != _content_hash(state):
                raise ReportMismatchError(
                    f"snapshot {path.name} content hash mismatch")
            prev_hash = state["hash"]
            latest = state
        return latest


def unfinished_blocks(state: dict, total_blocks: int) -> list[int]:
    """Block indices still to meter; committed blocks are never rewritten."""
    committed = {b["index"] for b in state["committed_blocks"]}
    return [i for i in range(total_blocks) if i not in committed]
