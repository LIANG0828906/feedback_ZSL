"""Bounded in-memory window buffers with local-disk spill."""

from __future__ import annotations

import json
import os
import shutil
import uuid
from pathlib import Path
from typing import Any, Iterable, Iterator


class TempWorkspace:
    def __init__(self, base_dir: Path, token: str) -> None:
        self.base_dir = base_dir.resolve()
        self.token = token
        self.base_dir.mkdir(parents=True, exist_ok=True)
        self.path = self.base_dir / f"run-{token}-{os.getpid()}-{uuid.uuid4().hex[:8]}"
        self.path.mkdir(parents=True, exist_ok=True)

    def cleanup(self) -> None:
        shutil.rmtree(self.path, ignore_errors=True)

    def cleanup_stale(self, keep: int = 0) -> int:
        """Best-effort removal of stale directories for this state token."""

        removed = 0
        candidates = sorted(
            self.base_dir.glob(f"run-{self.token}-*"),
            key=lambda path: path.stat().st_mtime if path.exists() else 0,
            reverse=True,
        )
        for path in candidates[keep:] if keep else candidates:
            if path.is_dir() and path != self.path:
                shutil.rmtree(path, ignore_errors=True)
                removed += 1
        return removed


class SpillBuffer:
    """Accumulates aggregate rows, spilling to JSONL after a byte threshold."""

    def __init__(self, workspace: TempWorkspace, threshold_bytes: int) -> None:
        self.workspace = workspace
        self.threshold_bytes = max(1, threshold_bytes)
        self._rows: list[dict[str, Any]] = []
        self._estimated_bytes = 0
        self._files: list[Path] = []
        self.written_bytes = 0
        self.spill_file_count = 0

    @staticmethod
    def estimate_row(row: dict[str, Any]) -> int:
        return 96 + len(str(row.get("payment_id", ""))) + len(str(row.get("currency", "")))

    @property
    def pending_rows(self) -> int:
        return len(self._rows)

    @property
    def in_memory_bytes(self) -> int:
        return self._estimated_bytes

    def add_rows(self, rows: Iterable[dict[str, Any]]) -> None:
        for row in rows:
            self._rows.append(row)
            self._estimated_bytes += self.estimate_row(row)
        if self._estimated_bytes >= self.threshold_bytes:
            self._flush()

    def _flush(self) -> None:
        if not self._rows:
            return
        self.spill_file_count += 1
        path = self.workspace.path / f"spill-{self.spill_file_count:06d}.jsonl"
        with path.open("w", encoding="utf-8", newline="\n") as fh:
            for row in self._rows:
                line = json.dumps(row, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
                fh.write(line + "\n")
                self.written_bytes += len(line.encode("utf-8")) + 1
            fh.flush()
            os.fsync(fh.fileno())
        self._files.append(path)
        self._rows.clear()
        self._estimated_bytes = 0

    def iter_rows(self) -> Iterator[dict[str, Any]]:
        for path in self._files:
            with path.open("r", encoding="utf-8") as fh:
                for line in fh:
                    if line.strip():
                        yield json.loads(line)
        yield from self._rows

    def reset(self) -> None:
        self._rows.clear()
        self._estimated_bytes = 0
        self._files.clear()
        self.written_bytes = 0
        self.spill_file_count = 0

    def clear_after_apply(self) -> None:
        """Delete spill files after their rows have been committed."""

        for path in self._files:
            try:
                path.unlink()
            except FileNotFoundError:
                pass
        self._rows.clear()
        self._estimated_bytes = 0
        self._files.clear()
        self.written_bytes = 0
        self.spill_file_count = 0

    def cleanup(self) -> None:
        self.clear_after_apply()
        self.written_bytes = 0
        self.spill_file_count = 0
        self.workspace.cleanup()
