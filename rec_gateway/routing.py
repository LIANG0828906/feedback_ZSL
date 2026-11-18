from __future__ import annotations

import hashlib
import secrets
import threading
from dataclasses import dataclass

from rec_gateway.errors import BusinessError


BUCKET_COUNT = 10000


@dataclass(frozen=True)
class RouteChoice:
    candidate_version: str
    client_bucket: int
    canary_weight: float
    stable_version: str
    canary_version: str
    client_keyed: bool


class Router:
    def __init__(
        self,
        stable_version: str,
        canary_version: str,
        tenant_weights: dict[str, float],
    ) -> None:
        self.stable_version = stable_version
        self.canary_version = canary_version
        self._lock = threading.RLock()
        self._weights = dict(tenant_weights)
        self._validate_weights(self._weights)

    @staticmethod
    def _validate_weights(weights: dict[str, float]) -> None:
        for tenant_id, weight in weights.items():
            if not tenant_id:
                raise ValueError("tenant_id cannot be empty")
            if not isinstance(weight, (int, float)) or isinstance(weight, bool):
                raise ValueError(f"invalid canary weight for {tenant_id}")
            if float(weight) < 0.0 or float(weight) > 1.0:
                raise ValueError(f"canary weight out of range for {tenant_id}")

    def has_tenant(self, tenant_id: str) -> bool:
        with self._lock:
            return tenant_id in self._weights

    def weight_for(self, tenant_id: str) -> float:
        with self._lock:
            try:
                return self._weights[tenant_id]
            except KeyError:
                raise BusinessError(
                    "UNKNOWN_TENANT",
                    "tenant is not configured",
                    {"tenant_id": tenant_id},
                )

    def tenant_weights(self) -> dict[str, float]:
        with self._lock:
            return dict(self._weights)

    def set_weight(self, tenant_id: str, weight: float) -> tuple[float, float]:
        if isinstance(weight, bool) or not isinstance(weight, (int, float)):
            raise BusinessError(
                "ROUTING_CONFIG_INVALID",
                "canary_weight must be numeric",
                {"tenant_id": tenant_id},
            )
        weight = float(weight)
        if weight < 0.0 or weight > 1.0:
            raise BusinessError(
                "ROUTING_CONFIG_INVALID",
                "canary_weight must be within [0, 1]",
                {"tenant_id": tenant_id, "canary_weight": weight},
            )
        with self._lock:
            if tenant_id not in self._weights:
                raise BusinessError(
                    "UNKNOWN_TENANT",
                    "tenant is not configured",
                    {"tenant_id": tenant_id},
                )
            previous = self._weights[tenant_id]
            self._weights[tenant_id] = weight
            return previous, weight

    def set_all_canary_weights(
        self, weight: float, expected_positive: bool = True
    ) -> dict[str, float]:
        if weight < 0.0 or weight > 1.0:
            raise ValueError("weight out of range")
        with self._lock:
            if expected_positive and not any(value > 0.0 for value in self._weights.values()):
                return dict(self._weights)
            previous = dict(self._weights)
            for tenant_id in self._weights:
                self._weights[tenant_id] = weight
            return previous

    def any_canary_enabled(self) -> bool:
        with self._lock:
            return any(weight > 0.0 for weight in self._weights.values())

    def choose(
        self, tenant_id: str, client_id: str | None, request_id: str
    ) -> RouteChoice:
        weight = self.weight_for(tenant_id)
        if client_id:
            digest = hashlib.sha256(
                f"{tenant_id}\x00{client_id}".encode("utf-8")
            ).digest()
            bucket = int.from_bytes(digest[:8], "big") % BUCKET_COUNT
            client_keyed = True
        else:
            bucket = secrets.randbelow(BUCKET_COUNT)
            client_keyed = False
        canary = bucket < int(round(weight * BUCKET_COUNT))
        return RouteChoice(
            candidate_version=(
                self.canary_version if canary else self.stable_version
            ),
            client_bucket=bucket,
            canary_weight=weight,
            stable_version=self.stable_version,
            canary_version=self.canary_version,
            client_keyed=client_keyed,
        )

    def preview(self, tenant_id: str, client_id: str | None) -> dict[str, object]:
        weight = self.weight_for(tenant_id)
        if client_id:
            digest = hashlib.sha256(
                f"{tenant_id}\x00{client_id}".encode("utf-8")
            ).digest()
            bucket: int | None = int.from_bytes(digest[:8], "big") % BUCKET_COUNT
            candidate = (
                self.canary_version
                if bucket < int(round(weight * BUCKET_COUNT))
                else self.stable_version
            )
        else:
            bucket = None
            candidate = None
        return {
            "tenant_id": tenant_id,
            "client_id": client_id,
            "client_bucket": bucket,
            "candidate_version": candidate,
            "canary_weight": weight,
            "stable_version": self.stable_version,
            "canary_version": self.canary_version,
            "client_keyed": client_id is not None,
        }
