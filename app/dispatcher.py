from __future__ import annotations

import json
import logging
import os
import socket
import threading
import time
import uuid
from typing import Callable

from .database import Database, EventRecord
from .http_client import HttpOutcome, get_json, post_event


LOGGER = logging.getLogger(__name__)


class DispatchManager:
    def __init__(
        self,
        database: Database,
        *,
        max_workers: int = 64,
        lease_ms: int = 5000,
        poll_seconds: float = 0.02,
        health_interval_seconds: float = 1.0,
        recovery_successes: int = 2,
        client: Callable[..., HttpOutcome] | None = None,
        health_get: Callable[..., tuple[int | None, object, str]] | None = None,
    ):
        self.database = database
        self.max_workers = max(1, max_workers)
        self.lease_ms = max(1000, lease_ms)
        self.poll_seconds = max(0.001, poll_seconds)
        self.health_interval_seconds = max(0.1, health_interval_seconds)
        self.recovery_successes = max(1, recovery_successes)
        self.client = client or post_event
        self.health_get = health_get or get_json
        self.worker_id = (
            f"{socket.gethostname()}:{os.getpid()}:{threading.get_ident()}:"
            f"{uuid.uuid4().hex[:8]}"
        )
        self._stop = threading.Event()
        self._slots = threading.BoundedSemaphore(self.max_workers)
        self._threads: set[threading.Thread] = set()
        self._threads_lock = threading.Lock()
        self._scheduler_thread: threading.Thread | None = None
        self._health_thread: threading.Thread | None = None
        self._health_successes: dict[str, int] = {}

    def start(self) -> None:
        if self._scheduler_thread is not None:
            return
        self._scheduler_thread = threading.Thread(
            target=self._scheduler_loop,
            name="webhook-scheduler",
            daemon=True,
        )
        self._health_thread = threading.Thread(
            target=self._health_loop,
            name="webhook-health",
            daemon=True,
        )
        self._scheduler_thread.start()
        self._health_thread.start()

    def stop(self, timeout: float = 5.0) -> None:
        self._stop.set()
        threads: list[threading.Thread] = []
        if self._scheduler_thread is not None:
            threads.append(self._scheduler_thread)
        if self._health_thread is not None:
            threads.append(self._health_thread)
        with self._threads_lock:
            threads.extend(list(self._threads))
        deadline = time.monotonic() + timeout
        for thread in threads:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            thread.join(timeout=remaining)

    def _scheduler_loop(self) -> None:
        while not self._stop.is_set():
            if not self._slots.acquire(blocking=False):
                self._stop.wait(0.005)
                continue
            try:
                claim = self.database.claim_next(self.worker_id, self.lease_ms)
            except Exception:
                self._slots.release()
                LOGGER.exception("failed to claim an event")
                self._stop.wait(0.25)
                continue
            if claim is None:
                self._slots.release()
                self._stop.wait(self.poll_seconds)
                continue
            thread = threading.Thread(
                target=self._dispatch_with_slot,
                args=(claim,),
                name=f"webhook-dispatch-{claim.event_id[:24]}",
                daemon=True,
            )
            with self._threads_lock:
                self._threads.add(thread)
            thread.start()

    def process_one(self) -> bool:
        """Claim and dispatch one event synchronously. Intended for tests."""
        claim = self.database.claim_next(self.worker_id, self.lease_ms)
        if claim is None:
            return False
        self.dispatch_claim(claim)
        return True

    def _dispatch_with_slot(self, claim: EventRecord) -> None:
        try:
            self.dispatch_claim(claim)
        finally:
            current = threading.current_thread()
            with self._threads_lock:
                self._threads.discard(current)
            self._slots.release()

    def dispatch_claim(self, claim: EventRecord) -> None:
        try:
            payload = json.loads(claim.payload_json)
            outcome = self.client(
                claim.receiver_url,
                event_id=claim.event_id,
                tenant_id=claim.tenant_id,
                payload=payload,
                attempt=claim.attempts + 1,
                timeout_ms=claim.timeout_ms,
            )
        except Exception as exc:
            LOGGER.exception("dispatch call failed for %s", claim.event_id)
            outcome = HttpOutcome(
                success=False,
                status_code=None,
                body="",
                kind="connection_error",
                message=f"dispatch error: {exc}",
                timed_out=False,
                permanent=False,
                latency_ms=0.0,
            )
        try:
            if outcome.success:
                self.database.complete_success(claim)
                return
            action = self.database.complete_failure(
                claim,
                kind=outcome.kind,
                message=outcome.message,
                timed_out=outcome.timed_out,
                permanent=outcome.permanent,
            )
            LOGGER.info(
                "dispatch event=%s tenant=%s result=%s status=%s action=%s",
                claim.event_id,
                claim.tenant_id,
                outcome.kind,
                outcome.status_code,
                action,
            )
        except Exception:
            LOGGER.exception("failed to persist result for %s", claim.event_id)

    def _health_loop(self) -> None:
        while not self._stop.wait(self.health_interval_seconds):
            try:
                degraded = self.database.degraded_tenants()
            except Exception:
                LOGGER.exception("failed to read degraded tenants")
                continue
            active_ids = {item["tenant_id"] for item in degraded}
            for tenant_id in list(self._health_successes):
                if tenant_id not in active_ids:
                    self._health_successes.pop(tenant_id, None)
            for tenant in degraded:
                tenant_id = tenant["tenant_id"]
                status, _, _ = self.health_get(
                    f"{tenant['receiver_url'].rstrip('/')}/health",
                    timeout_ms=min(1500, max(250, int(tenant["timeout_ms"]))),
                )
                if status is not None and 200 <= status < 300:
                    successes = self._health_successes.get(tenant_id, 0) + 1
                    self._health_successes[tenant_id] = successes
                    if successes >= self.recovery_successes:
                        try:
                            self.database.recover_tenant(tenant_id)
                        except Exception:
                            LOGGER.exception(
                                "failed to recover tenant %s", tenant_id
                            )
                        self._health_successes.pop(tenant_id, None)
                else:
                    self._health_successes[tenant_id] = 0

