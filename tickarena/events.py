"""Bounded, ordered event source implementations."""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any, Iterator

from .atomic import replace_with_retry
from .codes import E_ARG, E_ORDER, E_STATE, TickArenaError
from .model import MatchDef, parse_initial_state
from .numeric import canonical_bytes, canonical_json, fingerprint, require_int

EVENT_FORMAT_VERSION = 1


@dataclass(frozen=True)
class RawEvent:
    index: int
    tick: int
    payload: dict[str, Any]
    code: str | None = None
    raw_bytes: int = 0


class EventSource:
    """Sequential event source with deterministic order diagnostics."""

    def __init__(
        self,
        match: MatchDef,
        *,
        event_fingerprint: str,
        total_events: int,
    ) -> None:
        self.match = match
        self.initial_fingerprint = match.initial_fingerprint
        self.fingerprint = event_fingerprint
        self.total_events = total_events
        self._index = 0
        self._last_seen_tick = -1
        self._pending: RawEvent | None = None
        self._pending_bytes = 0
        self.max_inflight_events = 0

    def _read_next(self) -> RawEvent | None:
        raise NotImplementedError

    def _set_index(self, index: int) -> None:
        self._index = index

    def _peek(self) -> RawEvent | None:
        if self._pending is None:
            raw = self._read_next()
            if raw is None:
                return None
            self._pending = raw
            self._pending_bytes = raw.raw_bytes
        return self._pending

    def _pop(self) -> RawEvent | None:
        event = self._peek()
        if event is None:
            return None
        self._pending = None
        self._pending_bytes = 0
        return event

    def peek_tick(self) -> int | None:
        event = self._peek()
        return None if event is None else event.tick

    def take_for_tick(self, tick: int, limit: int) -> list[RawEvent]:
        if limit <= 0:
            return []
        out: list[RawEvent] = []
        while len(out) < limit:
            event = self._peek()
            if event is None or event.tick > tick:
                break
            event = self._pop()
            assert event is not None
            code = event.code
            if event.tick < 0 or event.tick < self._last_seen_tick or event.tick < tick:
                code = E_ORDER
            self._last_seen_tick = max(self._last_seen_tick, event.tick)
            out.append(replace(event, code=code))
        self.max_inflight_events = max(self.max_inflight_events, len(out))
        return out

    def discard_for_tick(self, tick: int) -> tuple[int, int | None, int | None]:
        count = 0
        first: int | None = None
        last: int | None = None
        while True:
            event = self._peek()
            if event is None or event.tick > tick:
                break
            event = self._pop()
            assert event is not None
            if first is None:
                first = event.index
            last = event.index
            count += 1
            self._last_seen_tick = max(self._last_seen_tick, event.tick)
        return count, first, last

    def drain_remaining(self) -> tuple[int, int | None, int | None]:
        count = 0
        first: int | None = None
        last: int | None = None
        while True:
            event = self._pop()
            if event is None:
                break
            if first is None:
                first = event.index
            last = event.index
            count += 1
            self._last_seen_tick = max(self._last_seen_tick, event.tick)
        return count, first, last

    def tell(self) -> int:
        # _index is the next byte/record read position and may point past a
        # look-ahead event cached in _pending.  Checkpoints must persist the
        # number of events already *consumed*.
        if self._pending is not None:
            return self._pending.index
        return self._index

    @property
    def last_seen_tick(self) -> int:
        return self._last_seen_tick

    @property
    def pending_event_bytes(self) -> int:
        return self._pending_bytes

    def seek(self, index: int, last_seen_tick: int = -1) -> None:
        if index < 0 or index > self.total_events:
            raise TickArenaError(E_ARG, f"event seek index {index} is out of range")
        self._set_index(index)
        self._last_seen_tick = last_seen_tick
        self._pending = None
        self._pending_bytes = 0

    def reset(self) -> None:
        self.seek(0, -1)

    def close(self) -> None:
        """Release subclass resources; in-memory sources have none."""


def _parse_event_line(line: bytes, index: int) -> RawEvent:
    try:
        value = json.loads(line)
    except (json.JSONDecodeError, UnicodeDecodeError):
        return RawEvent(index=index, tick=-1, payload={}, code=E_ARG, raw_bytes=len(line))
    if not isinstance(value, dict):
        return RawEvent(index=index, tick=-1, payload={}, code=E_ARG, raw_bytes=len(line))
    tick_raw = value.get("tick")
    if isinstance(tick_raw, bool) or not isinstance(tick_raw, int):
        return RawEvent(
            index=index,
            tick=-1,
            payload=value,
            code=E_ORDER,
            raw_bytes=len(line),
        )
    return RawEvent(
        index=index,
        tick=tick_raw,
        payload=value,
        raw_bytes=len(line),
    )


class InMemoryEventSource(EventSource):
    """Memory source intended for tests and small interactive matches."""

    def __init__(self, initial_state: Any, events: list[Any]) -> None:
        match = parse_initial_state(initial_state)
        normalized_events: list[Any] = []
        for event in events:
            if not isinstance(event, dict):
                normalized_events.append(event)
            else:
                normalized_events.append(dict(event))
        event_hash = fingerprint(
            {
                "initial_fingerprint": match.initial_fingerprint,
                "events": normalized_events,
            },
            "tickarena.events.v1",
        )
        super().__init__(
            match,
            event_fingerprint=event_hash,
            total_events=len(normalized_events),
        )
        self._events = normalized_events

    def _read_next(self) -> RawEvent | None:
        if self._index >= len(self._events):
            return None
        raw = self._events[self._index]
        event = _parse_event_line(canonical_bytes(raw), self._index)
        self._index += 1
        return event

    def _set_index(self, index: int) -> None:
        self._index = index


class JsonlEventSource(EventSource):
    """Streaming JSONL source.

    The source indexes line offsets but never retains the full event payload
    set.  At most the current parsed event and one engine chunk are resident.
    """

    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        if not self.path.is_file():
            raise TickArenaError(E_STATE, f"event file not found: {self.path}")
        self._offsets: list[int] = []
        hasher = hashlib.sha256()
        header: dict[str, Any] | None = None
        with self.path.open("rb") as fh:
            offset = 0
            first = True
            for line in fh:
                hasher.update(line)
                if first:
                    first = False
                    try:
                        parsed = json.loads(line)
                    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
                        raise TickArenaError(E_STATE, "invalid event file header") from exc
                    if not isinstance(parsed, dict):
                        raise TickArenaError(E_STATE, "event file header must be an object")
                    if parsed.get("kind") != "match":
                        raise TickArenaError(E_STATE, "event file must start with kind=match")
                    version = parsed.get("format_version", EVENT_FORMAT_VERSION)
                    if version != EVENT_FORMAT_VERSION:
                        raise TickArenaError(
                            E_STATE, f"unknown event format_version {version!r}"
                        )
                    header = parsed
                else:
                    if line.strip():
                        self._offsets.append(offset)
                offset += len(line)
        if header is None:
            raise TickArenaError(E_STATE, "empty event file")
        if "initial_state" not in header:
            raise TickArenaError(E_STATE, "event header is missing initial_state")
        match = parse_initial_state(header["initial_state"])
        self._line_offsets = self._offsets
        super().__init__(
            match,
            event_fingerprint=hasher.hexdigest(),
            total_events=len(self._offsets),
        )
        self._fh = self.path.open("rb")

    def _read_next(self) -> RawEvent | None:
        if self._index >= len(self._line_offsets):
            return None
        position = self._line_offsets[self._index]
        self._fh.seek(position)
        line = self._fh.readline()
        index = self._index
        self._index += 1
        return _parse_event_line(line, index)

    def _set_index(self, index: int) -> None:
        self._index = index

    def close(self) -> None:
        if not self._fh.closed:
            self._fh.close()

    def __enter__(self) -> "JsonlEventSource":
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.close()


def write_event_stream(
    path: str | Path,
    initial_state: Any,
    events: Iterator[dict[str, Any]],
) -> str:
    """Write a canonical JSONL event file atomically and return its fingerprint."""

    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    partial = target.with_name(target.name + ".partial")
    match = parse_initial_state(initial_state)
    header = {
        "kind": "match",
        "format_version": EVENT_FORMAT_VERSION,
        "initial_state": match.normalized_initial,
    }
    hasher = hashlib.sha256()
    with partial.open("wb") as fh:
        line = canonical_bytes(header) + b"\n"
        fh.write(line)
        hasher.update(line)
        for event in events:
            line = canonical_bytes(event) + b"\n"
            fh.write(line)
            hasher.update(line)
        fh.flush()
        os.fsync(fh.fileno())
    replace_with_retry(partial, target)
    return hasher.hexdigest()


def inspect_event_file(path: str | Path) -> dict[str, Any]:
    source = JsonlEventSource(path)
    try:
        return {
            "path": str(source.path),
            "seed": source.match.seed,
            "tick_count": source.match.tick_count,
            "players": len(source.match.units),
            "events": source.total_events,
            "initial_fingerprint": source.initial_fingerprint,
            "event_fingerprint": source.fingerprint,
        }
    finally:
        source.close()

