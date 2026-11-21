"""Content-hashed, byte-stable pick traces and exact replay."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping

from .camera import Camera
from .canonical import canonical_json_bytes, sha256_json
from .errors import E_TRACE_INVALID, fail
from .picking import PickResult, pick_snapshot
from .versioning import VersionStore

TRACE_FORMAT = "tactical-pick-trace"
TRACE_SCHEMA = 1
GENESIS_HASH = "0" * 64


def _line(value: Any) -> bytes:
    return canonical_json_bytes(value) + b"\n"


@dataclass
class TraceRecorder:
    records: list[dict[str, Any]]
    chain_head: str = GENESIS_HASH

    def __init__(self) -> None:
        self.records = []
        self.chain_head = GENESIS_HASH

    def append(
        self,
        result: PickResult,
        camera: Camera,
        *,
        label: str | None = None,
    ) -> dict[str, Any]:
        record: dict[str, Any] = {
            "seq": len(self.records),
            "screen": list(result.screen),
            "camera": camera.to_dict(),
            "region_version": result.version,
            "region_hash": result.content_hash,
            "ray": result.ray.to_dict(),
            "hit": result.hit.to_dict(),
            "diagnostics": [item.to_dict() for item in result.diagnostics],
            "prev_hash": self.chain_head,
        }
        if label is not None:
            record["label"] = label
        entry_hash = sha256_json(record)
        record["entry_hash"] = entry_hash
        self.records.append(record)
        self.chain_head = entry_hash
        return record

    def to_bytes(self) -> bytes:
        header = {
            "format": TRACE_FORMAT,
            "schema": TRACE_SCHEMA,
            "kind": "header",
            "genesis_hash": GENESIS_HASH,
        }
        footer = {
            "kind": "footer",
            "record_count": len(self.records),
            "chain_head": self.chain_head,
        }
        parts = [_line(header)]
        parts.extend(_line(record) for record in self.records)
        parts.append(_line(footer))
        return b"".join(parts)


@dataclass(frozen=True)
class LoadedTrace:
    records: tuple[dict[str, Any], ...]
    chain_head: str

    def to_bytes(self) -> bytes:
        header = {
            "format": TRACE_FORMAT,
            "schema": TRACE_SCHEMA,
            "kind": "header",
            "genesis_hash": GENESIS_HASH,
        }
        footer = {
            "kind": "footer",
            "record_count": len(self.records),
            "chain_head": self.chain_head,
        }
        return b"".join(
            [_line(header), *(_line(record) for record in self.records), _line(footer)]
        )


def _require(mapping: Mapping[str, Any], key: str, location: str) -> Any:
    if key not in mapping or mapping[key] is None:
        fail(
            E_TRACE_INVALID,
            "trace record is missing a required field",
            field=f"{location}.{key}",
        )
    return mapping[key]


def _reject_json_constant(constant: str) -> None:
    raise ValueError(f"non-finite JSON constant: {constant}")


def load_trace_bytes(data: bytes) -> LoadedTrace:
    if not data.endswith(b"\n"):
        fail(
            E_TRACE_INVALID,
            "trace is truncated before its final newline",
            field="trace.eof",
        )
    raw_lines = data.splitlines()
    if not raw_lines:
        fail(E_TRACE_INVALID, "trace is empty", field="trace")
    decoded: list[dict[str, Any]] = []
    for line_number, raw in enumerate(raw_lines, start=1):
        if not raw.strip():
            fail(
                E_TRACE_INVALID,
                "trace contains a blank line",
                field=f"trace.line[{line_number}]",
            )
        try:
            value = json.loads(
                raw.decode("utf-8"),
                parse_constant=_reject_json_constant,
            )
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            fail(
                E_TRACE_INVALID,
                "trace line is not valid canonical JSON",
                field=f"trace.line[{line_number}]",
                error=str(exc),
            )
        if not isinstance(value, dict):
            fail(
                E_TRACE_INVALID,
                "trace line must be a JSON object",
                field=f"trace.line[{line_number}]",
            )
        decoded.append(value)
    header = decoded[0]
    if (
        header.get("kind") != "header"
        or header.get("format") != TRACE_FORMAT
        or header.get("schema") != TRACE_SCHEMA
        or header.get("genesis_hash") != GENESIS_HASH
    ):
        fail(E_TRACE_INVALID, "trace header is missing or unsupported", field="trace.header")
    if len(decoded) < 2:
        fail(E_TRACE_INVALID, "trace is truncated before its footer", field="trace.footer")
    footer = decoded[-1]
    if footer.get("kind") != "footer":
        fail(
            E_TRACE_INVALID,
            "trace footer is missing; the trace is truncated",
            field="trace.footer",
        )
    if any(item.get("kind") == "footer" for item in decoded[1:-1]):
        fail(
            E_TRACE_INVALID,
            "trace contains a misplaced footer",
            field="trace.footer",
        )
    records = decoded[1:-1]
    expected_count = footer.get("record_count")
    if isinstance(expected_count, bool) or not isinstance(expected_count, int):
        fail(
            E_TRACE_INVALID,
            "trace footer record count is missing or invalid",
            field="trace.footer.record_count",
        )
    if expected_count != len(records):
        fail(
            E_TRACE_INVALID,
            "trace record count does not match the footer",
            field="trace.footer.record_count",
            expected=expected_count,
            actual=len(records),
        )
    previous = GENESIS_HASH
    for index, record in enumerate(records):
        location = f"records[{index}]"
        seq = _require(record, "seq", location)
        if seq != index:
            fail(
                E_TRACE_INVALID,
                "trace sequence numbers must be contiguous",
                field=f"{location}.seq",
                expected=index,
                actual=seq,
            )
        version = _require(record, "region_version", location)
        if isinstance(version, bool) or not isinstance(version, int) or version < 1:
            fail(
                E_TRACE_INVALID,
                "trace record region version is missing or invalid",
                field=f"{location}.region_version",
            )
        _require(record, "region_hash", location)
        _require(record, "screen", location)
        _require(record, "camera", location)
        _require(record, "ray", location)
        _require(record, "hit", location)
        prev_hash = _require(record, "prev_hash", location)
        entry_hash = _require(record, "entry_hash", location)
        if prev_hash != previous:
            fail(
                E_TRACE_INVALID,
                "trace hash chain is broken",
                field=f"{location}.prev_hash",
                expected=previous,
                actual=prev_hash,
            )
        unhashed = {key: value for key, value in record.items() if key != "entry_hash"}
        actual_hash = sha256_json(unhashed)
        if entry_hash != actual_hash:
            fail(
                E_TRACE_INVALID,
                "trace entry content hash does not match its fields",
                field=f"{location}.entry_hash",
                expected=actual_hash,
                actual=entry_hash,
            )
        previous = entry_hash
    footer_head = footer.get("chain_head")
    if footer_head != previous:
        fail(
            E_TRACE_INVALID,
            "trace footer chain head does not match the records",
            field="trace.footer.chain_head",
            expected=previous,
            actual=footer_head,
        )
    return LoadedTrace(tuple(records), previous)


def load_trace(path: str | Path) -> LoadedTrace:
    file_path = Path(path)
    try:
        data = file_path.read_bytes()
    except OSError as exc:
        fail(
            E_TRACE_INVALID,
            "trace file could not be read",
            field="trace.file",
            source=str(file_path),
            error=str(exc),
        )
    return load_trace_bytes(data)


def save_trace(path: str | Path, recorder: TraceRecorder) -> None:
    Path(path).write_bytes(recorder.to_bytes())


def _compare_float_lists(
    expected: Any,
    actual: Any,
    *,
    location: str,
) -> None:
    if expected != actual:
        fail(
            E_TRACE_INVALID,
            "replayed pick result differs from the trace",
            field=location,
            expected=expected,
            actual=actual,
        )


def replay_trace(trace: LoadedTrace, store: VersionStore) -> dict[str, Any]:
    """Replay every query against its historical immutable snapshot."""

    for index, record in enumerate(trace.records):
        location = f"records[{index}]"
        version = record["region_version"]
        expected_hash = record["region_hash"]
        snapshot = store.snapshot_for_version(version, expected_hash=expected_hash)
        camera = Camera.from_dict(record["camera"])
        screen = record["screen"]
        result = pick_snapshot(snapshot, camera, screen)
        actual = result.to_dict()
        if record.get("region_version") != actual["region_version"]:
            _compare_float_lists(
                record.get("region_version"),
                actual["region_version"],
                location=f"{location}.region_version",
            )
        if record.get("region_hash") != actual["region_hash"]:
            _compare_float_lists(
                record.get("region_hash"),
                actual["region_hash"],
                location=f"{location}.region_hash",
            )
        if record.get("screen") != actual["screen"]:
            _compare_float_lists(
                record.get("screen"), actual["screen"], location=f"{location}.screen"
            )
        if record.get("ray") != actual["ray"]:
            _compare_float_lists(
                record.get("ray"), actual["ray"], location=f"{location}.ray"
            )
        if record.get("hit") != actual["hit"]:
            _compare_float_lists(
                record.get("hit"), actual["hit"], location=f"{location}.hit"
            )
        recorded_diagnostics = record.get("diagnostics", [])
        if recorded_diagnostics != actual["diagnostics"]:
            _compare_float_lists(
                recorded_diagnostics,
                actual["diagnostics"],
                location=f"{location}.diagnostics",
            )
    return {
        "ok": True,
        "replayed_records": len(trace.records),
        "chain_head": trace.chain_head,
    }
