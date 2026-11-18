from __future__ import annotations

import asyncio
from collections import defaultdict
from typing import Any, Iterable

from .util import canonical_json, mono_ms, utc_now


OPEN_KINDS = {"connect_success", "accept", "open"}
CLOSE_KINDS = {"close", "reset", "timeout", "connect_fail", "connect_timeout", "syn_dropped"}


class EventStore:
    """Batch-scoped event collector with connection state invariants.

    Events remain uncommitted in memory until the complete batch is written in
    one SQLite transaction.
    """

    def __init__(self) -> None:
        self._events: list[dict[str, Any]] = []
        self._by_request: dict[tuple[str, str], list[int]] = defaultdict(list)
        self._by_batch: dict[str, list[int]] = defaultdict(list)
        self._connection_state: dict[tuple[str, str, str], str] = {}
        self._lock = asyncio.Lock()

    async def add(
        self,
        *,
        batch_id: str,
        request_id: str,
        hop: str,
        connection_id: str,
        kind: str,
        initiator: str,
        correlation_id: str,
        peer_connection_id: str | None = None,
        ts: str | None = None,
        ts_mono_ms: int | None = None,
        details: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        async with self._lock:
            state_key = (batch_id, hop, connection_id)
            current_state = self._connection_state.get(state_key)
            if kind in OPEN_KINDS:
                if current_state == "closed":
                    raise ValueError(
                        f"connection {hop}/{connection_id} cannot be opened after close"
                    )
                self._connection_state[state_key] = "open"
            elif kind in CLOSE_KINDS:
                if current_state == "closed" and kind in {"close", "reset"}:
                    return {
                        "duplicate_suppressed": True,
                        "connection_id": connection_id,
                        "hop": hop,
                        "kind": kind,
                    }
                self._connection_state[state_key] = "closed"

            event = {
                "event_id": len(self._events) + 1,
                "batch_id": batch_id,
                "request_id": request_id,
                "hop": hop,
                "connection_id": connection_id,
                "peer_connection_id": peer_connection_id,
                "correlation_id": correlation_id,
                "kind": kind,
                "initiator": initiator,
                "ts": ts or utc_now(),
                "ts_mono_ms": ts_mono_ms if ts_mono_ms is not None else mono_ms(),
                "details": details or {},
                "details_json": canonical_json(details or {}),
            }
            self._events.append(event)
            index = len(self._events) - 1
            self._by_request[(batch_id, request_id)].append(index)
            self._by_batch[batch_id].append(index)
            return event

    async def for_request(
        self, batch_id: str, request_id: str, connection_ids: Iterable[str] = ()
    ) -> list[dict[str, Any]]:
        async with self._lock:
            indexes = set(self._by_request.get((batch_id, request_id), []))
            connection_set = {value for value in connection_ids if value}
            if connection_set:
                for index, event in enumerate(self._events):
                    if event["batch_id"] != batch_id:
                        continue
                    if (
                        event["connection_id"] in connection_set
                        or event["peer_connection_id"] in connection_set
                        or event["correlation_id"] in connection_set
                    ):
                        indexes.add(index)
            selected = [self._events[index] for index in indexes]
            return [
                dict(event)
                for event in sorted(
                    selected,
                    key=lambda item: (
                        item["ts_mono_ms"],
                        item["event_id"],
                        item["hop"],
                    ),
                )
            ]

    async def snapshot(self, batch_id: str) -> list[dict[str, Any]]:
        async with self._lock:
            return [dict(self._events[index]) for index in self._by_batch.get(batch_id, [])]

    async def discard(self, batch_id: str) -> None:
        async with self._lock:
            kept = [event for event in self._events if event["batch_id"] != batch_id]
            self._events = kept
            self._by_request = defaultdict(list)
            self._by_batch = defaultdict(list)
            for index, event in enumerate(self._events):
                self._by_request[(event["batch_id"], event["request_id"])].append(index)
                self._by_batch[event["batch_id"]].append(index)
            state_keys = [
                key for key in self._connection_state if key[0] == batch_id
            ]
            for key in state_keys:
                del self._connection_state[key]


def expected_hops(path: str) -> tuple[str, ...]:
    return ("client", "proxy", "upstream")


def events_for_hop(events: list[dict[str, Any]], hop: str) -> list[dict[str, Any]]:
    return [event for event in events if event["hop"] == hop]
