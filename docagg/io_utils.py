from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Iterator, Mapping


class MissingValue:
    def __repr__(self) -> str:
        return "MISSING"


MISSING = MissingValue()


@dataclass
class ColumnarBatch:
    columns: dict[str, list[Any]]
    row_indices: list[int]

    @classmethod
    def from_rows(
        cls,
        rows: Iterable[Mapping[str, Any]],
        row_indices: Iterable[int],
    ) -> "ColumnarBatch":
        row_list = list(rows)
        index_list = list(row_indices)
        if len(row_list) != len(index_list):
            raise ValueError("行数据和行号数量不一致")
        all_fields: set[str] = set()
        for row in row_list:
            all_fields.update(row.keys())
        ordered_fields = sorted(all_fields)
        columns = {
            field: [
                row[field] if field in row else MISSING
                for row in row_list
            ]
            for field in ordered_fields
        }
        return cls(columns=columns, row_indices=index_list)

    def __len__(self) -> int:
        return len(self.row_indices)

    def iter_rows(self) -> Iterator[tuple[int, dict[str, Any]]]:
        for position, row_index in enumerate(self.row_indices):
            row: dict[str, Any] = {}
            for field, values in self.columns.items():
                value = values[position]
                if value is not MISSING:
                    row[field] = value
            yield row_index, row


def iter_jsonl_batches(path: str | Path, batch_size: int) -> Iterator[ColumnarBatch]:
    if batch_size < 1:
        raise ValueError("batch_size 必须大于 0")
    rows: list[dict[str, Any]] = []
    indices: list[int] = []
    with Path(path).open("r", encoding="utf-8") as handle:
        for line_number, line in enumerate(handle, start=1):
            text = line.strip()
            if not text:
                continue
            try:
                row = json.loads(text)
            except json.JSONDecodeError as exc:
                raise ValueError(f"第 {line_number} 行不是合法 JSON: {exc}") from exc
            if not isinstance(row, dict):
                raise ValueError(f"第 {line_number} 行必须是 JSON 对象")
            rows.append(row)
            indices.append(line_number)
            if len(rows) >= batch_size:
                yield ColumnarBatch.from_rows(rows, indices)
                rows = []
                indices = []
    if rows:
        yield ColumnarBatch.from_rows(rows, indices)


def count_jsonl_records(path: str | Path) -> int:
    count = 0
    with Path(path).open("r", encoding="utf-8") as handle:
        for line in handle:
            if line.strip():
                count += 1
    return count


def sha256_file(path: str | Path) -> str:
    digest = hashlib.sha256()
    with Path(path).open("rb") as handle:
        while True:
            chunk = handle.read(1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def dump_json_file(path: str | Path, payload: Any, *, pretty: bool = False) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    with target.open("w", encoding="utf-8") as handle:
        if pretty:
            json.dump(
                payload,
                handle,
                ensure_ascii=False,
                sort_keys=True,
                indent=2,
                allow_nan=False,
            )
            handle.write("\n")
        else:
            handle.write(canonical_json(payload))
            handle.write("\n")


def digest_payload(payload: Any) -> str:
    return hashlib.sha256(canonical_json(payload).encode("utf-8")).hexdigest()
