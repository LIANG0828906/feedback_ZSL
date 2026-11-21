"""Deterministic JSONL trace log (logical sequence numbers only)."""

import json


class TraceLog:
    def __init__(self, path):
        import os
        self.path = str(path)
        os.makedirs(os.path.dirname(self.path), exist_ok=True)
        self._fh = open(self.path, "w", encoding="utf-8", newline="\n")
        self.seq = 0

    def log(self, event, **fields):
        rec = {"seq": self.seq, "event": event}
        rec.update(fields)
        self._fh.write(json.dumps(rec, sort_keys=True, separators=(",", ":")) + "\n")
        self.seq += 1

    def close(self):
        self._fh.close()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
