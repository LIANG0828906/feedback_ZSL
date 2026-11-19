from __future__ import annotations

import json
import shutil
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Iterator

from .errors import MemoryLimitError, SettlementError
from .util import canonical_json


@dataclass
class SpillResult:
    records: Iterable[dict[str, Any]]
    bytes_written: int
    files: int
    path: Path | None


class SpillManager:
    def __init__(
        self,
        base_dir: str | Path,
        *,
        threshold_bytes: int,
        memory_limit_bytes: int,
        temp_limit_bytes: int,
    ):
        self.base_dir = Path(base_dir).resolve()
        self.base_dir.mkdir(parents=True, exist_ok=True)
        self.run_dir = Path(tempfile.mkdtemp(prefix="settlement-spill-", dir=self.base_dir))
        self.threshold_bytes = max(1, int(threshold_bytes))
        self.memory_limit_bytes = max(1, int(memory_limit_bytes))
        self.temp_limit_bytes = max(1, int(temp_limit_bytes))
        self.total_bytes = 0
        self.sequence = 0
        self.files = 0
        self._closed = False

    def _next_path(self) -> Path:
        self.sequence += 1
        return self.run_dir / f"buffer-{self.sequence:06d}.jsonl"

    def maybe_spill(
        self,
        records: list[dict[str, Any]],
        *,
        batch_id: int | None,
        reason: str = "batch_preaggregation_buffer",
    ) -> SpillResult:
        if not records:
            return SpillResult(records=records, bytes_written=0, files=0, path=None)
        estimated = sum(len(canonical_json(record)) + 1 for record in records)
        if estimated <= self.threshold_bytes:
            return SpillResult(records=records, bytes_written=0, files=0, path=None)
        if estimated > self.memory_limit_bytes:
            # It can be written in chunks, so this is not an error by itself.
            pass
        path = self._next_path()
        written = 0
        try:
            with path.open("w", encoding="utf-8", newline="\n") as handle:
                for record in records:
                    line = canonical_json(record) + "\n"
                    encoded = line.encode("utf-8")
                    written += len(encoded)
                    if self.total_bytes + written > self.temp_limit_bytes:
                        raise MemoryLimitError(
                            "temporary spill space limit exceeded",
                            details={
                                "temp_limit_bytes": self.temp_limit_bytes,
                                "attempted_bytes": self.total_bytes + written,
                            },
                        )
                    handle.write(line)
        except OSError as exc:
            raise SettlementError(
                "E_IO", f"could not write spill buffer: {exc}", details={"path": str(path)}
            ) from exc
        self.total_bytes += written
        self.files += 1
        return SpillResult(
            records=self._read_records(path),
            bytes_written=written,
            files=1,
            path=path,
        )

    @staticmethod
    def _read_records(path: Path) -> Iterator[dict[str, Any]]:
        with path.open("r", encoding="utf-8") as handle:
            for line in handle:
                if line.strip():
                    yield json.loads(line)

    def cleanup(self) -> None:
        if self._closed:
            return
        self._closed = True
        shutil.rmtree(self.run_dir, ignore_errors=True)
        try:
            self.base_dir.rmdir()
        except OSError:
            pass

    def __enter__(self) -> "SpillManager":
        return self

    def __exit__(self, exc_type, exc, traceback) -> None:
        self.cleanup()


class BoundedFileWriter:
    """Simple temp file writer used to exercise stable disk spill accounting."""

    def __init__(self, manager: SpillManager):
        self.manager = manager
        self.path = manager._next_path()
        self.bytes = 0
        self.handle = self.path.open("wb")

    def write(self, payload: bytes) -> None:
        if self.manager.total_bytes + self.bytes + len(payload) > self.manager.temp_limit_bytes:
            raise MemoryLimitError("temporary spill space limit exceeded")
        self.handle.write(payload)
        self.bytes += len(payload)

    def close(self) -> None:
        self.handle.close()
        self.manager.total_bytes += self.bytes
        self.manager.files += 1

