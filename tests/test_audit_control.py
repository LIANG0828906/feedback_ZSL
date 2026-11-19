from __future__ import annotations

import json
from pathlib import Path

import pytest

from order_guard.audit import AuditWriter
from order_guard.config import load_config
from order_guard.control import ControlPlane
from order_guard.limiter import TenantLimits
from order_guard.metrics import Metrics


ROOT = Path(__file__).resolve().parents[1]


def build_control(tmp_path: Path) -> tuple[ControlPlane, AuditWriter, Metrics]:
    config = load_config(ROOT / "config" / "service.json")
    audit = AuditWriter(tmp_path / "audit.db", "control-test-run")
    metrics = Metrics()
    metrics.declare_defaults(tenant.tenant_id for tenant in config.tenants)
    control = ControlPlane(
        config,
        audit,
        metrics,
        metrics_snapshot=lambda: {"inflight_total": 2, "backlog_depth": 1},
        sli_snapshot=lambda: {"availability": {"error_budget_consumed": 0.1}},
    )
    control.set_limiter_lookup(
        lambda _: TenantLimits(rate_per_second=10, burst=2, max_inflight=1)
    )
    control.start()
    return control, audit, metrics


def test_failed_audit_write_does_not_publish_capacity_state(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    control, _, _ = build_control(tmp_path)

    def fail(_events):
        raise RuntimeError("audit unavailable")

    monkeypatch.setattr(control.audit, "append_many", fail)
    with pytest.raises(RuntimeError):
        control.update_capacity(
            protected=True,
            reason="test",
            sample={"queue_depth": 20},
            thresholds={"queue_depth_threshold": 10},
        )
    assert control.state.capacity_protected is False
    assert control.state.writes_paused is False


def test_transitions_and_limit_adjustment_are_auditable(tmp_path: Path) -> None:
    control, audit, metrics = build_control(tmp_path)
    assert control.update_capacity(
        protected=True,
        reason="queue_depth",
        sample={"queue_depth": 9},
        thresholds={"queue_depth_threshold": 6},
    )
    assert control.state.capacity_protected is True
    assert control.state.writes_paused is True
    assert metrics.get_gauge("order_guard_capacity_protected") == 1

    old = TenantLimits(rate_per_second=10, burst=2, max_inflight=1)
    new = TenantLimits(rate_per_second=5, burst=1, max_inflight=1)
    control.adjust_tenant_limits(
        tenant_id="tenant-alpha",
        old_limits=old,
        new_limits=new,
        apply=lambda: None,
    )
    events = audit.query(run_id="control-test-run")
    event_types = {event["event_type"] for event in events}
    assert "CAPACITY_ENTERED" in event_types
    assert "RATE_LIMIT_ADJUSTED" in event_types
    assert any(
        event["metrics_snapshot"] for event in events
    )
    assert any(
        event["sli_snapshot"] for event in events
    )
