"""Byte-stable JSONL trace writer."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any, Callable

from .errors import E_IO, MixError


class TraceWriter:
    def __init__(
        self,
        path: str | os.PathLike[str],
        *,
        append: bool = False,
        listener: Callable[[dict[str, Any]], None] | None = None,
    ):
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._seq = 0
        self._listener = listener
        if append and self.path.exists():
            self._seq = self._repair_and_last_sequence()
            self._handle = self.path.open("a", encoding="utf-8", newline="\n")
        else:
            self._handle = self.path.open("w", encoding="utf-8", newline="\n")

    def _repair_and_last_sequence(self) -> int:
        """Drop an incomplete tail left by a forced process termination."""

        last = 0
        try:
            data = self.path.read_bytes()
            valid_end = 0
            offset = 0
            while offset < len(data):
                newline = data.find(b"\n", offset)
                if newline < 0:
                    break
                raw = data[offset:newline]
                try:
                    value = json.loads(raw.decode("utf-8"))
                except (UnicodeDecodeError, json.JSONDecodeError):
                    break
                if not isinstance(value, dict):
                    break
                last = max(last, int(value.get("seq", 0)))
                offset = newline + 1
                valid_end = offset
            if valid_end < len(data):
                with self.path.open("r+b") as handle:
                    handle.truncate(valid_end)
        except OSError as exc:
            raise MixError(E_IO, f"cannot read existing trace: {exc}", details={"path": str(self.path)}) from exc
        return last

    def emit(self, event: str, **fields: Any) -> dict[str, Any]:
        self._seq += 1
        value: dict[str, Any] = {"seq": self._seq, "event": event}
        value.update(fields)
        line = json.dumps(
            value,
            ensure_ascii=True,
            allow_nan=False,
            sort_keys=True,
            separators=(",", ":"),
        )
        try:
            self._handle.write(line + "\n")
            self._handle.flush()
            os.fsync(self._handle.fileno())
        except OSError as exc:
            raise MixError(E_IO, f"cannot write trace: {exc}", details={"path": str(self.path)}) from exc
        if self._listener is not None:
            try:
                self._listener(dict(value))
            except Exception:
                # A GUI observer must never break the recording transaction.
                pass
        return value

    @property
    def sequence(self) -> int:
        return self._seq

    def close(self) -> None:
        if not self._handle.closed:
            self._handle.close()

    def __enter__(self) -> "TraceWriter":
        return self

    def __exit__(self, *_exc: object) -> None:
        self.close()
