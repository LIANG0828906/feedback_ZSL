"""Partition scheduling, fault injection, and resource accounting."""

from __future__ import annotations

import json
import hashlib
import os
import shutil
import threading
from concurrent.futures import Future, ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

import pyarrow.parquet as pq

from .config import ExecutorConfig
from .errors import (
    ERROR_IO,
    ERROR_MEMORY_LIMIT,
    ERROR_RETRY_EXHAUSTED,
    ERROR_TEMP_LIMIT,
    ExecutorError,
)


class InjectedFault(RuntimeError):
    """Raised exactly once for a configured execution partition/batch."""


def directory_size(path: Path) -> int:
    if not path.exists():
        return 0
    total = 0
    for root, _, files in os.walk(path):
        for name in files:
            try:
                total += (Path(root) / name).stat().st_size
            except OSError:
                pass
    return total


@dataclass
class TaskResult:
    output_paths: tuple[Path, ...]
    rows: int
    metrics: list[dict[str, Any]] = field(default_factory=list)


class MemoryGovernor:
    def __init__(self, limit_bytes: int):
        self.limit_bytes = limit_bytes
        self.current_bytes = 0
        self.peak_bytes = 0
        self._lock = threading.Lock()

    def reserve(self, amount: int) -> None:
        with self._lock:
            if self.current_bytes + amount > self.limit_bytes:
                raise ExecutorError(
                    ERROR_MEMORY_LIMIT,
                    "estimated concurrent memory would exceed memory_limit_mb",
                    {
                        "current_bytes": self.current_bytes,
                        "requested_bytes": amount,
                        "limit_bytes": self.limit_bytes,
                    },
                )
            self.current_bytes += amount
            self.peak_bytes = max(self.peak_bytes, self.current_bytes)

    def release(self, amount: int) -> None:
        with self._lock:
            self.current_bytes = max(0, self.current_bytes - amount)


class TempQuota:
    def __init__(self, temp_dir: Path, limit_bytes: int):
        self.temp_dir = temp_dir
        self.limit_bytes = limit_bytes
        self._lock = threading.Lock()
        self.peak_bytes = 0

    def check(self, extra_bytes: int = 0) -> None:
        with self._lock:
            used = directory_size(self.temp_dir)
            total = used + extra_bytes
            self.peak_bytes = max(self.peak_bytes, total)
            if total > self.limit_bytes:
                raise ExecutorError(
                    ERROR_TEMP_LIMIT,
                    "temporary data would exceed temp_limit_mb",
                    {
                        "used_bytes": used,
                        "requested_bytes": extra_bytes,
                        "limit_bytes": self.limit_bytes,
                    },
                )


class FaultController:
    """Persistently fires a configured partition/batch fault only once."""

    def __init__(
        self,
        config: ExecutorConfig,
        state: "StateStore",
    ):
        raw = config.fault_injection or {}
        self.partition_id = str(raw.get("partition_id", "")) if raw else ""
        self.batch = int(raw.get("batch", 0)) if raw else 0
        self.state = state
        self._lock = threading.Lock()

    @property
    def enabled(self) -> bool:
        return bool(self.partition_id)

    def check(self, stage_id: str, partition: int, batch: int) -> None:
        if not self.enabled:
            return
        partition_id = make_partition_id(stage_id, partition)
        matches_partition = (
            self.partition_id == partition_id
            or self.partition_id == f"p{partition:04d}"
            or self.partition_id == stage_id
        )
        if not matches_partition or batch != self.batch:
            return
        key = f"{partition_id}:batch{batch}"
        with self._lock:
            if key in self.state.fault_fired:
                return
            self.state.fault_fired.add(key)
            self.state.save()
        raise InjectedFault(
            f"injected fault for {partition_id} at batch {batch}"
        )


def make_partition_id(stage_id: str, partition: int) -> str:
    return f"{stage_id}:p{partition:04d}"


class StateStore:
    """A crash-visible checkpoint manifest for completed partitions."""

    STATE_FORMAT_VERSION = 2

    def __init__(self, work_dir: Path, resume: bool = False):
        self.work_dir = work_dir
        self.path = work_dir / "state.json"
        self.completed: dict[str, dict[str, dict[str, Any]]] = {}
        self.fault_fired: set[str] = set()
        self.retry_partitions: dict[str, dict[str, Any]] = {}
        self.signature: str | None = None
        self._lock = threading.Lock()
        if resume and self.path.exists():
            self._load()

    def _load(self) -> None:
        try:
            data = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return
        if data.get("state_format_version") != self.STATE_FORMAT_VERSION:
            self.completed = {}
            self.fault_fired = set()
            self.retry_partitions = {}
            return
        self.completed = data.get("completed", {})
        self.fault_fired = set(data.get("fault_fired", []))
        self.retry_partitions = data.get("retry_partitions", {})
        self.signature = data.get("signature")

    def configure_signature(self, signature: str) -> None:
        """Invalidate checkpoints produced by different code/input/config."""
        with self._lock:
            if self.signature != signature:
                self.completed = {}
                self.fault_fired = set()
                self.retry_partitions = {}
                self.signature = signature
        self.save()

    def save(self) -> None:
        with self._lock:
            payload = {
                "state_format_version": self.STATE_FORMAT_VERSION,
                "signature": self.signature,
                "completed": self.completed,
                "fault_fired": sorted(self.fault_fired),
                "retry_partitions": self.retry_partitions,
            }
            tmp = self.path.with_suffix(".json.tmp")
            tmp.write_text(
                json.dumps(payload, sort_keys=True, indent=2) + "\n",
                encoding="utf-8",
            )
            os.replace(tmp, self.path)

    def get_completed(self, stage_id: str, partition: int) -> dict[str, Any] | None:
        with self._lock:
            return self.completed.get(stage_id, {}).get(str(partition))

    def mark_completed(
        self, stage_id: str, partition: int, paths: list[Path], rows: int
    ) -> None:
        with self._lock:
            stage = self.completed.setdefault(stage_id, {})
            stored_paths: list[str] = []
            for path in paths:
                try:
                    stored_paths.append(str(path.resolve().relative_to(self.work_dir)))
                except ValueError:
                    stored_paths.append(str(path.resolve()))
            stage[str(partition)] = {
                "paths": stored_paths,
                "rows": rows,
            }
        self.save()

    def record_retry(self, partition_id: str, attempts: int, reason: str) -> None:
        with self._lock:
            self.retry_partitions[partition_id] = {
                "attempts": attempts,
                "reason": reason,
            }
        self.save()

    def completed_partition_ids(self) -> list[str]:
        out: list[str] = []
        for stage_id, partitions in self.completed.items():
            for partition in partitions:
                out.append(make_partition_id(stage_id, int(partition)))
        return sorted(out)

    def retry_list(self) -> list[dict[str, Any]]:
        return [
            {"partition_id": key, **value}
            for key, value in sorted(self.retry_partitions.items())
        ]

    def completed_paths(self, stage_id: str, partition: int) -> list[Path]:
        entry = self.get_completed(stage_id, partition)
        if not entry:
            return []
        out: list[Path] = []
        for raw in entry.get("paths", []):
            path = Path(raw)
            if not path.is_absolute():
                path = self.work_dir / path
            out.append(path)
        return out


@dataclass
class StageOutcome:
    partitions: list[TaskResult]
    max_active: int


class PartitionScheduler:
    def __init__(
        self,
        config: ExecutorConfig,
        state: StateStore,
        fault: FaultController,
        memory: MemoryGovernor,
        temp: TempQuota,
    ):
        self.config = config
        self.state = state
        self.fault = fault
        self.memory = memory
        self.temp = temp
        self._active = 0
        self._max_active = 0
        self._active_lock = threading.Lock()

    def run_stage(
        self,
        stage_id: str,
        partition_count: int,
        task: Callable[[int], TaskResult],
    ) -> StageOutcome:
        pending = list(range(partition_count))
        results: dict[int, TaskResult] = {}
        attempts: dict[int, int] = {p: 0 for p in pending}
        last_errors: dict[int, str] = {}

        # A resumed job reuses completed partition outputs without recomputation.
        completed_now: list[int] = []
        for partition in list(pending):
            if self.state.get_completed(stage_id, partition):
                paths = self.state.completed_paths(stage_id, partition)
                rows = int(
                    self.state.get_completed(stage_id, partition).get("rows", 0)
                )
                results[partition] = TaskResult(tuple(paths), rows, [])
                completed_now.append(partition)
                pending.remove(partition)
        if completed_now:
            self.state.save()

        while pending:
            failures: list[tuple[int, BaseException]] = []
            with ThreadPoolExecutor(
                max_workers=self.config.max_workers,
                thread_name_prefix=f"exec-{stage_id}",
            ) as pool:
                futures: dict[Future[TaskResult], int] = {}
                for partition in pending:
                    f = pool.submit(self._run_one, task, partition)
                    futures[f] = partition
                for future in as_completed(futures):
                    partition = futures[future]
                    try:
                        result = future.result()
                    except BaseException as exc:  # keep business failures stable
                        text = str(exc).lower()
                        if "out of memory" in text or exc.__class__.__name__ in {
                            "OutOfMemoryException",
                            "OutOfMemoryError",
                        }:
                            exc = ExecutorError(
                                ERROR_MEMORY_LIMIT,
                                f"vectorized operator ran out of memory: {exc}",
                            )
                        elif "no space left" in text or "disk full" in text:
                            exc = ExecutorError(
                                ERROR_TEMP_LIMIT,
                                f"temporary storage exhausted: {exc}",
                            )
                        failures.append((partition, exc))
                        continue
                    results[partition] = result
                    self.state.mark_completed(
                        stage_id, partition, list(result.output_paths), result.rows
                    )
            if not failures:
                break
            pending = []
            for partition, exc in sorted(failures, key=lambda item: item[0]):
                attempts[partition] += 1
                last_errors[partition] = f"{type(exc).__name__}: {exc}"
                if isinstance(exc, ExecutorError) and exc.code in {
                    ERROR_MEMORY_LIMIT,
                    ERROR_TEMP_LIMIT,
                    ERROR_IO,
                }:
                    raise exc
                if attempts[partition] > self.config.max_retries:
                    partition_id = make_partition_id(stage_id, partition)
                    self.state.record_retry(
                        partition_id, attempts[partition], last_errors[partition]
                    )
                    raise ExecutorError(
                        ERROR_RETRY_EXHAUSTED,
                        f"partition {partition_id} still failing after retries",
                        {
                            "partition_id": partition_id,
                            "attempts": attempts[partition],
                            "last_error": last_errors[partition],
                            "completed_partitions": self.state.completed_partition_ids(),
                        },
                    )
                pending.append(partition)
                self.state.record_retry(
                    make_partition_id(stage_id, partition),
                    attempts[partition],
                    last_errors[partition],
                )
            # partitions that completed in this wave must not be retried
            pending = sorted(set(pending))

        ordered = [results[p] for p in sorted(results)]
        return StageOutcome(ordered, self._max_active)

    def _run_one(
        self, task: Callable[[int], TaskResult], partition: int
    ) -> TaskResult:
        with self._active_lock:
            self._active += 1
            self._max_active = max(self._max_active, self._active)
        try:
            return task(partition)
        finally:
            with self._active_lock:
                self._active -= 1


class JobWorkspace:
    def __init__(
        self,
        temp_root: str | Path,
        run_id: str,
        resume: bool = False,
    ):
        safe_id = "".join(
            ch if ch.isalnum() or ch in {"-", "_"} else "_" for ch in run_id
        )[:80]
        root = Path(temp_root).resolve() / safe_id
        if root.exists() and not resume:
            shutil.rmtree(root)
        self.root = root
        self.cache_dir = root / "cache"
        self.prepared_dir = root / "prepared"
        self.stage_dir = root / "stages"
        self.repart_dir = root / "repartition"
        self.spill_dir = root / "spill"
        self.duckdb_spill_dir = root / "duckdb_spill"
        self.report_dir = root / "reports"
        for path in (
            self.cache_dir,
            self.prepared_dir,
            self.stage_dir,
            self.repart_dir,
            self.spill_dir,
            self.duckdb_spill_dir,
            self.report_dir,
        ):
            path.mkdir(parents=True, exist_ok=True)
        if resume:
            # These directories are scratch for in-flight work only. Completed
            # partition outputs and the checkpoint manifest are kept.
            for scratch in (self.repart_dir, self.duckdb_spill_dir):
                shutil.rmtree(scratch, ignore_errors=True)
                scratch.mkdir(parents=True, exist_ok=True)
        self.state = StateStore(root, resume=resume)
        self._spill_lock = threading.Lock()
        self.spill_bytes = directory_size(self.spill_dir) if resume else 0
        self._initial_duckdb_spill = 0

    def record_spill_bytes(self, amount: int) -> None:
        with self._spill_lock:
            self.spill_bytes += max(0, int(amount))

    def duckdb_spill_bytes(self) -> int:
        return directory_size(self.duckdb_spill_dir)

    def cleanup_after_success(self) -> None:
        # The completed manifest is copied to the stable output report before
        # this method is called by the CLI.
        shutil.rmtree(self.root, ignore_errors=True)

    def write_failure_manifest(self, output_dir: Path, payload: dict[str, Any]) -> Path:
        output_dir.mkdir(parents=True, exist_ok=True)
        path = output_dir / "failure_manifest.json"
        tmp = path.with_suffix(".json.tmp")
        tmp.write_text(
            json.dumps(payload, sort_keys=True, indent=2) + "\n", encoding="utf-8"
        )
        os.replace(tmp, path)
        return path


def parquet_rows(path: Path) -> int:
    try:
        return int(pq.ParquetFile(path).metadata.num_rows)
    except Exception as exc:
        raise ExecutorError(ERROR_IO, f"cannot read parquet metadata {path}: {exc}") from exc


def state_signature(query_text: str, schema_text: str, config: ExecutorConfig) -> str:
    payload = {
        "query": query_text,
        "schema": schema_text,
        "batch_rows": config.batch_rows,
        "partitions": config.partitions,
        "max_workers": config.max_workers,
        "memory_limit_mb": config.memory_limit_mb,
        "agg_memory_threshold_mb": config.agg_memory_threshold_mb,
        "temp_limit_mb": config.temp_limit_mb,
        "duckdb_memory_limit_mb": config.duckdb_memory_limit_mb,
        "stream_batch_rows": config.stream_batch_rows,
        "stream_mode": config.stream_mode,
        "allowed_lateness_seconds": config.allowed_lateness_seconds,
        "result_format": config.result_format,
        "state_format_version": StateStore.STATE_FORMAT_VERSION,
    }
    return hashlib.sha256(
        json.dumps(payload, sort_keys=True).encode("utf-8")
    ).hexdigest()


def scan_for_fault(
    sources: list[Path],
    stage_id: str,
    partition: int,
    fault: FaultController,
    memory: MemoryGovernor,
    batch_rows: int,
) -> int:
    """Read batches without materializing them and inject at the exact batch."""
    total = 0
    batch_index = 0
    for source in sorted(set(sources)):
        try:
            parquet_file = pq.ParquetFile(source)
        except Exception as exc:
            raise ExecutorError(
                ERROR_IO, f"cannot open partition input {source}: {exc}"
            ) from exc
        for batch in parquet_file.iter_batches(batch_size=batch_rows):
            fault.check(stage_id, partition, batch_index)
            memory.reserve(batch.nbytes)
            try:
                total += batch.num_rows
            finally:
                memory.release(batch.nbytes)
            batch_index += 1
    return total

