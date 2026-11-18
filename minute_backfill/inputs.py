"""Input catalog, manifest checks, and strict Parquet/JSONL readers."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from decimal import Decimal
from pathlib import Path
from typing import Any, Iterator

import duckdb
import pyarrow.parquet as pq

from .errors import EngineError, input_error
from .manifest import SchemaManifest, TableSpec, convert_row, validate_arrow_schema
from .timeutil import parse_time_us
from .util import load_json_strict, reject_duplicate_pairs, sha256_file


@dataclass(frozen=True)
class PaymentShard:
    shard_id: str
    path: Path
    declared_event_time_us: int | None
    rows: int | None
    sha256: str | None
    partitions: tuple[str, ...] = ()


@dataclass(frozen=True)
class RefundBatch:
    batch_id: str
    batch_index: int
    path: Path
    declared_event_time_us: int
    rows: int | None
    sha256: str | None


@dataclass
class InputCatalog:
    data_dir: Path
    manifest_path: Path
    manifest: SchemaManifest
    shards: list[PaymentShard]
    batches: list[RefundBatch]
    warnings: list[str] = field(default_factory=list)
    schema_sha256: str = ""
    shard_manifest_sha256: str = ""
    batch_manifest_sha256: str = ""

    @classmethod
    def load(cls, data_dir: Path) -> "InputCatalog":
        data_dir = data_dir.resolve()
        manifest_path = data_dir / "manifest.json"
        from .manifest import load_manifest

        manifest = load_manifest(manifest_path)
        warnings: list[str] = []
        payments_raw = manifest.raw["payments"]
        refunds_raw = manifest.raw["refunds"]
        shard_manifest_rel = payments_raw.get("manifest", "payments/shards.json")
        batch_manifest_rel = refunds_raw.get("manifest", "refunds/batches.json")
        shard_manifest_path = _safe_child(data_dir, shard_manifest_rel, "payments shard manifest")
        batch_manifest_path = _safe_child(data_dir, batch_manifest_rel, "refund batch manifest")

        if shard_manifest_path.exists():
            shards_raw = _load_manifest_list(
                shard_manifest_path, "shards", [shard_manifest_path.name]
            )
            shards = _parse_shards(data_dir, shards_raw, shard_manifest_path)
            shard_sha = sha256_file(shard_manifest_path)
        else:
            shards = _discover_shards(data_dir)
            if not shards:
                raise input_error(
                    f"no payment parquet shards found under {data_dir / 'payments'}",
                    str(shard_manifest_path),
                )
            warnings.append(
                "payments/shards.json is absent; shards were discovered by lexical path order"
            )
            shard_sha = ""

        if batch_manifest_path.exists():
            batches_raw = _load_manifest_list(
                batch_manifest_path, "batches", [batch_manifest_path.name]
            )
            batches = _parse_batches(data_dir, batches_raw, batch_manifest_path)
            batch_sha = sha256_file(batch_manifest_path)
        else:
            batches = _discover_batches(data_dir, warnings)
            if not batches:
                raise input_error(
                    f"no refund JSONL batches found under {data_dir / 'refunds'}",
                    str(batch_manifest_path),
                )
            batch_sha = ""

        _check_sequence_gaps(batches, warnings)
        return cls(
            data_dir=data_dir,
            manifest_path=manifest_path,
            manifest=manifest,
            shards=shards,
            batches=batches,
            warnings=warnings,
            schema_sha256=sha256_file(manifest_path),
            shard_manifest_sha256=shard_sha,
            batch_manifest_sha256=batch_sha,
        )

    def shard_by_id(self) -> dict[str, PaymentShard]:
        return {item.shard_id: item for item in self.shards}

    def batch_by_id(self) -> dict[str, RefundBatch]:
        return {item.batch_id: item for item in self.batches}


def _safe_child(root: Path, rel: str, description: str) -> Path:
    if not isinstance(rel, str) or not rel:
        raise input_error(f"{description} path is empty", "manifest")
    candidate = (root / rel).resolve()
    try:
        candidate.relative_to(root)
    except ValueError as exc:
        raise input_error(f"{description} escapes the data directory: {rel}", "manifest") from exc
    return candidate


def _load_manifest_list(path: Path, key: str, subject_list: list[str]) -> list[dict[str, Any]]:
    subject = subject_list[0]
    try:
        raw = load_json_strict(path)
    except FileNotFoundError as exc:
        raise input_error(f"{subject}: manifest file not found", subject) from exc
    except Exception as exc:
        raise input_error(f"{subject}: manifest cannot be parsed: {exc}", subject) from exc
    if not isinstance(raw, dict):
        raise input_error(f"{subject}: manifest root must be an object", subject)
    items = raw.get(key)
    if not isinstance(items, list) or not items:
        raise input_error(f"{subject}: manifest.{key} must be a non-empty array", subject)
    result: list[dict[str, Any]] = []
    for index, item in enumerate(items):
        if not isinstance(item, dict):
            raise input_error(f"{subject}: manifest.{key}[{index}] must be an object", subject)
        result.append(item)
    return result


def _optional_int(value: Any, *, subject: str, field_name: str) -> int | None:
    if value is None:
        return None
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise input_error(
            f"{subject}: {field_name} must be a non-negative integer", subject
        )
    return value


def _optional_sha(value: Any, *, subject: str) -> str | None:
    if value is None:
        return None
    if not isinstance(value, str) or len(value) != 64:
        raise input_error(f"{subject}: sha256 must be a 64-character hex string", subject)
    try:
        int(value, 16)
    except ValueError as exc:
        raise input_error(f"{subject}: sha256 is not hexadecimal", subject) from exc
    return value.lower()


def _parse_shards(
    data_dir: Path, items: list[dict[str, Any]], manifest_path: Path
) -> list[PaymentShard]:
    result: list[PaymentShard] = []
    seen: set[str] = set()
    for index, item in enumerate(items):
        file_value = item.get("file")
        if not isinstance(file_value, str) or not file_value:
            raise input_error(
                f"{manifest_path.name}: shards[{index}].file must be a path",
                manifest_path.name,
            )
        path = _safe_child(data_dir, file_value, "payment shard")
        shard_id = item.get("shard_id")
        if shard_id is None:
            shard_id = path.relative_to(data_dir).as_posix()
        if not isinstance(shard_id, str) or not shard_id:
            raise input_error(
                f"{manifest_path.name}: shards[{index}].shard_id must be a string",
                manifest_path.name,
            )
        if shard_id in seen:
            raise input_error(
                f"{manifest_path.name}: duplicate shard_id {shard_id!r}", shard_id
            )
        seen.add(shard_id)
        if not path.is_file():
            raise input_error(f"{shard_id}: payment shard file not found: {path}", shard_id)
        declared_raw = item.get("declared_event_time")
        declared_us: int | None = None
        if declared_raw is not None:
            try:
                declared_us = parse_time_us(declared_raw)
            except ValueError as exc:
                raise input_error(f"{shard_id}: invalid declared_event_time: {exc}", shard_id) from exc
        partitions = tuple(
            sorted(part.name for part in path.parents if part.name.startswith("day="))
        )
        result.append(
            PaymentShard(
                shard_id=shard_id,
                path=path,
                declared_event_time_us=declared_us,
                rows=_optional_int(item.get("rows"), subject=shard_id, field_name="rows"),
                sha256=_optional_sha(item.get("sha256"), subject=shard_id),
                partitions=partitions,
            )
        )
    return result


def _discover_shards(data_dir: Path) -> list[PaymentShard]:
    root = data_dir / "payments"
    if not root.is_dir():
        return []
    files = sorted(root.glob("day=*/*.parquet"), key=lambda path: path.as_posix())
    result: list[PaymentShard] = []
    for path in files:
        result.append(
            PaymentShard(
                shard_id=path.relative_to(data_dir).as_posix(),
                path=path,
                declared_event_time_us=None,
                rows=None,
                sha256=None,
            )
        )
    return result


def _parse_batches(
    data_dir: Path, items: list[dict[str, Any]], manifest_path: Path
) -> list[RefundBatch]:
    result: list[RefundBatch] = []
    seen: set[str] = set()
    seen_indexes: set[int] = set()
    for position, item in enumerate(items):
        file_value = item.get("file")
        if not isinstance(file_value, str) or not file_value:
            raise input_error(
                f"{manifest_path.name}: batches[{position}].file must be a path",
                manifest_path.name,
            )
        path = _safe_child(data_dir, file_value, "refund batch")
        batch_id = item.get("batch_id")
        if not isinstance(batch_id, str) or not batch_id:
            raise input_error(
                f"{manifest_path.name}: batches[{position}].batch_id must be a string",
                manifest_path.name,
            )
        if batch_id in seen:
            raise input_error(
                f"{manifest_path.name}: duplicate batch_id {batch_id!r}", batch_id
            )
        seen.add(batch_id)
        if not path.is_file():
            raise input_error(f"{batch_id}: refund batch file not found: {path}", batch_id)
        declared_raw = item.get("declared_event_time")
        if declared_raw is None:
            raise input_error(
                f"{batch_id}: declared_event_time is required", batch_id
            )
        try:
            declared_us = parse_time_us(declared_raw, field_name="declared_event_time")
        except ValueError as exc:
            raise input_error(f"{batch_id}: invalid declared_event_time: {exc}", batch_id) from exc
        batch_index = item.get("batch_index", position + 1)
        if isinstance(batch_index, bool) or not isinstance(batch_index, int) or batch_index < 1:
            raise input_error(f"{batch_id}: batch_index must be >= 1", batch_id)
        if batch_index in seen_indexes:
            raise input_error(
                f"{batch_id}: duplicate batch_index {batch_index}", batch_id
            )
        seen_indexes.add(batch_index)
        result.append(
            RefundBatch(
                batch_id=batch_id,
                batch_index=batch_index,
                path=path,
                declared_event_time_us=declared_us,
                rows=_optional_int(item.get("rows"), subject=batch_id, field_name="rows"),
                sha256=_optional_sha(item.get("sha256"), subject=batch_id),
            )
        )
    result.sort(key=lambda item: (item.batch_index, item.batch_id))
    return result


def _discover_batches(data_dir: Path, warnings: list[str]) -> list[RefundBatch]:
    root = data_dir / "refunds"
    if not root.is_dir():
        return []
    files = sorted(root.glob("batch-*.jsonl"), key=lambda path: path.as_posix())
    result: list[RefundBatch] = []
    for position, path in enumerate(files, start=1):
        # Without a batch manifest there is no declared event time.  We refuse to
        # invent watermark semantics silently.
        warnings.append(f"refund batch manifest absent; {path.name} has no declared event time")
    if files:
        raise input_error(
            "refunds/batches.json is required because watermark semantics depend on "
            "each batch's declared_event_time",
            "refunds/batches.json",
        )
    return result


def _check_sequence_gaps(batches: list[RefundBatch], warnings: list[str]) -> None:
    previous: int | None = None
    previous_declared: int | None = None
    for batch in batches:
        if previous is not None and batch.batch_index > previous + 1:
            missing = list(range(previous + 1, batch.batch_index))
            warnings.append(
                f"batch_index gap before {batch.batch_id}: missing {missing}; "
                "watermark will not move backwards"
            )
        if previous_declared is not None and batch.declared_event_time_us < previous_declared:
            warnings.append(
                f"batch {batch.batch_id} declares an earlier event time than its predecessor; "
                "watermark will use max()"
            )
        previous = batch.batch_index
        previous_declared = max(previous_declared or batch.declared_event_time_us,
                                batch.declared_event_time_us)


def _verify_file(path: Path, expected_sha: str | None, subject: str) -> None:
    if expected_sha is None:
        return
    actual = sha256_file(path)
    if actual != expected_sha:
        raise input_error(
            f"{subject}: file sha256 mismatch (expected {expected_sha}, got {actual})",
            subject,
        )


def _parquet_row_count(path: Path, subject: str) -> int:
    try:
        con = duckdb.connect(config={"threads": 1, "memory_limit": "256MB"})
        try:
            row = con.execute(
                "SELECT count(*) FROM read_parquet(?)", [str(path)]
            ).fetchone()
        finally:
            con.close()
    except Exception as exc:
        raise input_error(f"{subject}: parquet is unreadable: {exc}", subject) from exc
    return int(row[0])


def read_payment_shard(
    shard: PaymentShard,
    table: TableSpec,
    *,
    chunk_rows: int = 50_000,
) -> Iterator[list[dict[str, Any]]]:
    _verify_file(shard.path, shard.sha256, shard.shard_id)
    try:
        parquet_file = pq.ParquetFile(shard.path)
        validate_arrow_schema(parquet_file.schema_arrow, table, subject=shard.shard_id)
    except EngineError:
        raise
    except Exception as exc:
        raise input_error(
            f"{shard.shard_id}: parquet schema cannot be read: {exc}", shard.shard_id
        ) from exc
    actual_rows = _parquet_row_count(shard.path, shard.shard_id)
    if shard.rows is not None and actual_rows != shard.rows:
        raise input_error(
            f"{shard.shard_id}: declared rows={shard.rows}, parquet rows={actual_rows}",
            shard.shard_id,
        )
    row_number = 0
    try:
        for record_batch in parquet_file.iter_batches(batch_size=chunk_rows, use_threads=False):
            rows = record_batch.to_pylist()
            converted: list[dict[str, Any]] = []
            for row in rows:
                row_number += 1
                normalized = convert_row(
                    row, table, subject=shard.shard_id, row_number=row_number
                )
                converted.append(
                    {
                        "side": "PAYMENT",
                        "payment_id": normalized["payment_id"],
                        "merchant_id": normalized["merchant_id"],
                        "event_time": normalized["event_time"],
                        "amount_minor": normalized["amount"],
                        "currency": normalized["currency"],
                        "event_type": normalized["event_type"],
                        "event_id": normalized["payment_id"],
                    }
                )
            if converted:
                yield converted
    except EngineError:
        raise
    except Exception as exc:
        raise input_error(
            f"{shard.shard_id}: parquet row parsing failed near row {row_number}: {exc}",
            shard.shard_id,
            row=row_number,
        ) from exc
    if row_number != actual_rows:
        raise input_error(
            f"{shard.shard_id}: only parsed {row_number} of {actual_rows} rows",
            shard.shard_id,
        )


def _json_bad_constant(value: str) -> None:
    raise ValueError(f"non-finite JSON number is not allowed: {value}")


def read_refund_batch(
    batch: RefundBatch,
    table: TableSpec,
    *,
    max_rows: int | None = None,
) -> list[dict[str, Any]]:
    _verify_file(batch.path, batch.sha256, batch.batch_id)
    result: list[dict[str, Any]] = []
    try:
        with batch.path.open("r", encoding="utf-8-sig", newline="") as fh:
            for line_number, line in enumerate(fh, start=1):
                if not line.strip():
                    continue
                try:
                    raw = json.loads(
                        line,
                        object_pairs_hook=reject_duplicate_pairs,
                        parse_float=Decimal,
                        parse_constant=_json_bad_constant,
                    )
                except Exception as exc:
                    raise input_error(
                        f"{batch.batch_id}: JSON parse failed at line {line_number}: {exc}",
                        batch.batch_id,
                        row=line_number,
                    ) from exc
                if not isinstance(raw, dict):
                    raise input_error(
                        f"{batch.batch_id}: line {line_number} is not a JSON object",
                        batch.batch_id,
                        row=line_number,
                    )
                normalized = convert_row(
                    raw, table, subject=batch.batch_id, row_number=line_number
                )
                result.append(
                    {
                        "side": "REFUND",
                        "payment_id": normalized["payment_id"],
                        "merchant_id": normalized["merchant_id"],
                        "event_time": normalized["event_time"],
                        "amount_minor": normalized["amount"],
                        "currency": normalized["currency"],
                        "event_type": normalized["event_type"],
                        "event_id": normalized["refund_id"],
                    }
                )
                if max_rows is not None and len(result) > max_rows:
                    raise input_error(
                        f"{batch.batch_id}: batch exceeds configured row limit {max_rows}",
                        batch.batch_id,
                    )
    except EngineError:
        raise
    except UnicodeDecodeError as exc:
        raise input_error(
            f"{batch.batch_id}: file is not valid UTF-8: {exc}", batch.batch_id
        ) from exc
    except OSError as exc:
        raise input_error(f"{batch.batch_id}: cannot read file: {exc}", batch.batch_id) from exc
    if batch.rows is not None and len(result) != batch.rows:
        raise input_error(
            f"{batch.batch_id}: declared rows={batch.rows}, parsed rows={len(result)}",
            batch.batch_id,
        )
    return result
