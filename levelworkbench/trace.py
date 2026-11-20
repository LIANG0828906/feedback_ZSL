"""Append-only, hash-chained, replayable generation traces."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .errors import E_TRACE_INVALID, WorkbenchError
from .config import parse_config_document
from .models import WorkbenchConfig
from .util import canonical_json, content_hash


TRACE_VERSION = "LEVEL_TRACE_V1"
GENERATOR_VERSION = "levelworkbench-generator/1"


class TraceBuilder:
    def __init__(
        self,
        config: WorkbenchConfig,
        *,
        seed_derivation_hash: str,
        params_hash: str,
    ) -> None:
        self.config = config
        self.meta: dict[str, Any] = {
            "trace_version": TRACE_VERSION,
            "generator_version": GENERATOR_VERSION,
            "seed": config.seed,
            "seed_derivation_hash": seed_derivation_hash,
            "params_hash": params_hash,
            "config_hash": content_hash(config.to_dict()),
            "config_snapshot": config.to_dict(),
            "event_count": 0,
        }
        self.events: list[dict[str, Any]] = []
        self._finished = False

    def add(self, stage: str, kind: str, payload: dict[str, Any]) -> dict[str, Any]:
        return self._append(stage, kind, payload)

    def _append(self, stage: str, kind: str, payload: dict[str, Any]) -> dict[str, Any]:
        if self._finished:
            raise RuntimeError("cannot append to a finished trace")
        seq = len(self.events)
        prev_hash = self.events[-1]["hash"] if self.events else "0" * 64
        body = {
            "seq": seq,
            "stage": stage,
            "kind": kind,
            "payload": payload,
            "prev_hash": prev_hash,
        }
        event = {**body, "hash": content_hash(body)}
        self.events.append(event)
        return event

    def record_draw(self, draw: Any) -> None:
        self.add(draw.stage, "random_draw", draw.to_dict())

    def finish(self, *, layout_hash: str, attribution_hash: str, attribution: dict[str, Any]) -> dict[str, Any]:
        if self._finished:
            raise RuntimeError("trace already finished")
        event_count = len(self.events) + 1
        self._append(
            "final",
            "trace_end",
            {
                "event_count": event_count,
                "layout_hash": layout_hash,
                "attribution_hash": attribution_hash,
                "attribution": attribution,
            },
        )
        self._finished = True
        self.meta["event_count"] = len(self.events)
        self.meta["layout_hash"] = layout_hash
        self.meta["attribution_hash"] = attribution_hash
        meta_for_hash = dict(self.meta)
        trace_hash = content_hash({"meta": meta_for_hash, "events": self.events})
        self.meta["trace_hash"] = trace_hash
        return {"meta": self.meta, "events": self.events}


def load_trace_document(path: str | Path | None = None, *, document: Any | None = None) -> dict[str, Any]:
    if document is None:
        if path is None:
            raise WorkbenchError(E_TRACE_INVALID, "trace path or document is required")
        try:
            text = Path(path).read_text(encoding="utf-8")
        except OSError as exc:
            raise WorkbenchError(E_TRACE_INVALID, f"cannot read trace: {exc}") from exc
        try:
            document = json.loads(text)
        except json.JSONDecodeError as exc:
            raise WorkbenchError(
                E_TRACE_INVALID,
                "trace JSON is truncated or malformed",
                details={"position": exc.pos, "line": exc.lineno, "column": exc.colno},
            ) from exc
    validate_trace(document)
    return document


def validate_trace(document: Any) -> None:
    if not isinstance(document, dict):
        raise WorkbenchError(E_TRACE_INVALID, "trace root must be an object")
    meta = document.get("meta")
    events = document.get("events")
    if not isinstance(meta, dict) or not isinstance(events, list):
        raise WorkbenchError(E_TRACE_INVALID, "trace meta/events are missing")
    if not meta.get("trace_version"):
        raise WorkbenchError(E_TRACE_INVALID, "trace version is missing")
    if meta["trace_version"] != TRACE_VERSION:
        raise WorkbenchError(
            E_TRACE_INVALID,
            "unsupported trace version",
            details={"expected": TRACE_VERSION, "actual": meta["trace_version"]},
        )
    if not meta.get("generator_version"):
        raise WorkbenchError(E_TRACE_INVALID, "generator version is missing")
    if not meta.get("config_snapshot") or not meta.get("config_hash"):
        raise WorkbenchError(E_TRACE_INVALID, "trace config snapshot or hash is missing")
    if content_hash(meta["config_snapshot"]) != meta["config_hash"]:
        raise WorkbenchError(E_TRACE_INVALID, "trace config snapshot hash does not match")
    if events:
        first = events[0]
        if first.get("prev_hash") != "0" * 64:
            raise WorkbenchError(E_TRACE_INVALID, "trace hash chain does not start at zero")
    previous_hash = "0" * 64
    for expected_seq, event in enumerate(events):
        if not isinstance(event, dict):
            raise WorkbenchError(E_TRACE_INVALID, "trace event is not an object")
        if event.get("seq") != expected_seq:
            raise WorkbenchError(
                E_TRACE_INVALID,
                "trace event sequence is discontinuous",
                details={"expected": expected_seq, "actual": event.get("seq")},
            )
        if event.get("prev_hash") != previous_hash:
            raise WorkbenchError(
                E_TRACE_INVALID,
                "trace hash chain is broken",
                details={"seq": expected_seq},
            )
        body = {
            "seq": event.get("seq"),
            "stage": event.get("stage"),
            "kind": event.get("kind"),
            "payload": event.get("payload"),
            "prev_hash": event.get("prev_hash"),
        }
        if content_hash(body) != event.get("hash"):
            raise WorkbenchError(
                E_TRACE_INVALID,
                "trace event content hash does not match",
                details={"seq": expected_seq},
            )
        previous_hash = event["hash"]
    if not events or events[-1].get("kind") != "trace_end":
        raise WorkbenchError(E_TRACE_INVALID, "trace is truncated: trace_end is missing")
    if meta.get("event_count") != len(events):
        raise WorkbenchError(
            E_TRACE_INVALID,
            "trace event count does not match metadata",
            details={"expected": meta.get("event_count"), "actual": len(events)},
        )
    end_payload = events[-1].get("payload", {})
    if end_payload.get("event_count") != len(events):
        raise WorkbenchError(E_TRACE_INVALID, "trace_end event count does not match")
    if end_payload.get("layout_hash") != meta.get("layout_hash"):
        raise WorkbenchError(E_TRACE_INVALID, "trace layout hash does not match metadata")
    if end_payload.get("attribution_hash") != meta.get("attribution_hash"):
        raise WorkbenchError(E_TRACE_INVALID, "trace attribution hash does not match metadata")
    meta_for_hash = dict(meta)
    trace_hash = meta_for_hash.pop("trace_hash", None)
    if not trace_hash:
        raise WorkbenchError(E_TRACE_INVALID, "trace checksum is missing")
    if content_hash({"meta": meta_for_hash, "events": events}) != trace_hash:
        raise WorkbenchError(E_TRACE_INVALID, "trace checksum does not match")
    trace_draws = [event for event in events if event.get("kind") == "random_draw"]
    if meta.get("draw_count") is not None and int(meta["draw_count"]) != len(trace_draws):
        raise WorkbenchError(E_TRACE_INVALID, "trace random draw count does not match metadata")


def extract_random_draws(document: dict[str, Any]) -> list[dict[str, Any]]:
    validate_trace(document)
    draws: list[dict[str, Any]] = []
    for event in document["events"]:
        if event.get("kind") != "random_draw":
            continue
        payload = event.get("payload")
        if not isinstance(payload, dict):
            raise WorkbenchError(E_TRACE_INVALID, "random draw payload is missing")
        draws.append(payload)
    return draws


def trace_config(document: dict[str, Any]) -> WorkbenchConfig:
    validate_trace(document)
    snapshot = document["meta"].get("config_snapshot")
    if not isinstance(snapshot, dict):
        raise WorkbenchError(E_TRACE_INVALID, "trace config snapshot is invalid")
    try:
        return parse_config_document(snapshot)
    except WorkbenchError as exc:
        raise WorkbenchError(
            E_TRACE_INVALID,
            f"trace config snapshot cannot be parsed: {exc.message}",
            details=exc.details,
        ) from exc


def trace_json(document: dict[str, Any]) -> str:
    return json.dumps(document, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
