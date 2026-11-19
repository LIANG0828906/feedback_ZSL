from __future__ import annotations

import math
import threading
import time
from dataclasses import asdict, dataclass

from .config import TenantConfig


@dataclass(frozen=True)
class TenantLimits:
    rate_per_second: float
    burst: int
    max_inflight: int


@dataclass(frozen=True)
class Admission:
    allowed: bool
    reason: str | None = None
    retry_after_ms: int = 0
    limit: TenantLimits | None = None
    inflight: int = 0


@dataclass(frozen=True)
class TenantState:
    tenant_id: str
    limits: TenantLimits | None
    enabled: bool
    tokens: float
    inflight: int

    def to_dict(self) -> dict[str, object]:
        data = asdict(self)
        data["limits"] = asdict(self.limits) if self.limits else None
        return data


class _Bucket:
    def __init__(self, config: TenantConfig, now: float) -> None:
        self.enabled = config.enabled
        self.limits = TenantLimits(
            rate_per_second=config.rate_per_second,
            burst=config.burst,
            max_inflight=config.max_inflight,
        )
        self.tokens = float(config.burst)
        self.updated_at = now
        self.inflight = 0
        self.lock = threading.RLock()

    def refill(self, now: float) -> None:
        elapsed = max(0.0, now - self.updated_at)
        self.tokens = min(
            float(self.limits.burst),
            self.tokens + elapsed * self.limits.rate_per_second,
        )
        self.updated_at = now

    def acquire(self, now: float) -> Admission:
        with self.lock:
            if not self.enabled:
                return Admission(
                    allowed=False,
                    reason="tenant_not_active",
                    limit=self.limits,
                    inflight=self.inflight,
                )
            self.refill(now)
            if self.tokens < 1.0:
                missing = 1.0 - self.tokens
                retry_ms = max(
                    1, math.ceil(missing / self.limits.rate_per_second * 1000)
                )
                return Admission(
                    allowed=False,
                    reason="rate",
                    retry_after_ms=retry_ms,
                    limit=self.limits,
                    inflight=self.inflight,
                )
            if self.inflight >= self.limits.max_inflight:
                retry_ms = max(
                    1,
                    math.ceil(
                        1000.0 / max(self.limits.rate_per_second, 0.001)
                    ),
                )
                return Admission(
                    allowed=False,
                    reason="concurrency",
                    retry_after_ms=retry_ms,
                    limit=self.limits,
                    inflight=self.inflight,
                )
            self.tokens -= 1.0
            self.inflight += 1
            return Admission(
                allowed=True,
                limit=self.limits,
                inflight=self.inflight,
            )

    def release(self) -> None:
        with self.lock:
            self.inflight = max(0, self.inflight - 1)

    def adjust(self, limits: TenantLimits, now: float) -> None:
        with self.lock:
            self.limits = limits
            self.tokens = min(self.tokens, float(limits.burst))
            self.updated_at = now

    def snapshot(self, tenant_id: str, now: float) -> TenantState:
        with self.lock:
            self.refill(now)
            return TenantState(
                tenant_id=tenant_id,
                limits=self.limits,
                enabled=self.enabled,
                tokens=self.tokens,
                inflight=self.inflight,
            )


class TenantLimiterRegistry:
    def __init__(
        self,
        tenants: list[TenantConfig],
        *,
        clock=time.monotonic,
    ) -> None:
        self.clock = clock
        now = clock()
        self._buckets = {
            tenant.tenant_id: _Bucket(tenant, now) for tenant in tenants
        }
        self._lock = threading.RLock()

    def acquire(self, tenant_id: str) -> Admission:
        bucket = self._buckets.get(tenant_id)
        if bucket is None:
            return Admission(allowed=False, reason="tenant_not_active")
        return bucket.acquire(self.clock())

    def release(self, tenant_id: str) -> None:
        bucket = self._buckets.get(tenant_id)
        if bucket is not None:
            bucket.release()

    def adjust(
        self,
        tenant_id: str,
        *,
        rate_per_second: float | None = None,
        burst: int | None = None,
        max_inflight: int | None = None,
    ) -> TenantLimits:
        with self._lock:
            bucket = self._buckets[tenant_id]
            old = bucket.limits
            limits = TenantLimits(
                rate_per_second=(
                    old.rate_per_second if rate_per_second is None else rate_per_second
                ),
                burst=old.burst if burst is None else burst,
                max_inflight=(
                    old.max_inflight if max_inflight is None else max_inflight
                ),
            )
            bucket.adjust(limits, self.clock())
            return limits

    def current_limits(self, tenant_id: str) -> TenantLimits | None:
        bucket = self._buckets.get(tenant_id)
        return bucket.limits if bucket else None

    def inflight(self, tenant_id: str) -> int:
        bucket = self._buckets.get(tenant_id)
        if bucket is None:
            return 0
        with bucket.lock:
            return bucket.inflight

    def total_inflight(self) -> int:
        return sum(self.inflight(tenant_id) for tenant_id in self._buckets)

    def snapshots(self) -> dict[str, dict[str, object]]:
        now = self.clock()
        return {
            tenant_id: bucket.snapshot(tenant_id, now).to_dict()
            for tenant_id, bucket in self._buckets.items()
        }

    @property
    def tenant_ids(self) -> tuple[str, ...]:
        return tuple(self._buckets)
