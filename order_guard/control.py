from __future__ import annotations

import logging
import threading
from dataclasses import asdict, dataclass
from types import MappingProxyType
from typing import Callable, Mapping

from .audit import AuditEvent, AuditWriter
from .circuit import CircuitTransition
from .config import Config, SLIConfig
from .limiter import TenantLimits
from .metrics import Metrics
from .sli import SLISnapshot

LOGGER = logging.getLogger("order_guard.control")


@dataclass(frozen=True)
class Cause:
    kind: str
    level: int
    business_code: str
    reason: str


@dataclass(frozen=True)
class ProtectionState:
    degrade_level: int
    level_name: str
    protection_state: str
    protection_code: str
    causes: Mapping[str, Cause]
    circuit_state: str
    capacity_protected: bool
    writes_paused: bool
    page_size_cap: int

    def label(self) -> str:
        return (
            f"level={self.degrade_level};mode={self.protection_state};"
            f"circuit={self.circuit_state};capacity={str(self.capacity_protected).lower()};"
            f"code={self.protection_code}"
        )

    def to_dict(self) -> dict[str, object]:
        return {
            "degrade_level": self.degrade_level,
            "level_name": self.level_name,
            "protection_state": self.protection_state,
            "protection_code": self.protection_code,
            "causes": {
                key: asdict(value) for key, value in self.causes.items()
            },
            "circuit_state": self.circuit_state,
            "capacity_protected": self.capacity_protected,
            "writes_paused": self.writes_paused,
            "page_size_cap": self.page_size_cap,
        }


class ControlPlane:
    def __init__(
        self,
        config: Config,
        audit: AuditWriter,
        metrics: Metrics,
        *,
        metrics_snapshot: Callable[[], dict[str, object]],
        sli_snapshot: Callable[[], dict[str, object]],
    ) -> None:
        self.config = config
        self.audit = audit
        self.metrics = metrics
        self._metrics_snapshot = metrics_snapshot
        self._sli_snapshot = sli_snapshot
        self._lock = threading.RLock()
        self._state = self._build_state(
            causes={},
            circuit_state="closed",
            capacity_protected=False,
        )
        self._publish_metrics(self._state)

    @property
    def state(self) -> ProtectionState:
        with self._lock:
            return self._state

    @property
    def degrade_level(self) -> int:
        return self.state.degrade_level

    @property
    def writes_paused(self) -> bool:
        return self.state.writes_paused

    @property
    def page_size_cap(self) -> int:
        return self.state.page_size_cap

    def start(self) -> None:
        with self._lock:
            event = AuditEvent(
                event_type="SERVICE_STARTED",
                business_code="OK",
                reason="service_started",
                from_state=None,
                to_state=self._state.label(),
                thresholds={
                    "slis": {
                        name: asdict(sli) for name, sli in self.config.slis.items()
                    },
                    "circuit": asdict(self.config.circuit),
                    "capacity": asdict(self.config.capacity),
                },
                decision={"state": self._state.to_dict()},
                metrics_snapshot=self._metrics_snapshot(),
                sli_snapshot=self._sli_snapshot(),
            )
            self.audit.append_many([event])
        self._publish_tenant_limits()

    def _build_state(
        self,
        *,
        causes: dict[str, Cause],
        circuit_state: str,
        capacity_protected: bool,
    ) -> ProtectionState:
        level = max((cause.level for cause in causes.values()), default=0)
        level_config = self.config.degrade_levels[level]
        priorities = {
            "capacity:local": 0,
            "circuit:orders_db": 1,
            "sli:availability": 2,
            "sli:latency": 3,
        }
        selected = (
            min(
                causes.items(),
                key=lambda item: (
                    -item[1].level,
                    priorities.get(item[0], 99),
                ),
            )[1]
            if causes
            else None
        )
        protection_code = selected.business_code if selected else "OK"
        page_cap = min(
            self.config.query.max_page_size,
            level_config.page_size_cap,
        )
        if capacity_protected:
            page_cap = min(page_cap, self.config.capacity.page_size_cap)
        return ProtectionState(
            degrade_level=level,
            level_name=level_config.name,
            protection_state=(
                "capacity_protected"
                if capacity_protected
                else level_config.name
            ),
            protection_code=protection_code,
            causes=MappingProxyType(dict(causes)),
            circuit_state=circuit_state,
            capacity_protected=capacity_protected,
            writes_paused=capacity_protected,
            page_size_cap=page_cap,
        )

    def _publish_metrics(self, state: ProtectionState) -> None:
        circuit_value = {"closed": 0, "half_open": 1, "open": 2}.get(
            state.circuit_state, 0
        )
        self.metrics.set(
            "order_guard_circuit_state", circuit_value, {"circuit": "orders_db"}
        )
        self.metrics.set("order_guard_degrade_level", state.degrade_level)
        self.metrics.set(
            "order_guard_capacity_protected", 1 if state.capacity_protected else 0
        )
        self.metrics.set(
            "order_guard_protection_active",
            1 if state.capacity_protected else 0,
            {"mode": "capacity"},
        )
        self.metrics.set(
            "order_guard_protection_active",
            1 if state.circuit_state != "closed" else 0,
            {"mode": "circuit"},
        )
        self.metrics.set(
            "order_guard_protection_active",
            1
            if any(key.startswith("sli:") for key in state.causes)
            else 0,
            {"mode": "slo"},
        )
        self.metrics.set(
            "order_guard_query_page_size_limit", state.page_size_cap
        )

    def _publish_tenant_limits(self) -> None:
        for tenant in self.config.tenants:
            limits = self._current_limits(tenant.tenant_id)
            if limits is None:
                continue
            self.metrics.set(
                "order_guard_tenant_limits",
                limits.rate_per_second,
                {
                    "tenant": tenant.tenant_id,
                    "limit": "rate_per_second",
                },
            )
            self.metrics.set(
                "order_guard_tenant_limits",
                limits.burst,
                {
                    "tenant": tenant.tenant_id,
                    "limit": "burst",
                },
            )
            self.metrics.set(
                "order_guard_tenant_limits",
                limits.max_inflight,
                {
                    "tenant": tenant.tenant_id,
                    "limit": "max_inflight",
                },
            )

    def set_limiter_lookup(
        self, lookup: Callable[[str], TenantLimits | None]
    ) -> None:
        self._current_limits = lookup
        self._publish_tenant_limits()

    def _snapshots(self) -> tuple[dict[str, object], dict[str, object]]:
        return self._metrics_snapshot(), self._sli_snapshot()

    @staticmethod
    def _with_transition_states(
        events: list[AuditEvent],
        old: ProtectionState,
        new: ProtectionState,
    ) -> list[AuditEvent]:
        enriched: list[AuditEvent] = []
        for event in events:
            enriched.append(
                AuditEvent(
                    event_type=event.event_type,
                    business_code=event.business_code,
                    reason=event.reason,
                    tenant_id=event.tenant_id,
                    from_state=event.from_state or old.label(),
                    to_state=event.to_state or new.label(),
                    metrics_snapshot=event.metrics_snapshot,
                    thresholds=event.thresholds,
                    sli_snapshot=event.sli_snapshot,
                    decision=event.decision,
                )
            )
        return enriched

    def _commit(
        self,
        old: ProtectionState,
        new: ProtectionState,
        events: list[AuditEvent],
    ) -> None:
        if old == new:
            return
        metrics_snapshot, sli_snapshot = self._snapshots()
        events = [
            AuditEvent(
                event_type=event.event_type,
                business_code=event.business_code,
                reason=event.reason,
                tenant_id=event.tenant_id,
                from_state=event.from_state,
                to_state=event.to_state,
                metrics_snapshot=metrics_snapshot,
                thresholds=event.thresholds,
                sli_snapshot=sli_snapshot,
                decision=event.decision,
            )
            for event in self._with_transition_states(events, old, new)
        ]
        if old.degrade_level != new.degrade_level:
            events.append(
                AuditEvent(
                    event_type="DEGRADE_LEVEL_CHANGED",
                    business_code=new.protection_code,
                    reason="effective_degrade_level_changed",
                    from_state=str(old.degrade_level),
                    to_state=str(new.degrade_level),
                    metrics_snapshot=metrics_snapshot,
                    thresholds={
                        "level": asdict(self.config.degrade_levels[new.degrade_level])
                    },
                    sli_snapshot=sli_snapshot,
                    decision={
                        "previous_level": old.degrade_level,
                        "new_level": new.degrade_level,
                        "mode": new.protection_state,
                    },
                )
            )
        if old.protection_code != new.protection_code:
            events.append(
                AuditEvent(
                    event_type="PROTECTION_CODE_CHANGED",
                    business_code=new.protection_code,
                    reason="effective_protection_code_changed",
                    from_state=old.protection_code,
                    to_state=new.protection_code,
                    metrics_snapshot=metrics_snapshot,
                    thresholds={},
                    sli_snapshot=sli_snapshot,
                    decision={"state": new.to_dict()},
                )
            )
        # The audit transaction must commit before the state snapshot is published.
        self.audit.append_many(events)
        self._state = new
        self._publish_metrics(new)
        for event in events:
            if event.event_type.startswith("CIRCUIT_"):
                circuit_target = {
                    "CIRCUIT_OPENED": "open",
                    "CIRCUIT_REOPENED": "reopened",
                    "CIRCUIT_HALF_OPENED": "half_open",
                    "CIRCUIT_CLOSED": "closed",
                }.get(event.event_type, event.event_type.removeprefix("CIRCUIT_").lower())
                self.metrics.inc(
                    "order_guard_circuit_transitions_total",
                    {"to": circuit_target},
                )
            elif event.event_type.startswith("CAPACITY_"):
                self.metrics.inc(
                    "order_guard_capacity_transitions_total",
                    {"to": event.event_type.removeprefix("CAPACITY_").lower()},
                )
        LOGGER.info(
            "protection transition previous=%s current=%s reason=%s",
            old.label(),
            new.label(),
            events[0].reason if events else "audit_only",
        )

    def update_sli(
        self,
        sli: SLIConfig,
        snapshot: SLISnapshot,
    ) -> bool:
        with self._lock:
            old = self._state
            causes = dict(old.causes)
            cause_key = f"sli:{sli.name}"
            changed = False
            events: list[AuditEvent] = []
            code = (
                "SLO_AVAILABILITY_BUDGET_EXHAUSTED"
                if sli.kind == "availability"
                else "SLO_LATENCY_BUDGET_EXHAUSTED"
            )
            thresholds = {
                "window_seconds": sli.window_seconds,
                "target": sli.target,
                "target_ms": sli.target_ms,
                "allowed_bad_ratio": sli.allowed_bad_ratio,
                "min_samples": sli.min_samples,
                "degrade_level": sli.degrade_level,
                "recover_budget_ratio": sli.recover_budget_ratio,
            }
            if snapshot.breached and cause_key not in causes:
                causes[cause_key] = Cause(
                    kind="sli",
                    level=sli.degrade_level,
                    business_code=code,
                    reason="error_budget_exhausted",
                )
                events.append(
                    AuditEvent(
                        event_type="SLO_BUDGET_EXHAUSTED",
                        business_code=code,
                        reason=f"{sli.name}_error_budget_exhausted",
                        thresholds=thresholds,
                        decision={
                            "sli": sli.name,
                            "action": "degrade",
                            "snapshot": snapshot.to_dict(),
                        },
                    )
                )
                changed = True
            elif (
                not snapshot.breached
                and snapshot.recovered
                and cause_key in causes
            ):
                removed = causes.pop(cause_key)
                events.append(
                    AuditEvent(
                        event_type="SLO_BUDGET_RECOVERED",
                        business_code=f"{code}_RECOVERED",
                        reason=f"{sli.name}_error_budget_recovered",
                        thresholds=thresholds,
                        decision={
                            "sli": sli.name,
                            "action": "recover",
                            "removed_level": removed.level,
                            "snapshot": snapshot.to_dict(),
                        },
                    )
                )
                changed = True
            if not changed:
                return False
            new = self._build_state(
                causes=causes,
                circuit_state=old.circuit_state,
                capacity_protected=old.capacity_protected,
            )
            self._commit(old, new, events)
            return True

    def update_circuit(self, transition: CircuitTransition) -> None:
        with self._lock:
            old = self._state
            causes = dict(old.causes)
            key = "circuit:orders_db"
            if transition.to_state == "closed":
                causes.pop(key, None)
            else:
                causes[key] = Cause(
                    kind="circuit",
                    level=self.config.circuit.open_degrade_level,
                    business_code=self.config.circuit.code,
                    reason=transition.reason,
                )
            new = self._build_state(
                causes=causes,
                circuit_state=transition.to_state,
                capacity_protected=old.capacity_protected,
            )
            event_name = {
                "open": (
                    "CIRCUIT_REOPENED"
                    if transition.from_state == "half_open"
                    else "CIRCUIT_OPENED"
                ),
                "half_open": "CIRCUIT_HALF_OPENED",
                "closed": "CIRCUIT_CLOSED",
            }[transition.to_state]
            business_code = (
                self.config.circuit.code
                if transition.to_state != "closed"
                else f"{self.config.circuit.code}_RECOVERED"
            )
            event = AuditEvent(
                event_type=event_name,
                business_code=business_code,
                reason=transition.reason,
                thresholds=asdict(self.config.circuit),
                decision={
                    "circuit": "orders_db",
                    "from": transition.from_state,
                    "to": transition.to_state,
                    "snapshot": transition.snapshot,
                },
            )
            self._commit(old, new, [event])

    def update_capacity(
        self,
        *,
        protected: bool,
        reason: str,
        sample: dict[str, object],
        thresholds: dict[str, object],
    ) -> bool:
        with self._lock:
            old = self._state
            if old.capacity_protected == protected:
                return False
            causes = dict(old.causes)
            key = "capacity:local"
            if protected:
                causes[key] = Cause(
                    kind="capacity",
                    level=self.config.capacity.degrade_level,
                    business_code=self.config.capacity.code,
                    reason=reason,
                )
                event_type = "CAPACITY_ENTERED"
                business_code = self.config.capacity.code
                action = "pause_writes_and_reduce_page_size"
            else:
                causes.pop(key, None)
                event_type = "CAPACITY_EXITED"
                business_code = f"{self.config.capacity.code}_RECOVERED"
                action = "resume_writes_and_restore_page_size"
            new = self._build_state(
                causes=causes,
                circuit_state=old.circuit_state,
                capacity_protected=protected,
            )
            event = AuditEvent(
                event_type=event_type,
                business_code=business_code,
                reason=reason,
                thresholds=thresholds,
                decision={
                    "action": action,
                    "sample": sample,
                    "effective_page_size_cap": new.page_size_cap,
                },
            )
            self._commit(old, new, [event])
            return True

    def adjust_tenant_limits(
        self,
        *,
        tenant_id: str,
        old_limits: TenantLimits,
        new_limits: TenantLimits,
        apply: Callable[[], None],
    ) -> None:
        with self._lock:
            event = AuditEvent(
                event_type="RATE_LIMIT_ADJUSTED",
                business_code="RATE_LIMIT_ADJUSTED",
                reason="operator_tenant_limit_adjustment",
                tenant_id=tenant_id,
                from_state=(
                    f"rate={old_limits.rate_per_second};burst={old_limits.burst};"
                    f"inflight={old_limits.max_inflight}"
                ),
                to_state=(
                    f"rate={new_limits.rate_per_second};burst={new_limits.burst};"
                    f"inflight={new_limits.max_inflight}"
                ),
                thresholds={},
                decision={
                    "action": "adjust_tenant_limits",
                    "previous": asdict(old_limits),
                    "current": asdict(new_limits),
                },
            )
            metrics_snapshot, sli_snapshot = self._snapshots()
            event = AuditEvent(
                event_type=event.event_type,
                business_code=event.business_code,
                reason=event.reason,
                tenant_id=event.tenant_id,
                from_state=event.from_state,
                to_state=event.to_state,
                metrics_snapshot=metrics_snapshot,
                thresholds=event.thresholds,
                sli_snapshot=sli_snapshot,
                decision=event.decision,
            )
            # Audit commits first; the limiter is published only after success.
            self.audit.append_many([event])
            apply()
            self._publish_tenant_limits()
