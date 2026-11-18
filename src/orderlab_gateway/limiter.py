from __future__ import annotations

import threading
import time
from collections import defaultdict

from .errors import E_CONN, GatewayError


class FairLimiter:
    def __init__(
        self,
        max_connections: int,
        per_tenant_connections: int,
        acquire_timeout_ms: int,
    ) -> None:
        self.max_connections = max_connections
        self.per_tenant_connections = per_tenant_connections
        self.acquire_timeout_ms = acquire_timeout_ms
        self._global = threading.BoundedSemaphore(max_connections)
        self._tenant_semaphores: dict[str, threading.BoundedSemaphore] = {}
        self._lock = threading.Lock()
        self._active_by_tenant: dict[str, int] = defaultdict(int)
        self._active = 0

    def _tenant_semaphore(self, tenant: str) -> threading.BoundedSemaphore:
        with self._lock:
            semaphore = self._tenant_semaphores.get(tenant)
            if semaphore is None:
                semaphore = threading.BoundedSemaphore(self.per_tenant_connections)
                self._tenant_semaphores[tenant] = semaphore
            return semaphore

    def acquire(self, tenant: str) -> None:
        deadline = time.monotonic() + self.acquire_timeout_ms / 1000.0
        tenant_semaphore = self._tenant_semaphore(tenant)
        if not self._poll_acquire(tenant_semaphore, deadline):
            raise GatewayError(
                E_CONN,
                "tenant connection quota is exhausted",
                details={
                    "reason": "tenant_limit",
                    "tenant": tenant,
                    "max_connections": self.max_connections,
                    "per_tenant_connections": self.per_tenant_connections,
                    "active": self.status()["active"],
                },
            )
        if not self._poll_acquire(self._global, deadline):
            tenant_semaphore.release()
            raise GatewayError(
                E_CONN,
                "database connection limit is exhausted",
                details={
                    "reason": "global_limit",
                    "tenant": tenant,
                    "max_connections": self.max_connections,
                    "per_tenant_connections": self.per_tenant_connections,
                    "active": self.status()["active"],
                },
            )
        with self._lock:
            self._active += 1
            self._active_by_tenant[tenant] += 1

    def _poll_acquire(self, semaphore: threading.BoundedSemaphore, deadline: float) -> bool:
        while True:
            if semaphore.acquire(blocking=False):
                return True
            if time.monotonic() >= deadline:
                return False
            time.sleep(0.005)

    def release(self, tenant: str) -> None:
        with self._lock:
            self._active = max(0, self._active - 1)
            self._active_by_tenant[tenant] = max(0, self._active_by_tenant[tenant] - 1)
        self._global.release()
        self._tenant_semaphore(tenant).release()

    def status(self) -> dict[str, object]:
        with self._lock:
            active = self._active
            by_tenant = {
                tenant: count
                for tenant, count in self._active_by_tenant.items()
                if count
            }
        return {
            "active": active,
            "max": self.max_connections,
            "per_tenant_max": self.per_tenant_connections,
            "by_tenant": dict(sorted(by_tenant.items())),
        }
