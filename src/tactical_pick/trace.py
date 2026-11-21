"""Hash-chained, timestamp-free pick and placement traces with replay."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

from .camera import Camera
from .errors import ToolError, fail
from .query import PickRequest, PickResult, Picker
from .region import PlacementRequest, PlacementVerdict, RegionJudge
from .snapshot import SceneManager
from .util import atomic_write_bytes, canonical_json, hash_payload

GENESIS_HASH = "0" * 64
TRACE_SCHEMA = 1
FOOTER_KIND = "__trace_footer__"


def _payload(seq: int, kind: str, request: Any, result: Any) -> dict[str, Any]:
    return {
        "schema": TRACE_SCHEMA,
        "seq": seq,
        "kind": kind,
        "request": request,
        "result": result,
    }


@dataclass(frozen=True)
class TraceRecord:
    seq: int
    kind: str
    request: dict[str, Any]
    result: dict[str, Any]
    prev_hash: str
    entry_hash: str

    def document(self) -> dict[str, Any]:
        return {
            ** _payload(self.seq, self.kind, self.request, self.result),
            "prev_hash": self.prev_hash,
            "entry_hash": self.entry_hash,
        }


class TraceLog:
    def __init__(self) -> None:
        self._records: list[TraceRecord] = []
        self._last_hash = GENESIS_HASH

    @property
    def records(self) -> tuple[TraceRecord, ...]:
        return tuple(self._records)

    def _append(self, kind: str, request: Any, result: Any) -> TraceRecord:
        seq = len(self._records)
        payload = _payload(seq, kind, request, result)
        entry_hash = hash_payload(self._last_hash, payload)
        record = TraceRecord(seq, kind, request, result, self._last_hash, entry_hash)
        self._records.append(record)
        self._last_hash = entry_hash
        return record

    def append_pick(self, request: PickRequest, result: PickResult) -> TraceRecord:
        request_doc = {
            "screen": list(request.screen),
            "camera": request.camera.to_dict(),
            "requested_version": request.requested_version,
        }
        return self._append("pick", request_doc, result.to_dict())

    def append_region(self, request: PlacementRequest, verdict: PlacementVerdict) -> TraceRecord:
        request_doc = {
            "polygon": [list(point) for point in request.polygon],
            "requested_version": request.requested_version,
            "height": request.height,
        }
        return self._append("region", request_doc, verdict.to_dict())

    def to_bytes(self) -> bytes:
        lines = [canonical_json(record.document()) for record in self._records]
        footer_payload = {
            "schema": TRACE_SCHEMA,
            "kind": FOOTER_KIND,
            "records": len(self._records),
            "final_hash": self._last_hash,
        }
        footer = {
            **footer_payload,
            "footer_hash": hash_payload(self._last_hash, footer_payload),
        }
        lines.append(canonical_json(footer))
        return ("\n".join(lines) + "\n").encode("utf-8")

    def write(self, path: str | Path) -> None:
        atomic_write_bytes(Path(path), self.to_bytes())

    @classmethod
    def from_bytes(cls, data: bytes, source: str = "trace") -> "TraceLog":
        log = cls()
        if not data:
            fail("E_TRACE_INVALID", "trace is empty or missing its footer", f"{source}.footer")
        if not data.endswith(b"\n"):
            fail(
                "E_TRACE_INVALID",
                "trace is truncated (missing final newline)",
                f"{source}.eof",
                first_conflict={"bytes": len(data)},
            )
        all_lines = data.splitlines()
        try:
            footer = json.loads(all_lines[-1].decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            fail("E_TRACE_INVALID", "trace footer is not valid JSON", f"{source}.footer")
        if not isinstance(footer, dict) or footer.get("kind") != FOOTER_KIND:
            fail("E_TRACE_INVALID", "trace is truncated (footer is missing)", f"{source}.footer")
        if footer.get("schema") != TRACE_SCHEMA:
            fail("E_TRACE_INVALID", "unsupported trace footer schema", f"{source}.footer.schema")
        footer_payload = {
            "schema": footer.get("schema"),
            "kind": footer.get("kind"),
            "records": footer.get("records"),
            "final_hash": footer.get("final_hash"),
        }
        expected_footer_hash = hash_payload(str(footer.get("final_hash", "")), footer_payload)
        if footer.get("footer_hash") != expected_footer_hash:
            fail(
                "E_TRACE_INVALID",
                "trace footer hash is invalid",
                f"{source}.footer.footer_hash",
            )
        body_lines = all_lines[:-1]
        if footer.get("records") != len(body_lines):
            fail(
                "E_TRACE_INVALID",
                "trace is truncated (record count differs from footer)",
                f"{source}.footer.records",
                first_conflict={"expected": footer.get("records"), "actual": len(body_lines)},
            )
        last_hash = GENESIS_HASH
        for line_number, raw_line in enumerate(body_lines, start=1):
            point = f"{source}.line[{line_number}]"
            try:
                document = json.loads(raw_line.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError):
                fail("E_TRACE_INVALID", "trace line is not valid JSON", point)
            if not isinstance(document, dict):
                fail("E_TRACE_INVALID", "trace line must be an object", point)
            for key in ("schema", "seq", "kind", "request", "result", "prev_hash", "entry_hash"):
                if key not in document:
                    fail("E_TRACE_INVALID", f"trace line is missing {key}", f"{point}.{key}")
            if document["schema"] != TRACE_SCHEMA:
                fail("E_TRACE_INVALID", "unsupported trace schema", f"{point}.schema")
            seq = document["seq"]
            if not isinstance(seq, int) or seq != line_number - 1:
                fail(
                    "E_TRACE_INVALID",
                    "trace sequence is discontinuous",
                    f"{point}.seq",
                    first_conflict={"expected": line_number - 1, "actual": seq},
                )
            if document["prev_hash"] != last_hash:
                fail(
                    "E_TRACE_INVALID",
                    "trace hash chain is broken",
                    f"{point}.prev_hash",
                    first_conflict={"expected": last_hash, "actual": document["prev_hash"]},
                )
            payload = _payload(seq, document["kind"], document["request"], document["result"])
            expected_hash = hash_payload(last_hash, payload)
            if document["entry_hash"] != expected_hash:
                fail(
                    "E_TRACE_INVALID",
                    "trace entry hash does not match its content",
                    f"{point}.entry_hash",
                    first_conflict={"expected": expected_hash, "actual": document["entry_hash"]},
                )
            log._records.append(
                TraceRecord(
                    seq=seq,
                    kind=document["kind"],
                    request=document["request"],
                    result=document["result"],
                    prev_hash=last_hash,
                    entry_hash=expected_hash,
                )
            )
            last_hash = expected_hash
        if last_hash != footer.get("final_hash"):
            fail(
                "E_TRACE_INVALID",
                "trace footer final hash does not match the entry chain",
                f"{source}.footer.final_hash",
                first_conflict={"expected": last_hash, "actual": footer.get("final_hash")},
            )
        log._last_hash = last_hash
        return log

    @classmethod
    def read(cls, path: str | Path) -> "TraceLog":
        try:
            data = Path(path).read_bytes()
        except OSError as exc:
            fail("E_TRACE_INVALID", f"cannot read trace: {exc}", "trace.file")
        return cls.from_bytes(data, str(path))


def replay_trace(trace: TraceLog, manager: SceneManager) -> dict[str, Any]:
    """Replay every record against the archived version and assert exact results."""
    picker = Picker(manager)
    judge = RegionJudge(manager)
    replayed_picks = 0
    replayed_regions = 0
    for record in trace.records:
        try:
            if record.kind == "pick":
                request_doc = record.request
                expected_version = int(record.result["scene_version"])
                request = PickRequest(
                    screen=(float(request_doc["screen"][0]), float(request_doc["screen"][1])),
                    camera=Camera.from_dict(request_doc["camera"]),
                    requested_version=request_doc.get("requested_version"),
                )
                handle = manager.acquire_archived(expected_version)
                try:
                    actual = picker.pick(request, handle=handle).to_dict()
                finally:
                    handle.release()
                if canonical_json(actual) != canonical_json(record.result):
                    fail(
                        "E_CHECK_FAILED",
                        "trace pick replay differs from recorded result",
                        f"trace.replay[{record.seq}].pick",
                        first_conflict={"expected": record.result, "actual": actual},
                    )
                replayed_picks += 1
            elif record.kind == "region":
                request_doc = record.request
                expected_version = int(record.result["scene_version"])
                request = PlacementRequest(
                    polygon=tuple((float(point[0]), float(point[1])) for point in request_doc["polygon"]),
                    requested_version=request_doc.get("requested_version"),
                    height=float(request_doc.get("height", 2.0)),
                )
                handle = manager.acquire_archived(expected_version)
                try:
                    actual = judge.judge(request, handle=handle).to_dict()
                finally:
                    handle.release()
                if canonical_json(actual) != canonical_json(record.result):
                    fail(
                        "E_CHECK_FAILED",
                        "trace placement replay differs from recorded verdict",
                        f"trace.replay[{record.seq}].region",
                        first_conflict={"expected": record.result, "actual": actual},
                    )
                replayed_regions += 1
            else:
                fail("E_TRACE_INVALID", f"unknown trace record kind {record.kind!r}", f"trace.line[{record.seq}].kind")
        except ToolError:
            raise
    return {
        "records": len(trace.records),
        "picks": replayed_picks,
        "regions": replayed_regions,
        "final_hash": trace.records[-1].entry_hash if trace.records else GENESIS_HASH,
    }

