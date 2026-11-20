"""确定性轨迹日志：JSONL，逻辑序号，无时间戳，保证逐字节可复现。"""

from __future__ import annotations

import json
from pathlib import Path


class TraceLogger:
    def __init__(self, path):
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._seq = 0
        self._fh = open(self.path, "w", encoding="utf-8", newline="\n")

    def log(self, event: str, **fields) -> None:
        record = {"seq": self._seq, "event": event}
        record.update(fields)
        self._fh.write(json.dumps(record, sort_keys=True, ensure_ascii=False) + "\n")
        self._fh.flush()
        self._seq += 1

    def close(self) -> None:
        if not self._fh.closed:
            self._fh.close()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
        return False
