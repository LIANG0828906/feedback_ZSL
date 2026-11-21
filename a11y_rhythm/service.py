"""Background solve sessions with whole-snapshot publication and stale handles."""

from __future__ import annotations

import copy
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

from .errors import E_BUSY, E_CANCELED, E_STALE_HANDLE, RhythmError
from .models import SourceBundle, content_hash
from .store import DeliverySession, RecoveryStatus, Snapshot, VersionStore
from .timebase import FrameTable


_MATERIAL_REGISTRY_LOCK = threading.Lock()
_ACTIVE_MATERIALS: set[str] = set()


@dataclass(frozen=True)
class VersionHandle:
    material_id: str
    version: int
    chain_hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "material_id": self.material_id,
            "version": self.version,
            "chain_hash": self.chain_hash,
        }


@dataclass
class SolveTicket:
    ticket_id: str
    material_id: str
    base_handle: VersionHandle
    target_version: int
    changed_id: str
    affected_ids: list[str]
    _done: threading.Event
    _error: RhythmError | None = None
    _snapshot: Snapshot | None = None

    @property
    def done(self) -> bool:
        return self._done.is_set()

    @property
    def error(self) -> RhythmError | None:
        return copy.deepcopy(self._error)

    @property
    def published(self) -> Snapshot | None:
        return self._snapshot

    def wait(self, timeout: float | None = None) -> bool:
        return self._done.wait(timeout)

    def status(self) -> dict[str, Any]:
        value = {
            "ticket_id": self.ticket_id,
            "material_id": self.material_id,
            "base_handle": self.base_handle.to_dict(),
            "target_version": self.target_version,
            "changed_id": self.changed_id,
            "affected_ids": list(self.affected_ids),
            "done": self.done,
            "published_version": self._snapshot.version if self._snapshot else None,
        }
        if self._error is not None:
            value["error"] = self._error.to_dict()
        return value


@dataclass
class _ActiveSolve:
    ticket: SolveTicket
    cancel_event: threading.Event
    thread: threading.Thread


class PublishService:
    """A material can only have one background solve at a time.

    Publication swaps one immutable Snapshot reference under a lock. Readers
    therefore see either the complete old payload or the complete new payload,
    and an old handle is rejected after the swap.
    """

    def __init__(self, session: DeliverySession):
        self.session = session
        self.material_id = session.bundle.material_id
        snapshot = session.initialize()
        self._lock = threading.RLock()
        self._published = snapshot
        self._active: _ActiveSolve | None = None
        self._last_error: RhythmError | None = None
        self._trace_callback: Callable[[dict[str, Any]], None] | None = None

    @property
    def handle(self) -> VersionHandle:
        with self._lock:
            return self._handle(self._published)

    @staticmethod
    def _handle(snapshot: Snapshot) -> VersionHandle:
        return VersionHandle(snapshot.material_id, snapshot.version, snapshot.chain_hash)

    def start_update(
        self,
        item_id: str,
        patch: dict[str, Any],
        *,
        progress: Callable[[dict[str, Any]], None] | None = None,
    ) -> SolveTicket:
        with self._lock:
            if self._active is not None and self._active.thread.is_alive():
                raise RhythmError(
                    E_BUSY,
                    "a solve session is already active for this material",
                    item_id,
                    {"material_id": self.material_id},
                )
            with _MATERIAL_REGISTRY_LOCK:
                if self.material_id in _ACTIVE_MATERIALS:
                    raise RhythmError(
                        E_BUSY,
                        "another service already owns this material's solve session",
                        item_id,
                        {"material_id": self.material_id},
                    )
                _ACTIVE_MATERIALS.add(self.material_id)
            base = self._published
            base_handle = self._handle(base)
            patch_hash = content_hash({"item": item_id, "patch": patch, "base": base.chain_hash})
            target = base.version + 1
            isolated_patch = copy.deepcopy(patch)
            ticket = SolveTicket(
                ticket_id=f"{self.material_id}:{target}:{patch_hash[:16]}",
                material_id=self.material_id,
                base_handle=base_handle,
                target_version=target,
                changed_id=item_id,
                affected_ids=[],
                _done=threading.Event(),
            )
            cancel_event = threading.Event()
            thread = threading.Thread(
                target=self._worker,
                name=f"a11y-solve-{self.material_id}",
                args=(ticket, cancel_event, item_id, isolated_patch, progress),
                daemon=True,
            )
            self._active = _ActiveSolve(ticket, cancel_event, thread)
            try:
                thread.start()
            except BaseException:
                self._active = None
                with _MATERIAL_REGISTRY_LOCK:
                    _ACTIVE_MATERIALS.discard(self.material_id)
                raise
            return ticket

    def _worker(
        self,
        ticket: SolveTicket,
        cancel_event: threading.Event,
        item_id: str,
        patch: dict[str, Any],
        progress: Callable[[dict[str, Any]], None] | None,
    ) -> None:
        try:
            snapshot = self.session.update_item(
                item_id,
                patch,
                expected_version=ticket.base_handle.version,
                cancel_check=cancel_event.is_set,
                progress=progress,
            )
            with self._lock:
                self._published = snapshot
                ticket._snapshot = snapshot
                ticket.affected_ids = list(snapshot.result.affected_ids)
                self._last_error = None
        except RhythmError as error:
            with self._lock:
                ticket._error = copy.deepcopy(error)
                self._last_error = copy.deepcopy(error)
        except BaseException as exc:
            with self._lock:
                error = RhythmError(
                    E_CANCELED if cancel_event.is_set() else "E_INTERNAL",
                    "background solve failed",
                    item_id,
                    {"type": type(exc).__name__, "message": str(exc)},
                )
                ticket._error = error
                self._last_error = error
        finally:
            with self._lock:
                if self._active is not None and self._active.ticket is ticket:
                    self._active = None
                with _MATERIAL_REGISTRY_LOCK:
                    _ACTIVE_MATERIALS.discard(self.material_id)
            ticket._done.set()

    def cancel(self, ticket: SolveTicket) -> None:
        with self._lock:
            if self._active is None or self._active.ticket.ticket_id != ticket.ticket_id:
                if not ticket.done:
                    raise RhythmError(E_BUSY, "ticket does not own the active solve", ticket.changed_id)
                return
            self._active.cancel_event.set()

    def query(self, handle: VersionHandle | None = None) -> dict[str, Any]:
        with self._lock:
            current = self._published
            if handle is not None:
                expected = self._handle(current)
                if handle != expected:
                    raise RhythmError(
                        E_STALE_HANDLE,
                        "requested snapshot handle has been superseded",
                        None,
                        {
                            "requested": handle.to_dict(),
                            "current": expected.to_dict(),
                        },
                    )
            return {
                "handle": self._handle(current).to_dict(),
                "payload": copy.deepcopy(current.payload),
            }

    def result(self) -> dict[str, Any]:
        with self._lock:
            return copy.deepcopy(self._published.payload)

    @property
    def last_error(self) -> RhythmError | None:
        with self._lock:
            return copy.deepcopy(self._last_error)

    @property
    def busy(self) -> bool:
        with self._lock:
            return self._active is not None and self._active.thread.is_alive()

    def recover(
        self,
        *,
        expected_version: int | None = None,
        progress: Callable[[dict[str, Any]], None] | None = None,
    ) -> RecoveryStatus:
        with self._lock:
            if self._active is not None and self._active.thread.is_alive():
                raise RhythmError(
                    E_BUSY,
                    "cannot recover while another solve session is active",
                    None,
                    {"material_id": self.material_id},
                )
            with _MATERIAL_REGISTRY_LOCK:
                if self.material_id in _ACTIVE_MATERIALS:
                    raise RhythmError(
                        E_BUSY,
                        "another service already owns this material's solve session",
                        None,
                        {"material_id": self.material_id},
                    )
                _ACTIVE_MATERIALS.add(self.material_id)
        try:
            status = self.session.recover(
                expected_version=expected_version,
                progress=progress,
            )
        finally:
            with _MATERIAL_REGISTRY_LOCK:
                _ACTIVE_MATERIALS.discard(self.material_id)
        with self._lock:
            if self.session.snapshot is not None:
                self._published = self.session.snapshot
        return status


def open_publish_service(
    bundle: SourceBundle,
    frame_table: FrameTable,
    coverage_limit: int,
    store_path: str | Path,
) -> PublishService:
    session = DeliverySession.open(bundle, frame_table, coverage_limit, store_path)
    return PublishService(session)


def compact_view(payload: dict[str, Any]) -> dict[str, Any]:
    result = payload["result"]
    return {
        "material_id": payload["material_id"],
        "version": payload["version"],
        "params_version": payload["params_version"],
        "result_hash": result["result_hash"],
        "items": [
            {
                "id": item["id"],
                "kind": item["kind"],
                "text": item["text"],
                "speaker": item["speaker"],
                "start": item["start"],
                "end": item["end"],
                "readable": item["readable"],
                "status": item["status"],
                "decision": item["decision"],
                "conflicts": item["conflicts"],
            }
            for item in result["items"]
        ],
        "affected_ids": result["affected_ids"],
        "reused_ids": result["reused_ids"],
        "conflicts": result["conflicts"],
        "compliance": {
            "compliant": payload["compliance"]["compliant"],
            "violations": payload["compliance"]["violations"],
        },
    }
