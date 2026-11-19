"""Stable/canary traffic routing with auditable per-client decisions."""

from __future__ import annotations

import hashlib
import threading
import time
from collections import deque


def _stable_bucket(tenant_id: str, client_id: str | None, request_id: str) -> int:
    material = f"{tenant_id}|{client_id or ''}|{request_id}".encode("utf-8")
    digest = hashlib.sha256(material).hexdigest()
    return int(digest[:8], 16) % 10000


class Router:
    """Routes requests to stable/canary based on tenant weights.

    Weight changes apply to subsequent requests immediately. Every decision is
    retained in a bounded per-client audit trail.
    """

    def __init__(
        self,
        stable_version: str,
        canary_version: str,
        default_weight: float,
        tenant_defaults: dict | None = None,
        trace_size: int = 256,
    ):
        self._stable = stable_version
        self._canary = canary_version
        self._default_weight = float(default_weight)
        self._tenant_weights: dict[str, float] = dict(tenant_defaults or {})
        self._lock = threading.Lock()
        self._traces: dict[str, deque] = {}
        self._trace_size = trace_size

    @property
    def stable_version(self) -> str:
        return self._stable

    @property
    def canary_version(self) -> str:
        return self._canary

    def get_weight(self, tenant_id: str) -> float:
        with self._lock:
            if tenant_id in self._tenant_weights:
                return self._tenant_weights[tenant_id]
            return self._default_weight

    def weights_snapshot(self) -> dict:
        with self._lock:
            return {
                "default": self._default_weight,
                "tenants": dict(sorted(self._tenant_weights.items())),
            }

    def set_default_weight(self, weight: float) -> None:
        with self._lock:
            self._default_weight = self._clamp(weight)

    def set_tenant_weight(self, tenant_id: str, weight: float) -> None:
        with self._lock:
            self._tenant_weights[tenant_id] = self._clamp(weight)

    def zero_canary(self, tenant_id: str | None = None) -> None:
        with self._lock:
            # A rollback must converge every routing path, including tenant
            # overrides, so no subsequent request can reach the canary.
            self._default_weight = 0.0
            for tenant in list(self._tenant_weights):
                self._tenant_weights[tenant] = 0.0

    @staticmethod
    def _clamp(weight: float) -> float:
        return max(0.0, min(1.0, float(weight)))

    def route(
        self,
        tenant_id: str,
        client_id: str | None,
        request_id: str,
        canary_available: bool,
    ) -> dict:
        weight = self.get_weight(tenant_id)
        bucket = _stable_bucket(tenant_id, client_id, request_id)
        threshold = int(weight * 10000)
        chosen_canary = canary_available and weight > 0.0 and bucket < threshold
        target = self._canary if chosen_canary else self._stable
        reason = "canary_weight_match" if chosen_canary else (
            "canary_unavailable_fallback"
            if weight > 0.0 and not canary_available
            else "stable_weight_match"
        )
        decision = {
            "bucket": bucket,
            "canary_available": canary_available,
            "client_id": client_id,
            "request_id": request_id,
            "target_version": target,
            "tenant_id": tenant_id,
            "weight": weight,
            "reason": reason,
            "decided_at": time.time(),
        }
        self._record(decision)
        return decision

    def _record(self, decision: dict) -> None:
        key = decision["client_id"] or f"tenant:{decision['tenant_id']}:anonymous"
        with self._lock:
            trail = self._traces.get(key)
            if trail is None:
                trail = deque(maxlen=self._trace_size)
                self._traces[key] = trail
            trail.append(dict(decision))

    def client_trace(self, client_id: str) -> list[dict]:
        with self._lock:
            trail = self._traces.get(client_id)
            return [dict(item) for item in trail] if trail else []
