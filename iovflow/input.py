from __future__ import annotations

import math
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterator

from .errors import EngineError
from .util import canonical_json, fingerprint, iter_jsonl, parse_time, read_json
from .util import stable_hash_hex


@dataclass
class BatchInfo:
    batch_id: int
    batch_no: int
    path: str
    declared_epoch: int
    rows: int = 0
    max_event_epoch: int = 0
    min_event_epoch: int = 0
    manifest: dict[str, Any] = field(default_factory=dict)


@dataclass
class InputDataset:
    root: str
    batches: list[BatchInfo]
    stats: dict[str, Any] = field(default_factory=dict)
    manifest_path: str | None = None

    @property
    def total_rows(self) -> int:
        return sum(b.rows for b in self.batches)

    def to_dict(self) -> dict[str, Any]:
        sig = stable_hash_hex(
            canonical_json(
                [
                    (
                        b.batch_id,
                        Path(b.path).name,
                        b.rows,
                        b.declared_epoch,
                        Path(b.path).stat().st_size if Path(b.path).exists() else 0,
                        Path(b.path).stat().st_mtime_ns if Path(b.path).exists() else 0,
                    )
                    for b in self.batches
                ]
            )
        )
        return {
            "root": self.root,
            "manifest": self.manifest_path,
            "batch_count": len(self.batches),
            "rows": self.total_rows,
            "stats": self.stats,
            "batch_signature": sig,
            "batches": [
                {
                    "batch_id": b.batch_id,
                    "batch_no": b.batch_no,
                    "file": Path(b.path).name,
                    "declared_event_time": b.declared_epoch,
                    "rows": b.rows,
                    "min_event_time": b.min_event_epoch,
                    "max_event_time": b.max_event_epoch,
                }
                for b in self.batches
            ],
        }


def load_input(input_dir: str | Path, vehicle_column: str) -> InputDataset:
    root = Path(input_dir)
    if not root.exists():
        raise EngineError("IOV_INPUT_INVALID", f"input directory does not exist: {root}")
    if root.is_file():
        files = [root]
        manifest: dict[str, Any] = {}
        manifest_path = None
    else:
        manifest_path = _find_manifest(root)
        manifest = read_json(manifest_path) if manifest_path else {}
        files = sorted(root.glob("*.jsonl"))
        if not files:
            files = sorted(root.glob("**/*.jsonl"))
    if not files:
        raise EngineError("IOV_INPUT_EMPTY", f"no .jsonl batches under {root}")

    entries = _manifest_entries(manifest)
    by_name: dict[str, dict[str, Any]] = {}
    for entry in entries:
        name = Path(str(entry.get("file") or entry.get("path") or "")).name
        if name:
            by_name[name] = entry

    ordered: list[tuple[int, Path, dict[str, Any]]] = []
    if entries:
        for i, entry in enumerate(entries):
            rel = str(entry.get("file") or entry.get("path") or "")
            p = (root / rel) if root.is_dir() else root
            if not p.exists():
                p = root / Path(rel).name
            if not p.exists():
                raise EngineError(
                    "IOV_INPUT_INVALID",
                    f"manifest references missing batch file {rel!r}",
                )
            ordered.append((int(entry.get("batch_no", i)), p, entry))
    else:
        ordered = [(i, p, {}) for i, p in enumerate(files)]

    batches: list[BatchInfo] = []
    for index, (batch_no, path, entry) in enumerate(ordered):
        declared = entry.get("declared_event_time") or entry.get("declared_time") or entry.get(
            "batch_time"
        )
        min_ts: int | None = None
        max_ts: int | None = None
        rows = 0
        first_row: dict[str, Any] | None = None
        for row in iter_jsonl(path):
            rows += 1
            if first_row is None:
                first_row = row
            ts = _row_time(row, vehicle_column)
            min_ts = ts if min_ts is None else min(min_ts, ts)
            max_ts = ts if max_ts is None else max(max_ts, ts)
        if rows == 0:
            raise EngineError("IOV_INPUT_EMPTY", f"batch file is empty: {path}")
        if declared is None:
            declared_epoch = max_ts if max_ts is not None else 0
        else:
            try:
                declared_epoch = parse_time(declared)
            except Exception as exc:
                raise EngineError(
                    "IOV_INPUT_INVALID",
                    f"invalid declared_event_time in {path}: {declared!r}",
                ) from exc
        batches.append(
            BatchInfo(
                batch_id=index,
                batch_no=batch_no,
                path=str(path),
                declared_epoch=declared_epoch,
                rows=rows,
                max_event_epoch=max_ts or 0,
                min_event_epoch=min_ts or 0,
                manifest=entry,
            )
        )
    batches.sort(key=lambda b: (b.batch_id, b.batch_no))
    stats = dict(manifest.get("stats") or {})
    stats.setdefault("rows_manifest", manifest.get("rows") or manifest.get("rows_total"))
    stats["batches"] = len(batches)
    stats["rows"] = sum(b.rows for b in batches)
    declared_rows = manifest.get("rows") or manifest.get("rows_total")
    if declared_rows is not None and int(declared_rows) != stats["rows"]:
        raise EngineError(
            "IOV_INPUT_INVALID",
            f"manifest declares {declared_rows} rows but files contain {stats['rows']}",
        )
    declared_batches = manifest.get("batches_count") or manifest.get("batch_count")
    if declared_batches is not None and int(declared_batches) != len(batches):
        raise EngineError(
            "IOV_INPUT_INVALID",
            f"manifest declares {declared_batches} batches but found {len(batches)}",
        )
    return InputDataset(
        root=str(root),
        batches=batches,
        stats=stats,
        manifest_path=str(manifest_path) if manifest_path else None,
    )


def scan_counts(
    dataset: InputDataset,
    vehicle_column: str,
    dedupe_keys: tuple[str, ...],
    window_start_fn,
    row_filter=None,
) -> dict[str, Any]:
    """Deduplicate the whole input and collect routing/window statistics.

    This pass is metadata-only; the execution stages still consume the files.
    It makes routing deterministic and independent of batch completion order.
    """
    seen: set[str] = set()
    vehicle_trips: dict[str, dict[str, int]] = {}
    vehicle_totals: dict[str, int] = {}
    windows: set[tuple[str, int]] = set()
    samples: list[tuple[str, str, str]] = []
    duplicate_rows = 0
    filtered_out_rows = 0
    invalid = 0
    total_unique = 0
    for batch in dataset.batches:
        for row in iter_jsonl(batch.path):
            if row_filter is not None:
                verdict = row_filter.eval(row)
                if verdict is not True:
                    filtered_out_rows += 1
                    continue
            vid = row.get(vehicle_column)
            if vid is None:
                invalid += 1
                raise EngineError(
                    "IOV_INPUT_INVALID",
                    f"event missing vehicle column {vehicle_column!r}",
                    fragment=canonical_json(row)[:300],
                )
            vid = str(vid)
            fp = fingerprint(row, dedupe_keys)
            if fp in seen:
                duplicate_rows += 1
                continue
            seen.add(fp)
            total_unique += 1
            trip = row.get("trip_id")
            trip = "" if trip is None else str(trip)
            vehicle_totals[vid] = vehicle_totals.get(vid, 0) + 1
            samples.append((vid, trip, fp))
            trips = vehicle_trips.setdefault(vid, {})
            trips[trip] = trips.get(trip, 0) + 1
            try:
                wstart = int(window_start_fn(row))
            except Exception as exc:
                raise EngineError(
                    "IOV_INPUT_INVALID",
                    f"cannot evaluate event-time window: {exc}",
                    fragment=canonical_json(row)[:300],
                ) from exc
            windows.add((vid, wstart))
    if total_unique == 0:
        raise EngineError("IOV_INPUT_EMPTY", "input contains no events after deduplication")
    return {
        "vehicle_trips": vehicle_trips,
        "vehicle_totals": vehicle_totals,
        "windows": windows,
        "samples": samples,
        "total_unique": total_unique,
        "duplicate_rows": duplicate_rows,
        "invalid_rows": invalid,
        "input_rows": dataset.total_rows,
        "filtered_out_rows": filtered_out_rows,
    }


def _find_manifest(root: Path) -> Path | None:
    for name in ("manifest.json", "batches.json", "input_manifest.json"):
        p = root / name
        if p.exists():
            return p
    return None


def _manifest_entries(manifest: dict[str, Any]) -> list[dict[str, Any]]:
    if isinstance(manifest, list):
        return [e for e in manifest if isinstance(e, dict)]
    for key in ("batches", "files", "items"):
        value = manifest.get(key)
        if isinstance(value, list):
            return [e for e in value if isinstance(e, dict)]
    return []


def _row_time(row: dict[str, Any], vehicle_column: str) -> int:
    for name in ("event_time", "event_ts", "timestamp", "ts"):
        if name in row:
            try:
                return parse_time(row[name])
            except Exception as exc:
                raise EngineError(
                    "IOV_INPUT_INVALID",
                    f"invalid event time {row[name]!r}",
                    fragment=canonical_json(row)[:300],
                ) from exc
    raise EngineError(
        "IOV_INPUT_INVALID",
        "event is missing event_time",
        fragment=canonical_json(row)[:300],
    )

