from __future__ import annotations

import json
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any

from conftest import ROOT, ServiceHandle, http_json


def test_rate_limit_isolation_and_bypass_paths(service_factory) -> None:
    def mutate(config: dict[str, Any]) -> None:
        config["tenants"][0].update(
            {
                "rate_per_second": 1,
                "burst": 1,
                "max_inflight": 1,
                "enabled": True,
            }
        )
        config["tenants"][1].update(
            {
                "rate_per_second": 50,
                "burst": 20,
                "max_inflight": 4,
                "enabled": True,
            }
        )

    service = service_factory(mutate)
    statuses = [
        service.get(
            "/api/v1/orders?status=created&page=1&page_size=3",
            tenant="tenant-alpha",
        )
        for _ in range(6)
    ]
    limited = [
        item for item in statuses if item[0] == 429
    ]
    assert len(limited) >= 4
    assert limited[0][2]["business_code"] in {
        "TENANT_RATE_LIMITED",
        "TENANT_CONCURRENCY_LIMITED",
    }
    assert "Retry-After" in limited[0][1]

    good_status, _, good_body, good_elapsed = service.get(
        "/api/v1/orders?status=created&page=1&page_size=3",
        tenant="tenant-beta",
    )
    assert good_status == 200
    assert good_body["business_code"] == "OK"
    assert good_elapsed < 500

    disabled_status, _, disabled_body, _ = service.get(
        "/api/v1/orders?status=created", tenant="tenant-disabled"
    )
    assert disabled_status == 403
    assert disabled_body["business_code"] == "TENANT_NOT_ACTIVE"

    health_status, _, health_body, _ = service.get("/healthz")
    assert health_status == 200
    assert health_body["status"] == "ok"

    adjust_status, _, adjust_body, _ = service.admin(
        "PATCH",
        "/admin/tenants/tenant-alpha/limits",
        body={"rate_per_second": 5, "burst": 3},
    )
    assert adjust_status == 200
    assert adjust_body["current"]["rate_per_second"] == 5
    state = service.status()
    assert state["tenants"]["tenant-alpha"]["limits"]["rate_per_second"] == 5
    audit = service.admin(
        "GET", f"/admin/audit?run_id={service.run_id}&limit=1000"
    )[2]["events"]
    assert any(
        event["event_type"] == "RATE_LIMIT_ADJUSTED"
        for event in audit
    )

    metrics_status, _, metrics_text, _ = service.get("/metrics")
    assert metrics_status == 200
    assert "order_guard_rate_limit_rejections_total" in str(metrics_text)


def test_circuit_opens_falls_back_and_half_open_recovers(service_factory) -> None:
    def mutate(config: dict[str, Any]) -> None:
        config["circuit"].update(
            {
                "failure_threshold": 2,
                "timeout_ratio_threshold": 0.5,
                "timeout_min_requests": 2,
                "window_seconds": 5,
                "open_seconds": 1.0,
                "open_degrade_level": 2,
            }
        )
        config["database"]["operation_timeout_ms"] = 100
        config["capacity"]["thread_occupancy_threshold"] = 1.0
        config["capacity"]["queue_depth_threshold"] = 100
        config["capacity"]["enter_after_samples"] = 10

    service = service_factory(mutate)
    path = "/api/v1/orders?status=created&page=1&page_size=3"
    warm_status, _, warm_body, _ = service.get(path, tenant="tenant-alpha")
    assert warm_status == 200
    assert warm_body["rows"]

    status, _, body, _ = service.admin(
        "POST",
        "/admin/faults",
        body={"mode": "slow", "delay_ms": 250, "duration_seconds": 1.5},
    )
    assert status == 200 and body["fault"]["mode"] == "slow"

    fallback_statuses = []
    for _ in range(2):
        fallback_statuses.append(
            service.get(path, tenant="tenant-alpha")
        )
    assert all(item[0] == 200 for item in fallback_statuses)
    assert fallback_statuses[-1][2]["fallback"]["source"] in {
        "cache",
        "slim_snapshot",
    }

    status, _, body, _ = service.get(path, tenant="tenant-alpha")
    assert status == 200
    assert body["business_code"] == "ORDERS_DB_CIRCUIT_OPEN"
    assert body["fallback"]["source"] in {"cache", "slim_snapshot"}

    time.sleep(1.3)
    status, _, body, _ = service.get(path, tenant="tenant-alpha")
    assert status == 200
    assert body["business_code"] in {"OK", "DATABASE_DEGRADED"}
    state = service.status()
    assert state["circuit"]["state"] == "closed"
    audit = service.admin(
        "GET", f"/admin/audit?run_id={service.run_id}&limit=1000"
    )[2]["events"]
    event_types = {event["event_type"] for event in audit}
    assert "CIRCUIT_OPENED" in event_types
    assert "CIRCUIT_HALF_OPENED" in event_types
    assert "CIRCUIT_CLOSED" in event_types


def test_capacity_protection_pauses_writes_and_auto_recovers(service_factory) -> None:
    def mutate(config: dict[str, Any]) -> None:
        config["service"]["business_threads"] = 8
        config["capacity"].update(
            {
                "worker_threads": 2,
                "thread_occupancy_threshold": 0.5,
                "queue_depth_threshold": 1,
                "sqlite_busy_events_threshold": 2,
                "enter_after_samples": 2,
                "recover_thread_occupancy": 0.2,
                "recover_queue_depth": 0,
                "recover_after_samples": 2,
                "sample_interval_ms": 100,
                "page_size_cap": 5,
            }
        )
        config["database"]["operation_timeout_ms"] = 3000
        config["circuit"]["failure_threshold"] = 100
        config["circuit"]["timeout_min_requests"] = 100
        config["slis"]["latency"]["min_samples"] = 100
        config["slis"]["availability"]["min_samples"] = 100

    service = service_factory(mutate)
    status, _, body, _ = service.admin(
        "POST",
        "/admin/faults",
        body={"mode": "slow", "delay_ms": 900, "duration_seconds": 3},
    )
    assert status == 200
    path = "/api/v1/orders?status=created&page=1&page_size=20"
    with ThreadPoolExecutor(max_workers=9) as pool:
        futures = [
            pool.submit(service.get, path, tenant=tenant)
            for tenant in (
                ["tenant-alpha"] * 3
                + ["tenant-beta"] * 3
                + ["tenant-gamma"] * 3
            )
        ]
        deadline = time.monotonic() + 4
        protected = False
        while time.monotonic() < deadline:
            state = service.status()
            if state["protection"]["capacity_protected"]:
                protected = True
                break
            time.sleep(0.05)
        assert protected is True
        assert service.status()["protection"]["page_size_cap"] == 5
        health_status, _, health_body, health_elapsed = service.get("/healthz")
        assert health_status == 200
        assert health_body["status"] == "ok"
        assert health_elapsed < 500
        write_status, _, write_body, _ = http_json(
            "POST",
            service.base_url + "/api/v1/orders",
            body={"amount_cents": 1000},
            headers={"X-Tenant-Id": "tenant-beta"},
        )
        assert write_status == 503
        assert write_body["business_code"] == "CAPACITY_PROTECTED", (
            write_status,
            write_body,
        )
        for future in futures:
            future.result(timeout=5)

    service.admin("DELETE", "/admin/faults")
    deadline = time.monotonic() + 6
    recovered = False
    while time.monotonic() < deadline:
        state = service.status()
        if not state["protection"]["capacity_protected"]:
            recovered = True
            break
        time.sleep(0.1)
    assert recovered is True
    audit = service.admin(
        "GET", f"/admin/audit?run_id={service.run_id}&limit=1000"
    )[2]["events"]
    event_types = {event["event_type"] for event in audit}
    assert "CAPACITY_ENTERED" in event_types
    assert "CAPACITY_EXITED" in event_types


def test_sli_breach_recovery_and_metric_contract(service_factory) -> None:
    def mutate(config: dict[str, Any]) -> None:
        config["slis"]["availability"].update(
            {
                "window_seconds": 1.2,
                "target": 0.9,
                "min_samples": 2,
                "degrade_level": 3,
                "recover_budget_ratio": 0.2,
            }
        )
        config["slis"]["latency"].update(
            {
                "window_seconds": 1.2,
                "target_ms": 10000,
                "min_samples": 100,
                "degrade_level": 1,
            }
        )
        config["sli_evaluation_interval_ms"] = 50

    service = service_factory(mutate)
    for _ in range(2):
        status, _, body, _ = service.get(
            "/api/v1/orders?status=invalid", tenant="tenant-alpha"
        )
        assert status == 422
    deadline = time.monotonic() + 3
    breached = False
    while time.monotonic() < deadline:
        state = service.status()
        if state["protection"]["degrade_level"] == 3:
            breached = True
            break
        time.sleep(0.05)
    assert breached is True
    status, headers, body, _ = service.get(
        "/api/v1/orders?status=created&page=1&page_size=2",
        tenant="tenant-alpha",
    )
    assert status == 200
    assert body["protection_code"] == "SLO_AVAILABILITY_BUDGET_EXHAUSTED"
    assert headers["X-Protection-Code"] == body["protection_code"]

    time.sleep(1.4)
    for _ in range(2):
        service.get(
            "/api/v1/orders?status=created&page=1&page_size=2",
            tenant="tenant-alpha",
        )
    deadline = time.monotonic() + 3
    recovered = False
    while time.monotonic() < deadline:
        state = service.status()
        if state["protection"]["degrade_level"] == 0:
            recovered = True
            break
        time.sleep(0.05)
    assert recovered is True

    status, _, metrics, _ = http_json("GET", service.base_url + "/metrics")
    assert status == 200
    for name in (
        "order_guard_rate_limit_rejections_total",
        "order_guard_inflight_requests",
        "order_guard_circuit_state",
        "order_guard_degrade_level",
        "order_guard_backlog_depth",
        "order_guard_sqlite_busy_events_total",
        "order_guard_sli_value",
        "order_guard_error_budget_consumed_ratio",
    ):
        assert name in str(metrics)
    state = service.status()
    assert state["slis"]["availability"]["value"] is not None


def test_sqlite_busy_signal_enters_and_exits_capacity_protection(
    service_factory,
) -> None:
    def mutate(config: dict[str, Any]) -> None:
        config["capacity"].update(
            {
                "worker_threads": 8,
                "thread_occupancy_threshold": 1.0,
                "queue_depth_threshold": 100,
                "sqlite_busy_events_threshold": 1,
                "enter_after_samples": 1,
                "recover_thread_occupancy": 0.9,
                "recover_queue_depth": 99,
                "recover_after_samples": 2,
                "sample_interval_ms": 50,
                "page_size_cap": 5,
            }
        )
        config["circuit"]["failure_threshold"] = 100
        config["circuit"]["timeout_min_requests"] = 100
        config["slis"]["latency"]["min_samples"] = 100
        config["slis"]["availability"]["min_samples"] = 100

    service = service_factory(mutate)
    service.admin(
        "POST",
        "/admin/faults",
        body={"mode": "busy", "error_rate": 1.0, "duration_seconds": 2},
    )
    path = "/api/v1/orders?status=created&page=1&page_size=3"
    for _ in range(3):
        status, _, body, _ = service.get(path, tenant="tenant-alpha")
        assert status == 200
        assert body["fallback"]["source"] in {"cache", "slim_snapshot"}

    deadline = time.monotonic() + 3
    protected = False
    while time.monotonic() < deadline:
        state = service.status()
        if state["protection"]["capacity_protected"]:
            protected = True
            break
        time.sleep(0.05)
    assert protected is True
    metrics = service.get("/metrics")[2]
    assert "order_guard_sqlite_busy_events_total" in str(metrics)
    assert service.status()["metrics"]["sqlite_busy_events_total"] >= 1

    service.admin("DELETE", "/admin/faults")
    deadline = time.monotonic() + 3
    recovered = False
    while time.monotonic() < deadline:
        state = service.status()
        if not state["protection"]["capacity_protected"]:
            recovered = True
            break
        time.sleep(0.05)
    assert recovered is True
