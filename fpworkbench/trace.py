"""Deterministic trace log: fixed-seed runs produce byte-identical logs.
No wall-clock timestamps are ever written."""

from __future__ import annotations

import json


class TraceLog:
    def __init__(self):
        self._lines = []

    def event(self, kind, **fields):
        record = {"event": kind}
        record.update(fields)
        self._lines.append(json.dumps(record, sort_keys=True))

    def text(self):
        return "\n".join(self._lines) + "\n"

    def save(self, path):
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(self.text())
        return path
