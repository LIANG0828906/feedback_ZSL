"""Fault-injection channel.

Injections are applied at the data-processing entry points only
(operation ingest, or files on disk before they are read back). The
settler and snapshot modules are never modified or bypassed, and every
injected anomaly is intercepted before a new snapshot is written.
"""

from __future__ import annotations

import json
import os
from dataclasses import replace

from .model import MAP_BOUND, Operation

OP_INJECTION_KINDS = ("duplicate-op", "out-of-bounds", "late-op")
ALL_KINDS = OP_INJECTION_KINDS + ("truncated-snapshot", "corrupt-baseline")


class OpInjector:
    """Entry-point operation injector driven by parameters.

    ``kind`` selects the anomaly, ``at_step`` the step boundary at which
    the next ingested operation is tampered with.
    """

    def __init__(self, kind: str, at_step: int, late_window: int):
        if kind not in OP_INJECTION_KINDS:
            raise ValueError(f"unsupported op injection: {kind}")
        self.kind = kind
        self.at_step = int(at_step)
        self.late_window = int(late_window)
        self.report = None

    def transform(self, op: Operation, current_step: int):
        """Return the list of operations to offer in place of ``op``."""
        if self.report is not None or current_step != self.at_step:
            return [op]
        point = {"injection_point": "op_ingest", "kind": self.kind,
                 "at_step": self.at_step, "target": op.ref()}
        if self.kind == "duplicate-op":
            self.report = {**point, "expected_code": "E_OP_DUPLICATE"}
            return [op, op]
        if self.kind == "out-of-bounds":
            payload = dict(op.payload)
            payload["dx"] = MAP_BOUND * 10
            payload["dy"] = MAP_BOUND * 10
            tampered = replace(op, kind="move", payload=payload)
            self.report = {**point, "expected_code": "E_OP_OUT_OF_BOUNDS"}
            return [tampered]
        # late-op: rewind the declared tick far beyond the late window
        tampered = replace(op, tick=current_step - self.late_window - 5)
        self.report = {**point, "expected_code": "E_OP_LATE"}
        return [tampered]


def truncate_snapshot(snapshot_path: str, keep_bytes: int = 64) -> dict:
    """Corrupt the on-disk snapshot by truncation (crash mid-write)."""
    with open(snapshot_path, "rb") as fh:
        data = fh.read()
    with open(snapshot_path, "wb") as fh:
        fh.write(data[:keep_bytes])
    return {"injection_point": "snapshot_file", "kind": "truncated-snapshot",
            "path": snapshot_path, "original_bytes": len(data),
            "kept_bytes": min(keep_bytes, len(data)),
            "expected_code": "E_RECOVERY_INVALID"}


def corrupt_baseline(baseline_path: str, at_line: int = 2) -> dict:
    """Flip one hash character on a baseline line (keeps valid JSON)."""
    with open(baseline_path, "r", encoding="utf-8") as fh:
        lines = fh.read().splitlines()
    index = min(max(at_line, 2), len(lines)) - 1
    entry = json.loads(lines[index])
    original = entry["state_hash"]
    flipped = ("0" if original[0] != "0" else "1") + original[1:]
    entry["state_hash"] = flipped
    lines[index] = json.dumps(entry, sort_keys=True, separators=(",", ":"))
    with open(baseline_path, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines) + "\n")
    return {"injection_point": "baseline_file", "kind": "corrupt-baseline",
            "path": baseline_path, "line": index + 1,
            "step": entry.get("step"),
            "expected_code": "E_BASELINE_DIVERGED"}
