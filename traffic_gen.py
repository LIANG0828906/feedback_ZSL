from __future__ import annotations

import argparse
import asyncio
import json
import os
import random
import subprocess
import sys
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable
from urllib.parse import urlparse

import httpx

from gateway.metrics import percentile

PROJECT_ROOT = Path(__file__).resolve().parent

FEATURE_NAMES = {
    "ranking-features-v1": [
        "user_age",
        "item_age",
        "ctr_7d",
        "cvr_7d",
        "price",
        "category_affinity",
        "freshness",
        "query_match",
    ],
    "ranking-features-v2": [
        "user_age",
        "item_age",
        "price",
        "category_affinity",
        "ctr_7d",
        "cvr_7d",
        "discount",
        "delivery_eta",
        "freshness",
        "query_match",
    ],
}


@dataclass
class Outcome:
    sequence: int
    phase: str
    tenant: str
    client_id: str
    declared_protocol: str
    status_code: int | None
    model_version: str | None
    error_code: str | None
    error_field: str | None
    latency_ms: float
    timeout: bool = False
    transport_error: bool = False

    @property
    def error(self) -> bool:
        return self.transport_error or self.status_code is None or self.status_code >= 400


@dataclass
class RunContext:
    scenario: dict[str, Any]
    base_url: str
    run_started_epoch: float
    outcomes: list[Outcome] = field(default_factory=list)
    fault_events: list[dict[str, Any]] = field(default_factory=list)
    reload_checks: list[dict[str, Any]] = field(default_factory=list)
    hot_switch: list[dict[str, Any]] = field(default_factory=list)
    probes: list[Outcome] = field(default_factory=list)
    rollback_latency_ms: float | None = None


def _request_features(
    rng: random.Random,
    feature_protocol: str,
    swap_indices: tuple[int, int] | None = None,
) -> tuple[list[str], list[float]]:
    names = list(FEATURE_NAMES[feature_protocol])
    values = [round(rng.uniform(0.01, 0.99), 6) for _ in names]
    if swap_indices is not None:
        first, second = swap_indices
        names[first], names[second] = names[second], names[first]
        values[first], values[second] = values[second], values[first]
    return names, values


async def _post_json(
    client: httpx.AsyncClient, path: str, payload: dict[str, Any]
) -> tuple[int, dict[str, Any]]:
    response = await client.post(path, json=payload)
    try:
        body = response.json()
    except ValueError:
        body = {"raw": response.text}
    return response.status_code, body


async def _get_state(client: httpx.AsyncClient) -> dict[str, Any]:
    response = await client.get("/state")
    response.raise_for_status()
    return response.json()


async def _wait_for_gateway(client: httpx.AsyncClient, timeout: float = 20.0) -> None:
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            response = await client.get("/state")
            if response.status_code == 200:
                state = response.json()
                versions = state.get("versions", {})
                if versions and all(
                    item.get("load_state") in {"READY", "FAILED"}
                    for item in versions.values()
                ):
                    return
        except Exception as exc:
            last_error = exc
        await asyncio.sleep(0.05)
    raise RuntimeError(f"gateway did not become ready: {last_error}")


async def _send_sample(
    client: httpx.AsyncClient,
    *,
    sequence: int,
    phase: str,
    tenant: str,
    client_id: str,
    declared_protocol: str,
    feature_protocol: str,
    swap_indices: tuple[int, int] | None,
    rng: random.Random,
    request_timeout: float,
) -> Outcome:
    names, values = _request_features(rng, feature_protocol, swap_indices)
    request_id = f"{phase}-{sequence:06d}"
    payload = {
        "request_id": request_id,
        "tenant_id": tenant,
        "client_id": client_id,
        "protocol_version": declared_protocol,
        "feature_names": names,
        "features": values,
    }
    started = time.perf_counter()
    try:
        response = await client.post("/predict", json=payload, timeout=request_timeout)
        latency_ms = (time.perf_counter() - started) * 1000.0
        try:
            body = response.json()
        except ValueError:
            body = {}
        if response.status_code < 400:
            return Outcome(
                sequence=sequence,
                phase=phase,
                tenant=tenant,
                client_id=client_id,
                declared_protocol=declared_protocol,
                status_code=response.status_code,
                model_version=body.get("model_version"),
                error_code=None,
                error_field=None,
                latency_ms=latency_ms,
            )
        error = body.get("error", {})
        return Outcome(
            sequence=sequence,
            phase=phase,
            tenant=tenant,
            client_id=client_id,
            declared_protocol=declared_protocol,
            status_code=response.status_code,
            model_version=error.get("version"),
            error_code=error.get("code"),
            error_field=error.get("field"),
            latency_ms=latency_ms,
            timeout=error.get("code") == "INFERENCE_TIMEOUT",
        )
    except httpx.TimeoutException:
        return Outcome(
            sequence=sequence,
            phase=phase,
            tenant=tenant,
            client_id=client_id,
            declared_protocol=declared_protocol,
            status_code=None,
            model_version=None,
            error_code="CLIENT_TIMEOUT",
            error_field=None,
            latency_ms=(time.perf_counter() - started) * 1000.0,
            timeout=True,
            transport_error=True,
        )
    except Exception as exc:
        return Outcome(
            sequence=sequence,
            phase=phase,
            tenant=tenant,
            client_id=client_id,
            declared_protocol=declared_protocol,
            status_code=None,
            model_version=None,
            error_code=f"TRANSPORT_{type(exc).__name__.upper()}",
            error_field=None,
            latency_ms=(time.perf_counter() - started) * 1000.0,
            transport_error=True,
        )


async def _run_phase(
    context: RunContext,
    client: httpx.AsyncClient,
    *,
    name: str,
    tenant: str,
    clients: list[str],
    declared_protocol: str,
    feature_protocol: str | None = None,
    swap_indices: tuple[int, int] | None = None,
    count: int,
    rate_per_second: float,
    http_timeout: float,
    seed_offset: int,
    concurrency: int = 2,
) -> list[Outcome]:
    if count <= 0:
        return []
    feature_protocol = feature_protocol or declared_protocol
    outcomes: list[Outcome | None] = [None] * count
    cursor = 0
    cursor_lock = asyncio.Lock()
    start_time = asyncio.get_running_loop().time()
    interval = 1.0 / max(rate_per_second, 0.001)

    async def worker() -> None:
        nonlocal cursor
        while True:
            async with cursor_lock:
                index = cursor
                cursor += 1
            if index >= count:
                return
            scheduled = start_time + index * interval
            delay = scheduled - asyncio.get_running_loop().time()
            if delay > 0:
                await asyncio.sleep(delay)
            selected_client = clients[index % len(clients)]
            local_rng = random.Random(
                (int(context.scenario["seed"]) + seed_offset) * 1_000_003 + index
            )
            outcomes[index] = await _send_sample(
                client,
                sequence=index,
                phase=name,
                tenant=tenant,
                client_id=selected_client,
                declared_protocol=declared_protocol,
                feature_protocol=feature_protocol,
                swap_indices=swap_indices,
                rng=local_rng,
                request_timeout=http_timeout,
            )

    await asyncio.gather(*(worker() for _ in range(max(1, concurrency))))
    completed = [item for item in outcomes if item is not None]
    context.outcomes.extend(completed)
    return completed


async def _set_weights(
    client: httpx.AsyncClient,
    weights: dict[str, float],
    *,
    tenant: str | None = None,
    client_id: str | None = None,
) -> None:
    payload: dict[str, Any] = {"weights": weights}
    if tenant:
        payload["tenant_id"] = tenant
    if client_id:
        payload["client_id"] = client_id
    status, body = await _post_json(client, "/admin/weights", payload)
    if status != 200:
        raise RuntimeError(f"weight update failed ({status}): {body}")


def _rollback_records(state: dict[str, Any]) -> list[dict[str, Any]]:
    return list(state.get("rollback", {}).get("recent", []))


async def _wait_rollback_and_probe(
    context: RunContext,
    client: httpx.AsyncClient,
    rollback_config: dict[str, Any],
    http_timeout: float,
    baseline_count: int,
) -> tuple[list[dict[str, Any]], list[Outcome]]:
    deadline = time.monotonic() + float(rollback_config.get("timeout_seconds", 30))
    probe_count = int(rollback_config.get("probe_count", 40))
    probe_interval = float(rollback_config.get("probe_interval_ms", 50)) / 1000.0
    probe_outcomes: list[Outcome | None] = [None] * probe_count
    trigger_epoch: float | None = None

    async def probes() -> None:
        for index in range(probe_count):
            if index:
                await asyncio.sleep(probe_interval)
            local_rng = random.Random(context.scenario["seed"] + 9000 + index)
            probe_outcomes[index] = await _send_sample(
                client,
                sequence=index,
                phase="rollback_probe",
                tenant=str(rollback_config["probe_tenant_id"]),
                client_id=str(rollback_config["probe_client_id"]),
                declared_protocol=str(rollback_config["probe_protocol_version"]),
                feature_protocol=str(rollback_config["probe_protocol_version"]),
                swap_indices=None,
                rng=local_rng,
                request_timeout=http_timeout,
            )

    async def poll() -> None:
        nonlocal trigger_epoch
        while time.monotonic() < deadline:
            state = await _get_state(client)
            if int(state.get("rollback", {}).get("count", 0)) > baseline_count:
                records = _rollback_records(state)
                if records:
                    trigger_epoch = float(records[-1].get("rollback_at", time.time()))
                return
            await asyncio.sleep(0.05)

    await asyncio.gather(probes(), poll())
    completed = [item for item in probe_outcomes if item is not None]
    context.probes.extend(completed)
    await asyncio.sleep(0.1)
    final_state = await _get_state(client)
    records = [
        item
        for item in _rollback_records(final_state)
        if float(item.get("rollback_at", 0)) >= context.run_started_epoch
    ]
    if trigger_epoch is not None:
        context.rollback_latency_ms = round(
            max(0.0, trigger_epoch - context.run_started_epoch) * 1000.0, 3
        )
    return records, completed


async def run_scenario(
    base_url: str, scenario: dict[str, Any], report_path: Path
) -> dict[str, Any]:
    started_epoch = time.time()
    started_monotonic = time.perf_counter()
    context = RunContext(
        scenario=scenario,
        base_url=base_url,
        run_started_epoch=started_epoch,
    )
    rate = float(scenario.get("rate_per_second", 50))
    http_timeout = float(scenario.get("http_timeout_seconds", 5))

    async with httpx.AsyncClient(base_url=base_url, timeout=http_timeout) as client:
        await _wait_for_gateway(client)

        stable = scenario["stable"]
        await _set_weights(client, {"v3": 1.0, "v4": 0.0}, tenant=stable["tenant_id"])
        await _run_phase(
            context,
            client,
            name="stable_baseline",
            tenant=stable["tenant_id"],
            clients=list(stable["clients"]),
            declared_protocol=stable["protocol_version"],
            count=int(stable["count"]),
            rate_per_second=rate,
            http_timeout=http_timeout,
            seed_offset=100,
        )

        hot = scenario["hot_switch"]
        for target_version, protocol in (
            ("v3", "ranking-features-v1"),
            ("v4", "ranking-features-v2"),
            ("v3", "ranking-features-v1"),
        ):
            weights = {"v3": 1.0, "v4": 0.0} if target_version == "v3" else {"v3": 0.0, "v4": 1.0}
            await _set_weights(client, weights, tenant=hot["tenant_id"])
            result = await _run_phase(
                context,
                client,
                name=f"hot_switch_to_{target_version}",
                tenant=hot["tenant_id"],
                clients=[hot["client_id"]],
                declared_protocol=protocol,
                count=int(hot.get("samples", 2)),
                rate_per_second=rate,
                http_timeout=http_timeout,
                seed_offset=200 + int(target_version[-1]),
                concurrency=1,
            )
            context.hot_switch.append(
                {
                    "target_version": target_version,
                    "client_id": hot["client_id"],
                    "requests": len(result),
                    "successes": sum(1 for item in result if not item.error),
                    "observed_versions": sorted(
                        {item.model_version for item in result if item.model_version}
                    ),
                    "error_codes": sorted(
                        {item.error_code for item in result if item.error_code}
                    ),
                }
            )

        canary = scenario["canary"]
        await _run_phase(
            context,
            client,
            name="canary_healthy",
            tenant=canary["tenant_id"],
            clients=list(canary["clients"]),
            declared_protocol=canary["protocol_version"],
            count=int(canary["count"]),
            rate_per_second=rate,
            http_timeout=http_timeout,
            seed_offset=300,
        )

        load_failure = scenario["load_failure"]
        load_version = str(load_failure["version"])
        status, body = await _post_json(
            client, "/admin/faults", {"fail_next_load": [load_version]}
        )
        if status != 200:
            raise RuntimeError(f"could not queue load failure: {status} {body}")
        reload_status, reload_body = await _post_json(
            client, f"/admin/reload/{load_version}?wait=true", {}
        )
        stable_probe = await _run_phase(
            context,
            client,
            name="stable_after_load_failure",
            tenant=load_failure["stable_tenant_id"],
            clients=[load_failure["stable_client_id"]],
            declared_protocol="ranking-features-v1",
            count=1,
            rate_per_second=rate,
            http_timeout=http_timeout,
            seed_offset=400,
            concurrency=1,
        )
        state_after_failure = await _get_state(client)
        context.reload_checks.append(
            {
                "version": load_version,
                "reload_status": reload_status,
                "reload_error_code": reload_body.get("error_code"),
                "load_state": state_after_failure["versions"][load_version].get("load_state"),
                "ready": state_after_failure["versions"][load_version].get("ready"),
                "last_load_error_code": state_after_failure["versions"][load_version].get(
                    "last_load_error_code"
                ),
                "stable_probe_success": bool(stable_probe and not stable_probe[0].error),
            }
        )

        rollback = scenario["rollback"]
        state_before_fault = await _get_state(client)
        baseline_rollback_count = int(
            state_before_fault.get("rollback", {}).get("count", 0)
        )
        await _set_weights(
            client,
            {"v3": 1.0, "v4": 0.0},
            tenant=rollback["probe_tenant_id"],
        )
        delay_ms = float(rollback["inference_delay_ms"])
        status, body = await _post_json(
            client,
            "/admin/faults",
            {"inference_delay_ms": {load_version: delay_ms}},
        )
        if status != 200:
            raise RuntimeError(f"could not inject inference delay: {status} {body}")
        context.fault_events.append(
            {
                "type": "inference_delay",
                "version": load_version,
                "delay_ms": delay_ms,
                "at": round(time.time(), 6),
            }
        )
        fault_started = time.time()
        fault_outcomes = await _run_phase(
            context,
            client,
            name="canary_latency_fault",
            tenant=rollback["tenant_id"],
            clients=list(rollback["clients"]),
            declared_protocol=rollback["protocol_version"],
            count=int(rollback["count"]),
            rate_per_second=float(rollback.get("send_rate_per_second", rate)),
            http_timeout=http_timeout,
            seed_offset=500,
            concurrency=int(rollback.get("concurrency", 1)),
        )
        rollback_records, probes = await _wait_rollback_and_probe(
            context,
            client,
            rollback,
            http_timeout,
            baseline_rollback_count,
        )
        if rollback_records:
            rollback_record = rollback_records[-1]
            rollback_epoch = float(rollback_record.get("rollback_at", fault_started))
            context.rollback_latency_ms = round(
                max(0.0, rollback_epoch - fault_started) * 1000.0, 3
            )
        context.fault_events.append(
            {
                "type": "rollback_observed",
                "version": load_version,
                "count": len(rollback_records),
                "at": round(time.time(), 6),
            }
        )
        clear_status, clear_body = await _post_json(
            client, "/admin/faults", {"clear": True}
        )
        if clear_status != 200:
            raise RuntimeError(f"could not clear faults: {clear_status} {clear_body}")

        protocol_checks = scenario["protocol_checks"]
        model_mismatch = await _run_phase(
            context,
            client,
            name="model_protocol_mismatch",
            tenant=protocol_checks["tenant_id"],
            clients=[protocol_checks["client_id"]],
            declared_protocol=protocol_checks["model_mismatch"][
                "declared_protocol_version"
            ],
            feature_protocol=protocol_checks["model_mismatch"][
                "feature_protocol_version"
            ],
            count=1,
            rate_per_second=rate,
            http_timeout=http_timeout,
            seed_offset=600,
            concurrency=1,
        )
        field_mismatch = await _run_phase(
            context,
            client,
            name="field_order_mismatch",
            tenant=protocol_checks["tenant_id"],
            clients=[protocol_checks["client_id"]],
            declared_protocol=protocol_checks["field_order_mismatch"][
                "declared_protocol_version"
            ],
            feature_protocol=protocol_checks["field_order_mismatch"][
                "feature_protocol_version"
            ],
            swap_indices=tuple(
                protocol_checks["field_order_mismatch"].get("swap_indices", ())
            )
            if protocol_checks["field_order_mismatch"].get("swap_indices")
            else None,
            count=1,
            rate_per_second=rate,
            http_timeout=http_timeout,
            seed_offset=601,
            concurrency=1,
        )

        final_state = await _get_state(client)

    finished_epoch = time.time()
    duration_ms = (time.perf_counter() - started_monotonic) * 1000.0
    report = _build_report(
        context,
        final_state,
        started_epoch=started_epoch,
        finished_epoch=finished_epoch,
        duration_ms=duration_ms,
        fault_outcomes=fault_outcomes,
        model_mismatch=model_mismatch,
        field_mismatch=field_mismatch,
        rollback_records=rollback_records,
        probes=probes,
    )
    report_path.parent.mkdir(parents=True, exist_ok=True)
    report_path.write_text(
        json.dumps(report, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    return report


def _summary_for(outcomes: Iterable[Outcome]) -> dict[str, Any]:
    rows = list(outcomes)
    latencies = [item.latency_ms for item in rows]
    errors = sum(1 for item in rows if item.error)
    timeouts = sum(1 for item in rows if item.timeout)
    return {
        "requests": len(rows),
        "errors": errors,
        "timeouts": timeouts,
        "error_rate": round(errors / len(rows), 6) if rows else 0.0,
        "p50_ms": round(percentile(latencies, 0.50), 3) if rows else 0.0,
        "p95_ms": round(percentile(latencies, 0.95), 3) if rows else 0.0,
        "p99_ms": round(percentile(latencies, 0.99), 3) if rows else 0.0,
    }


def _group_summary(
    outcomes: Iterable[Outcome], key_name: str
) -> dict[str, dict[str, Any]]:
    groups: dict[str, list[Outcome]] = {}
    for item in outcomes:
        key = getattr(item, key_name) or "unknown"
        groups.setdefault(str(key), []).append(item)
    return {key: _summary_for(rows) for key, rows in sorted(groups.items())}


def _build_report(
    context: RunContext,
    state: dict[str, Any],
    *,
    started_epoch: float,
    finished_epoch: float,
    duration_ms: float,
    fault_outcomes: list[Outcome],
    model_mismatch: list[Outcome],
    field_mismatch: list[Outcome],
    rollback_records: list[dict[str, Any]],
    probes: list[Outcome],
) -> dict[str, Any]:
    all_outcomes = context.outcomes
    status_counts: dict[str, int] = {}
    error_code_counts: dict[str, int] = {}
    for item in all_outcomes:
        status_key = str(item.status_code) if item.status_code is not None else "transport"
        status_counts[status_key] = status_counts.get(status_key, 0) + 1
        if item.error_code:
            error_code_counts[item.error_code] = error_code_counts.get(item.error_code, 0) + 1

    version_stats = _group_summary(all_outcomes, "model_version")
    tenant_stats = _group_summary(all_outcomes, "tenant")
    phase_stats = _group_summary(all_outcomes, "phase")
    stable_versions = [
        item for item in all_outcomes if item.phase.startswith("stable")
    ]
    stable_summary = _summary_for(stable_versions)
    probe_summary = _summary_for(probes)
    hot_switch_ok = all(
        item["successes"] == item["requests"]
        and item["observed_versions"] == [item["target_version"]]
        for item in context.hot_switch
    )
    load_failure_ok = bool(context.reload_checks) and all(
        item["reload_status"] == 409
        and item["reload_error_code"] == "MODEL_LOAD_FAILED"
        and item["ready"]
        and item["stable_probe_success"]
        for item in context.reload_checks
    )
    protocol_checks = {
        "model_mismatch": {
            "status_code": model_mismatch[0].status_code if model_mismatch else None,
            "error_code": model_mismatch[0].error_code if model_mismatch else None,
            "field": model_mismatch[0].error_field if model_mismatch else None,
            "model_version": model_mismatch[0].model_version if model_mismatch else None,
        },
        "field_order_mismatch": {
            "status_code": field_mismatch[0].status_code if field_mismatch else None,
            "error_code": field_mismatch[0].error_code if field_mismatch else None,
            "field": field_mismatch[0].error_field if field_mismatch else None,
            "model_version": field_mismatch[0].model_version if field_mismatch else None,
        },
    }
    rollback_events = []
    for item in rollback_records:
        rollback_events.append(
            {
                "version": item.get("version"),
                "fallback_to": item.get("fallback_to"),
                "trigger_metric": item.get("trigger_metric"),
                "observed": item.get("observed"),
                "threshold": item.get("threshold"),
                "window": item.get("window"),
                "rollback_at": item.get("rollback_at"),
                "rollback_time_iso": item.get("rollback_time_iso"),
            }
        )
    rollback_within_budget = (
        context.rollback_latency_ms is not None
        and context.rollback_latency_ms <= 30_000
    )
    rollback_trigger_valid = any(
        item.get("trigger_metric") in {"p99_latency_ms", "error_rate"}
        for item in rollback_events
    )
    availability_ok = bool(probes) and all(
        item.status_code == 200 for item in probes
    )
    report = {
        "schema_version": 1,
        "scenario_seed": context.scenario["seed"],
        "base_url": context.base_url,
        "started_at": _iso(started_epoch),
        "finished_at": _iso(finished_epoch),
        "duration_ms": round(duration_ms, 3),
        "requests": {
            "total": len(all_outcomes),
            "succeeded": sum(1 for item in all_outcomes if not item.error),
            "failed": sum(1 for item in all_outcomes if item.error),
            "timeouts": sum(1 for item in all_outcomes if item.timeout),
            "by_status": dict(sorted(status_counts.items())),
            "by_model_version": {
                key: value["requests"] for key, value in version_stats.items()
            },
            "by_tenant": {
                key: value["requests"] for key, value in tenant_stats.items()
            },
        },
        "versions": version_stats,
        "tenants": tenant_stats,
        "phases": phase_stats,
        "error_codes": dict(sorted(error_code_counts.items())),
        "stable_p95_within_30ms": stable_summary["p95_ms"] <= 30.0,
        "stable_latency": stable_summary,
        "rollback": {
            "count": len(rollback_events),
            "latency_ms": context.rollback_latency_ms,
            "within_30s": rollback_within_budget,
            "events": rollback_events,
            "active_versions": state.get("rollback", {}).get("active_versions", []),
        },
        "availability_during_rollback": {
            **probe_summary,
            "all_probes_succeeded": availability_ok,
        },
        "hot_update": {
            "same_client_reassigned": hot_switch_ok,
            "checks": context.hot_switch,
        },
        "load_failure": {
            "isolated": load_failure_ok,
            "checks": context.reload_checks,
        },
        "protocol_validation": protocol_checks,
        "faults": {
            "injected": context.fault_events,
            "fault_injected_requests": {
                "requests": len(fault_outcomes),
                "versions": sorted(
                    {item.model_version for item in fault_outcomes if item.model_version}
                ),
                "p95_ms": _summary_for(fault_outcomes)["p95_ms"],
                "p99_ms": _summary_for(fault_outcomes)["p99_ms"],
            },
        },
        "gateway_state": {
            "stable_version": state.get("stable_version"),
            "routing_weights": state.get("routing", {}).get("weights"),
            "versions": {
                version: {
                    "load_state": item.get("load_state"),
                    "ready": item.get("ready"),
                    "protocol_version": item.get("protocol_version"),
                    "last_load_error_code": item.get("last_load_error_code"),
                }
                for version, item in state.get("versions", {}).items()
            },
            "metrics": state.get("metrics", {}),
        },
        "acceptance": {
            "dual_version_serving": any(
                value["requests"] > 0 for value in version_stats.values()
            )
            and len([key for key in version_stats if key != "unknown"]) >= 2,
            "protocol_checks_pass": protocol_checks["model_mismatch"]["status_code"]
            == 422
            and protocol_checks["model_mismatch"]["error_code"]
            == "MODEL_PROTOCOL_MISMATCH"
            and protocol_checks["field_order_mismatch"]["status_code"] == 422
            and protocol_checks["field_order_mismatch"]["error_code"]
            == "FEATURE_ORDER_MISMATCH",
            "hot_update_pass": hot_switch_ok,
            "load_failure_isolated": load_failure_ok,
            "rollback_pass": len(rollback_events) >= 1
            and rollback_within_budget
            and rollback_trigger_valid
            and state.get("routing", {}).get("weights", {}).get("v4") == 0.0
            and availability_ok,
            "stable_latency_pass": stable_summary["p95_ms"] <= 30.0,
        },
    }
    return report


def _iso(epoch: float) -> str:
    return datetime.fromtimestamp(epoch, tz=timezone.utc).isoformat().replace("+00:00", "Z")


def deterministic_projection(value: Any) -> Any:
    if isinstance(value, dict):
        result: dict[str, Any] = {}
        for key, item in value.items():
            if (
                key in {"started_at", "finished_at", "rollback_at", "rollback_time_iso", "at"}
                or key.endswith("_ms")
                or key.endswith("_ms_total")
                or key in {"duration_ms", "latency_ms", "observed"}
            ):
                continue
            result[key] = deterministic_projection(item)
        return result
    if isinstance(value, list):
        return [deterministic_projection(item) for item in value]
    return value


def _start_gateway(config: str, port: int, log_path: Path) -> subprocess.Popen[str]:
    env = os.environ.copy()
    env.pop("RANKING_FAULT_FAIL_LOAD", None)
    log_path.parent.mkdir(parents=True, exist_ok=True)
    log_file = log_path.open("w", encoding="utf-8")
    command = [
        sys.executable,
        str(PROJECT_ROOT / "run_gateway.py"),
        "--config",
        str((PROJECT_ROOT / config).resolve())
        if not Path(config).is_absolute()
        else config,
        "--port",
        str(port),
    ]
    process = subprocess.Popen(
        command,
        cwd=PROJECT_ROOT,
        env=env,
        stdout=log_file,
        stderr=subprocess.STDOUT,
        text=True,
    )
    process._traffic_log_file = log_file  # type: ignore[attr-defined]
    return process


async def main_async(args: argparse.Namespace) -> int:
    scenario_path = Path(args.scenario).resolve()
    scenario = json.loads(scenario_path.read_text(encoding="utf-8"))
    base_url = args.base_url.rstrip("/")
    parsed = urlparse(base_url)
    if parsed.hostname not in {"127.0.0.1", "localhost", "::1"}:
        raise SystemExit("only loopback gateway URLs are allowed")
    report_path = Path(args.report).resolve()
    process: subprocess.Popen[str] | None = None
    gateway_log = PROJECT_ROOT / "artifacts" / "traffic_gateway.log"
    if args.autostart:
        process = _start_gateway(args.config, parsed.port or 8080, gateway_log)
    try:
        report = await run_scenario(base_url, scenario, report_path)
    finally:
        if process is not None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
            log_file = getattr(process, "_traffic_log_file", None)
            if log_file is not None:
                log_file.close()
    print(
        json.dumps(
            {
                "report": str(report_path),
                "requests": report["requests"],
                "rollback_count": report["rollback"]["count"],
                "rollback_latency_ms": report["rollback"]["latency_ms"],
                "acceptance": report["acceptance"],
            },
            indent=2,
            sort_keys=True,
        )
    )
    return 0 if all(report["acceptance"].values()) else 1


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Deterministic ranking gateway traffic and fault generator"
    )
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--scenario", default="traffic_scenario.json")
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--report", default="artifacts/traffic_report.json")
    parser.add_argument(
        "--autostart",
        action="store_true",
        help="start a local gateway subprocess and stop it after the run",
    )
    args = parser.parse_args()
    raise SystemExit(asyncio.run(main_async(args)))


if __name__ == "__main__":
    main()
