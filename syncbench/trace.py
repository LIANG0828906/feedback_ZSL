"""Deterministic trajectory log.

Log lines are sequence-numbered and carry no wall-clock timestamps, so
identical seeds and operation sequences produce byte-identical logs.
Cost figures are derived from deterministic operation counts (samples
processed, correlation length), never from measured time.
"""

from __future__ import annotations

import os


class TraceLogger:
    def __init__(self, path=None):
        self.path = path
        self._seq = 0
        self._lines = []

    def event(self, kind, **fields):
        self._seq += 1
        parts = " ".join(f"{k}={fields[k]}" for k in sorted(fields))
        line = f"{self._seq:05d} {kind}" + (f" {parts}" if parts else "")
        self._lines.append(line)

    def text(self):
        return "\n".join(self._lines) + ("\n" if self._lines else "")

    def flush(self):
        if not self.path:
            return
        os.makedirs(os.path.dirname(self.path) or ".", exist_ok=True)
        tmp = self.path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as fh:
            fh.write(self.text())
        os.replace(tmp, self.path)
