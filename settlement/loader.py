from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Any, Iterator

from .errors import DataError
from .sql import SettlementQuery
from .timeutil import (
    date_partition,
    datetime_to_us,
    fixed_from_number,
    floor_us,
    parse_datetime,
    window_end_us,
)
from .util import sha256_file


@dataclass(frozen=True)
class DatasetManifest:
    root: Path
    path: Path
    raw: dict[str, Any]
    sha256: str

    @property
    def telemetry_rows(self) -> int:
        return int(self.raw["telemetry_rows"])

    @property
    def supplement_rows(self) -> int:
        return int(self.raw["supplement_rows"])

    @property
    def batch_count(self) -> int:
        return int(self.raw["batch_count"])

    @property
    def telemetry_partitions(self) -> list[dict[str, Any]]:
        return list(self.raw["telemetry_partitions"])

    @property
    def supplement_batches(self) -> list[dict[str, Any]]:
        return list(self.raw["supplement_batches"])


def load_manifest(data_dir: str | Path) -> DatasetManifest:
    root = Path(data_dir).resolve()
    path = root / "manifest.json"
    if not path.exists():
        raise DataError(
            f"manifest not found under {root}; run the generate command first",
            details={"path": str(path)},
        )
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except Exception as exc:
        raise DataError(f"invalid dataset manifest: {exc}", details={"path": str(path)}) from exc
    if raw.get("format_version") != 1:
        raise DataError("unsupported dataset manifest format_version")
    for key in ("telemetry_partitions", "supplement_batches"):
        if not isinstance(raw.get(key), list) or not raw[key]:
            raise DataError(f"dataset manifest has no {key}")
    for item in raw["telemetry_partitions"] + raw["supplement_batches"]:
        target = root / item["path"]
        if not target.exists():
            raise DataError(f"input file listed in manifest is missing: {target}")
    return DatasetManifest(root=root, path=path, raw=raw, sha256=sha256_file(path))


def _normalize_common(
    *,
    vehicle_id: str,
    trip_id: str,
    event_time_us: int,
    mileage_delta: Any,
    energy_delta: Any,
    source_type: str,
    source_id: str,
    query: SettlementQuery,
    batch_id: int | None = None,
) -> dict[str, Any]:
    window_start = floor_us(event_time_us, query.window_seconds)
    return {
        "vehicle_id": str(vehicle_id),
        "trip_id": str(trip_id),
        "event_time_us": int(event_time_us),
        "partition_date": date_partition(event_time_us),
        "window_start_us": window_start,
        "window_end_us": window_end_us(window_start, query.window_seconds),
        "mileage_u": fixed_from_number(mileage_delta),
        "energy_u": fixed_from_number(energy_delta),
        "source_type": source_type,
        "source_id": source_id,
        "batch_id": batch_id,
    }


def read_telemetry(
    manifest: DatasetManifest,
    query: SettlementQuery,
    *,
    pushdown: bool,
    columns: list[str] | None = None,
) -> list[dict[str, Any]]:
    import pyarrow.dataset as ds

    root = manifest.root / "telemetry"
    try:
        dataset = ds.dataset(str(root), format="parquet", partitioning="hive")
    except Exception as exc:
        raise DataError(f"could not open telemetry Parquet dataset: {exc}") from exc
    selected = columns or [
        "vehicle_id",
        "trip_id",
        "event_time",
        "mileage_delta",
        "energy_delta",
        "event_id",
        "partition_date",
    ]
    filter_expression = query.scan_filter() if pushdown else None
    try:
        table = dataset.to_table(columns=selected, filter=filter_expression)
    except Exception as exc:
        raise DataError(f"failed to scan telemetry Parquet data: {exc}") from exc
    rows = table.to_pylist()
    result: list[dict[str, Any]] = []
    for row in rows:
        event_time = row["event_time"]
        if isinstance(event_time, datetime):
            event_us = datetime_to_us(event_time)
        else:
            raise DataError(f"invalid telemetry event_time value: {event_time!r}")
        partition = row.get("partition_date")
        if partition is None:
            partition = date_partition(event_us)
        event = _normalize_common(
            vehicle_id=row["vehicle_id"],
            trip_id=row["trip_id"],
            event_time_us=event_us,
            mileage_delta=row["mileage_delta"],
            energy_delta=row["energy_delta"],
            source_type="telemetry",
            source_id=str(
                row.get("event_id")
                or (
                    f"T:{event_us}:{row['vehicle_id']}:{row['trip_id']}:"
                    f"{row.get('mileage_delta')}:{row.get('energy_delta')}"
                )
            ),
            query=query,
        )
        event["partition_date"] = str(partition)
        result.append(event)
    return result


def read_batch(
    manifest: DatasetManifest,
    batch: dict[str, Any],
    query: SettlementQuery,
) -> tuple[list[dict[str, Any]], dict[str, Any]]:
    path = manifest.root / batch["path"]
    events: list[dict[str, Any]] = []
    malformed = 0
    try:
        with path.open("r", encoding="utf-8") as handle:
            for line_number, line in enumerate(handle, start=1):
                if not line.strip():
                    continue
                try:
                    row = json.loads(line)
                    event_time = parse_datetime(row["event_time"])
                    source_id = str(
                        row.get("record_id")
                        or (
                            f"C:{event_time.isoformat()}:{row.get('vehicle_id')}:"
                            f"{row.get('trip_id')}:{row.get('mileage_delta')}:"
                            f"{row.get('energy_delta')}"
                        )
                    )
                    event = _normalize_common(
                        vehicle_id=row["vehicle_id"],
                        trip_id=row["trip_id"],
                        event_time_us=datetime_to_us(event_time),
                        mileage_delta=row["mileage_delta"],
                        energy_delta=row["energy_delta"],
                        source_type="charging_supplement",
                        source_id=source_id,
                        query=query,
                        batch_id=int(batch["batch_id"]),
                    )
                    events.append(event)
                except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
                    malformed += 1
                    raise DataError(
                        f"invalid supplement event at {path}:{line_number}: {exc}",
                        details={"batch_id": batch.get("batch_id"), "line": line_number},
                    ) from exc
    except OSError as exc:
        raise DataError(f"could not read supplement batch {path}: {exc}") from exc
    metadata = {
        "batch_id": int(batch["batch_id"]),
        "available_at": str(batch["available_at"]),
        "available_at_us": datetime_to_us(parse_datetime(batch["available_at"])),
        "rows": len(events),
        "malformed": malformed,
        "path": str(batch["path"]),
    }
    return events, metadata


def iter_batches(manifest: DatasetManifest) -> Iterator[dict[str, Any]]:
    for batch in sorted(manifest.supplement_batches, key=lambda item: int(item["batch_id"])):
        yield batch


def manifest_telemetry_file_rows(manifest: DatasetManifest) -> int:
    return sum(int(item["rows"]) for item in manifest.telemetry_partitions)


def manifest_batch_rows(manifest: DatasetManifest) -> int:
    return sum(int(item["rows"]) for item in manifest.supplement_batches)

