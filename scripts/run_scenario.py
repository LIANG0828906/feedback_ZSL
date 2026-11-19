from __future__ import annotations

import argparse
import http.client
import json
import math
import os
import random
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
_CONNECTION_LOCAL = threading.local()
_NO_PROXY_OPENER = urllib.request.build_opener(
    urllib.request.ProxyHandler({})
)


@dataclass(frozen=True)
class RequestResult:
    tenant_id: str
    method: str
    started_at: float
    latency_ms: float
    status_code: int | None
    business_code: str
    fallback_source: str
    reason: str
    error: str | None
    response: dict[str, Any]


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds")


def http_json(
    method: str,
    url: str,
    *,
    body: dict[str, Any] | None = None,
    headers: dict[str, str] | None = None,
    timeout: float = 5.0,
) -> tuple[int, dict[str, str], Any, float]:
    payload = (
        json.dumps(body, separators=(",", ":")).encode("utf-8")
        if body is not None
        else None
    )
    request_headers = {
        "Accept": "application/json",
        "Connection": "close",
    }
    request_headers.update(headers or {})
    if payload is not None:
        request_headers["Content-Type"] = "application/json"
    request = urllib.request.Request(
        url,
        data=payload,
        headers=request_headers,
        method=method,
    )
    started = time.monotonic()
    try:
        with _NO_PROXY_OPENER.open(request, timeout=timeout) as response:
            raw = response.read()
            elapsed = (time.monotonic() - started) * 1000.0
            parsed = json.loads(raw) if raw else None
            return response.status, dict(response.headers.items()), parsed, elapsed
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        elapsed = (time.monotonic() - started) * 1000.0
        try:
            parsed = json.loads(raw) if raw else None
        except json.JSONDecodeError:
            parsed = {"raw": raw.decode("utf-8", errors="replace")}
        return exc.code, dict(exc.headers.items()), parsed, elapsed


def wait_for_health(base_url: str, timeout: float = 30.0) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            _, _, payload, _ = http_json(
                "GET", base_url + "/healthz", timeout=1.0
            )
            if isinstance(payload, dict) and payload.get("status") == "ok":
                return payload
        except Exception as exc:
            last_error = exc
        time.sleep(0.2)
    raise RuntimeError(f"service did not become healthy: {last_error}")


def admin_call(
    base_url: str,
    token: str,
    method: str,
    path: str,
    *,
    body: dict[str, Any] | None = None,
    timeout: float = 5.0,
) -> Any:
    status, _, payload, _ = http_json(
        method,
        base_url + path,
        body=body,
        headers={"X-Admin-Token": token},
        timeout=timeout,
    )
    if status >= 400:
        raise RuntimeError(f"admin call {method} {path} failed: {status} {payload}")
    return payload


def fetch_all_audit(
    base_url: str,
    token: str,
    run_id: str,
    after_seq: int = 0,
) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    cursor = after_seq
    while True:
        payload = admin_call(
            base_url,
            token,
            "GET",
            f"/admin/audit?run_id={urllib.parse.quote(run_id)}&after_seq={cursor}&limit=5000",
        )
        batch = payload.get("events", [])
        events.extend(batch)
        if len(batch) < 5000:
            break
        cursor = batch[-1]["seq"]
    return events


def percentile(values: list[float], fraction: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    index = max(0, math.ceil(fraction * len(ordered)) - 1)
    return round(ordered[index], 3)


def latency_summary(values: list[float]) -> dict[str, float | None]:
    return {
        "p50": percentile(values, 0.50),
        "p95": percentile(values, 0.95),
        "p99": percentile(values, 0.99),
        "max": round(max(values), 3) if values else None,
    }


def _parse_dt(value: str) -> float:
    return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def _overlap(seconds: list[tuple[float, float]]) -> float:
    if not seconds:
        return 0.0
    seconds = sorted(seconds)
    merged: list[list[float]] = []
    for start, end in seconds:
        if end < start:
            continue
        if not merged or start > merged[-1][1]:
            merged.append([start, end])
        else:
            merged[-1][1] = max(merged[-1][1], end)
    return round(sum(end - start for start, end in merged), 3)


def duration_metrics(
    *,
    events: list[dict[str, Any]],
    initial_status: dict[str, Any],
    scenario_start: float,
    scenario_end: float,
) -> dict[str, float]:
    start = max(scenario_start, scenario_start)
    end = max(scenario_end, start)
    level = int(initial_status.get("protection", {}).get("degrade_level", 0))
    capacity = bool(
        initial_status.get("protection", {}).get("capacity_protected", False)
    )
    circuit = str(initial_status.get("circuit", {}).get("state", "closed"))
    level_periods: list[tuple[float, float]] = []
    capacity_periods: list[tuple[float, float]] = []
    circuit_periods: list[tuple[float, float]] = []
    level_since = start
    capacity_since = start
    circuit_since = start
    for event in events:
        try:
            event_time = _parse_dt(event["created_at"])
        except (KeyError, ValueError):
            continue
        event_time = min(max(event_time, start), end)
        event_type = event.get("event_type")
        if event_type == "DEGRADE_LEVEL_CHANGED":
            if level > 0:
                level_periods.append((level_since, event_time))
            try:
                level = int(event.get("to_state", "0"))
            except (TypeError, ValueError):
                level = 0
            level_since = event_time
        elif event_type == "CAPACITY_ENTERED":
            if not capacity:
                capacity = True
                capacity_since = event_time
        elif event_type == "CAPACITY_EXITED":
            if capacity:
                capacity_periods.append((capacity_since, event_time))
                capacity = False
        elif event_type in {"CIRCUIT_OPENED", "CIRCUIT_REOPENED"}:
            if circuit == "closed":
                circuit = "open"
                circuit_since = event_time
        elif event_type == "CIRCUIT_CLOSED":
            if circuit != "closed":
                circuit_periods.append((circuit_since, event_time))
                circuit = "closed"
    if level > 0:
        level_periods.append((level_since, end))
    if capacity:
        capacity_periods.append((capacity_since, end))
    if circuit != "closed":
        circuit_periods.append((circuit_since, end))
    return {
        "degraded_seconds": _overlap(level_periods),
        "capacity_protected_seconds": _overlap(capacity_periods),
        "circuit_open_seconds": _overlap(circuit_periods),
    }


def _choose_tenant(rng: random.Random, weights: dict[str, float]) -> str:
    tenants = list(weights)
    values = [max(0.0, float(weights[tenant])) for tenant in tenants]
    return rng.choices(tenants, weights=values, k=1)[0]


def _profile_at(
    profiles: list[dict[str, Any]], elapsed: float
) -> dict[str, Any] | None:
    for profile in profiles:
        if float(profile["start_seconds"]) <= elapsed < float(profile["end_seconds"]):
            return profile
    return None


def execute_request(
    base_url: str,
    tenant_id: str,
    method: str,
    path: str,
    *,
    body: dict[str, Any] | None,
    timeout: float,
) -> RequestResult:
    started_at = time.time()
    parsed = urllib.parse.urlparse(base_url)
    connection_key = (parsed.hostname, parsed.port or 80)
    connection = getattr(_CONNECTION_LOCAL, "connection", None)
    if connection is None or getattr(_CONNECTION_LOCAL, "key", None) != connection_key:
        connection = http.client.HTTPConnection(
            connection_key[0], connection_key[1], timeout=timeout
        )
        _CONNECTION_LOCAL.connection = connection
        _CONNECTION_LOCAL.key = connection_key
    request_headers = {
        "Accept": "application/json",
        "Connection": "keep-alive",
        "X-Tenant-Id": tenant_id,
    }
    payload = (
        json.dumps(body, separators=(",", ":")).encode("utf-8")
        if body is not None
        else None
    )
    if payload is not None:
        request_headers["Content-Type"] = "application/json"
    try:
        try:
            connection.request(
                method,
                path,
                body=payload,
                headers=request_headers,
            )
            response = connection.getresponse()
            raw = response.read()
        except (
            http.client.CannotSendRequest,
            http.client.BadStatusLine,
            http.client.RemoteDisconnected,
            BrokenPipeError,
            ConnectionResetError,
        ):
            connection.close()
            connection = http.client.HTTPConnection(
                connection_key[0], connection_key[1], timeout=timeout
            )
            _CONNECTION_LOCAL.connection = connection
            connection.request(
                method,
                path,
                body=payload,
                headers=request_headers,
            )
            response = connection.getresponse()
            raw = response.read()
        elapsed = (time.time() - started_at) * 1000.0
        status = response.status
        headers = dict(response.getheaders())
        try:
            parsed_payload = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            parsed_payload = {
                "raw": raw.decode("utf-8", errors="replace")
            }
        payload_dict = (
            parsed_payload if isinstance(parsed_payload, dict) else {}
        )
        lower_headers = {
            key.lower(): value for key, value in headers.items()
        }
        reason = ""
        details = payload_dict.get("details")
        if isinstance(details, dict):
            reason = str(details.get("reason", ""))
        fallback = lower_headers.get("x-fallback-source", "")
        if not fallback and isinstance(payload_dict.get("fallback"), dict):
            fallback = str(payload_dict["fallback"].get("source", ""))
        return RequestResult(
            tenant_id=tenant_id,
            method=method,
            started_at=started_at,
            latency_ms=elapsed,
            status_code=status,
            business_code=str(
                lower_headers.get("x-business-code")
                or payload_dict.get("business_code", "")
            ),
            fallback_source=fallback,
            reason=reason,
            error=None,
            response=payload_dict,
        )
    except Exception as exc:
        try:
            connection.close()
        except Exception:
            pass
        _CONNECTION_LOCAL.connection = None
        return RequestResult(
            tenant_id=tenant_id,
            method=method,
            started_at=started_at,
            latency_ms=(time.time() - started_at) * 1000.0,
            status_code=None,
            business_code="TRANSPORT_ERROR",
            fallback_source="",
            reason="",
            error=str(exc),
            response={},
        )


def run_scenario(
    scenario: dict[str, Any],
    *,
    base_url: str,
    run_id: str,
    initial_status: dict[str, Any],
    initial_seq: int,
) -> tuple[dict[str, Any], dict[str, Any]]:
    token = str(scenario["admin_token"])
    duration = float(scenario["duration_seconds"])
    warmup = float(scenario.get("warmup_seconds", 0.0))
    if warmup > 0:
        time.sleep(warmup)
    profiles = scenario.get("traffic_profiles") or [
        {
            "start_seconds": 0,
            "end_seconds": duration,
            "request_rate_per_second": scenario["request_rate_per_second"],
            "read_ratio": scenario.get("read_ratio", 0.85),
        }
    ]
    tenants = scenario["tenants"]
    statuses = scenario.get("statuses", ["created", "paid"])
    page_size = int(scenario.get("page_size", 20))
    max_workers = int(scenario.get("max_workers", 32))
    max_pending = int(scenario.get("max_pending", 256))
    timeout = float(scenario.get("request_timeout_seconds", 5.0))
    create_ratio = float(
        scenario.get("write_mix", {}).get("create_ratio", 0.6)
    )
    seed = int(scenario.get("seed", 268))
    rng = random.Random(seed)
    results: list[RequestResult] = []
    results_lock = threading.Lock()
    pending = threading.BoundedSemaphore(max_pending)
    skipped = 0
    scheduled = 0
    fault_applications: list[dict[str, Any]] = []
    fault_lock = threading.Lock()
    load_start_wall = time.time()
    load_start_monotonic = time.monotonic()

    admin_call(base_url, token, "DELETE", "/admin/faults")

    def worker(
        tenant_id: str,
        method: str,
        path: str,
        body: dict[str, Any] | None,
    ) -> None:
        try:
            result = execute_request(
                base_url,
                tenant_id,
                method,
                path,
                body=body,
                timeout=timeout,
            )
            with results_lock:
                results.append(result)
        finally:
            pending.release()

    executor = ThreadPoolExecutor(
        max_workers=max_workers, thread_name_prefix="order-guard-load"
    )

    def load_loop() -> None:
        nonlocal skipped, scheduled
        index = 0
        next_tick = 0.0
        while True:
            elapsed = time.monotonic() - load_start_monotonic
            if elapsed >= duration:
                break
            profile = _profile_at(profiles, elapsed)
            if profile is None:
                time.sleep(min(0.05, duration - elapsed))
                continue
            rate = max(0.001, float(profile["request_rate_per_second"]))
            interval = 1.0 / rate
            target = next_tick
            if target > elapsed:
                time.sleep(min(0.05, target - elapsed))
                continue
            if not pending.acquire(timeout=0.05):
                skipped += 1
                next_tick += interval
                continue
            tenant_id = _choose_tenant(rng, tenants)
            read_ratio = float(profile.get("read_ratio", 0.85))
            if rng.random() < read_ratio:
                method = "GET"
                params = urllib.parse.urlencode(
                    {
                        "status": rng.choice(statuses),
                        "page": 1,
                        "page_size": page_size,
                    }
                )
                path = f"/api/v1/orders?{params}"
                body = None
            elif rng.random() < create_ratio:
                method = "POST"
                path = "/api/v1/orders"
                body = {"amount_cents": rng.randint(100, 250000)}
            else:
                method = "PATCH"
                order_index = rng.randrange(150)
                path = f"/api/v1/orders/ord-{tenant_id}-{order_index:06d}/status"
                body = {"status": rng.choice(statuses)}
            try:
                executor.submit(worker, tenant_id, method, path, body)
                scheduled += 1
            except RuntimeError:
                pending.release()
            index += 1
            next_tick += interval
            if elapsed - next_tick > 1.0:
                next_tick = elapsed

    def fault_loop() -> None:
        for fault in sorted(
            scenario.get("faults", []), key=lambda item: item["at_seconds"]
        ):
            wait = load_start_monotonic + float(fault["at_seconds"]) - time.monotonic()
            if wait > 0:
                time.sleep(wait)
            if time.monotonic() - load_start_monotonic >= duration:
                break
            body = {
                key: fault[key]
                for key in (
                    "mode",
                    "delay_ms",
                    "error_rate",
                    "duration_seconds",
                )
                if key in fault
            }
            try:
                response = admin_call(
                    base_url,
                    token,
                    "POST",
                    "/admin/faults",
                    body=body,
                )
                with fault_lock:
                    fault_applications.append(
                        {
                            "at_seconds": float(fault["at_seconds"]),
                            "request": body,
                            "applied": True,
                            "state": response.get("fault"),
                        }
                    )
            except Exception as exc:
                with fault_lock:
                    fault_applications.append(
                        {
                            "at_seconds": float(fault["at_seconds"]),
                            "request": body,
                            "applied": False,
                            "error": str(exc),
                        }
                    )

    fault_thread = threading.Thread(
        target=fault_loop, name="order-guard-faults", daemon=True
    )
    fault_thread.start()
    try:
        load_loop()
        load_end_wall = time.time()
        executor.shutdown(wait=True, cancel_futures=False)
    finally:
        fault_thread.join(timeout=max(1.0, duration))
        try:
            admin_call(base_url, token, "DELETE", "/admin/faults")
        except Exception:
            pass
        executor.shutdown(wait=False, cancel_futures=True)

    settle_seconds = float(scenario.get("settle_seconds", 1.0))
    if settle_seconds > 0:
        time.sleep(settle_seconds)
    end_wall = time.time()

    final_status = admin_call(base_url, token, "GET", "/admin/status")
    final_audit = fetch_all_audit(
        base_url, token, run_id, after_seq=initial_seq
    )
    capacity_report = admin_call(
        base_url,
        token,
        "GET",
        f"/admin/capacity-report?scenario={urllib.parse.quote(str(scenario['name']))}",
    )
    durations = duration_metrics(
        events=final_audit,
        initial_status=initial_status,
        scenario_start=load_start_wall,
        scenario_end=end_wall,
    )

    tenant_ids = list(tenants)
    tenant_results: dict[str, Any] = {}
    for tenant_id in tenant_ids:
        selected = [item for item in results if item.tenant_id == tenant_id]
        reads = [item for item in selected if item.method == "GET"]
        statuses_seen: dict[str, int] = {}
        codes: dict[str, int] = {}
        rate_limited = 0
        concurrency_limited = 0
        write_rejected = 0
        fallback_count = 0
        transport_errors = 0
        for item in selected:
            statuses_seen[str(item.status_code)] = (
                statuses_seen.get(str(item.status_code), 0) + 1
            )
            codes[item.business_code] = codes.get(item.business_code, 0) + 1
            if item.status_code == 429:
                if item.reason == "concurrency":
                    concurrency_limited += 1
                else:
                    rate_limited += 1
            if item.status_code == 503 and item.method in {"POST", "PATCH"}:
                write_rejected += 1
            if item.fallback_source:
                fallback_count += 1
            if item.error:
                transport_errors += 1
        tenant_results[tenant_id] = {
            "requests": len(selected),
            "success": sum(
                1
                for item in selected
                if item.status_code is not None
                and 200 <= item.status_code < 400
            ),
            "transport_errors": transport_errors,
            "rate_limited": rate_limited,
            "concurrency_limited": concurrency_limited,
            "write_rejected": write_rejected,
            "fallback_responses": fallback_count,
            "latency_ms": latency_summary([item.latency_ms for item in selected]),
            "read_latency_ms": latency_summary(
                [item.latency_ms for item in reads]
            ),
            "status_codes": dict(sorted(statuses_seen.items())),
            "business_codes": dict(sorted(codes.items())),
        }

    rate_by_reason: dict[str, int] = {}
    rate_by_tenant: dict[str, int] = {}
    for item in results:
        if item.status_code == 429:
            reason = item.reason or "rate"
            rate_by_reason[reason] = rate_by_reason.get(reason, 0) + 1
            rate_by_tenant[item.tenant_id] = (
                rate_by_tenant.get(item.tenant_id, 0) + 1
            )
    circuit_open_events = sum(
        1
        for event in final_audit
        if event.get("event_type") in {"CIRCUIT_OPENED", "CIRCUIT_REOPENED"}
    )
    capacity_entries = sum(
        1
        for event in final_audit
        if event.get("event_type") == "CAPACITY_ENTERED"
    )
    event_types: dict[str, int] = {}
    for event in final_audit:
        event_types[event["event_type"]] = (
            event_types.get(event["event_type"], 0) + 1
        )
    audit_summary = {
        "event_count": len(final_audit),
        "first_seq": final_audit[0]["seq"] if final_audit else None,
        "last_seq": final_audit[-1]["seq"] if final_audit else None,
        "event_types": dict(sorted(event_types.items())),
        "events": [
            {
                "seq": event["seq"],
                "event_type": event["event_type"],
                "tenant_id": event.get("tenant_id"),
                "from_state": event.get("from_state"),
                "to_state": event.get("to_state"),
                "business_code": event["business_code"],
                "reason": event["reason"],
                "created_at": event["created_at"],
            }
            for event in final_audit
        ],
    }
    slis = final_status.get("slis", {})
    error_budget = {
        name: {
            "kind": item.get("kind"),
            "value": item.get("value"),
            "target": item.get("target"),
            "target_ms": item.get("target_ms"),
            "samples": item.get("samples"),
            "error_budget_consumed": item.get("error_budget_consumed"),
            "error_budget_remaining": item.get("error_budget_remaining"),
            "breached": item.get("breached"),
            "peak_error_budget_consumed": (
                final_status.get("sli_peaks", {})
                .get(name, {})
                .get("peak_error_budget_consumed")
            ),
        }
        for name, item in slis.items()
    }
    with fault_lock:
        applied_faults = list(fault_applications)
    result = {
        "schema_version": "scenario-result-v1",
        "scenario": scenario["name"],
        "run_id": run_id,
        "started_at": datetime.fromtimestamp(
            load_start_wall, tz=timezone.utc
        ).isoformat(timespec="milliseconds"),
        "finished_at": datetime.fromtimestamp(
            end_wall, tz=timezone.utc
        ).isoformat(timespec="milliseconds"),
        "planned_duration_seconds": duration,
        "traffic": {
            "scheduled_requests": scheduled,
            "completed_requests": len(results),
            "generator_skipped_requests": skipped,
            "profiles": profiles,
        },
        "tenants": tenant_results,
        "protection": {
            "rate_limit_rejections": {
                "total": sum(rate_by_reason.values()),
                "by_reason": dict(sorted(rate_by_reason.items())),
                "by_tenant": dict(sorted(rate_by_tenant.items())),
            },
            "circuit_open_events": circuit_open_events,
            "fallback_responses": sum(
                1 for item in results if item.fallback_source
            ),
            "capacity_entries": capacity_entries,
            **durations,
            "final_degrade_level": final_status.get("protection", {}).get(
                "degrade_level"
            ),
            "final_protection_state": final_status.get("protection", {}).get(
                "protection_state"
            ),
            "final_protection_code": final_status.get("protection", {}).get(
                "protection_code"
            ),
            "final_circuit_state": final_status.get("circuit", {}).get("state"),
            "final_capacity_protected": final_status.get("protection", {}).get(
                "capacity_protected"
            ),
        },
        "error_budget": error_budget,
        "faults": applied_faults,
        "audit": audit_summary,
        "capacity_report": capacity_report,
    }
    return result, capacity_report


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Run a deterministic local order-guard load scenario"
    )
    parser.add_argument("scenario", help="scenario JSON path")
    parser.add_argument(
        "--service-config",
        default=str(ROOT / "config" / "service.json"),
        help="service JSON config used when starting a managed service",
    )
    parser.add_argument(
        "--base-url",
        help="override scenario base_url and attach to an existing service",
    )
    parser.add_argument(
        "--no-start-service",
        action="store_true",
        help="do not spawn a service process",
    )
    parser.add_argument(
        "--output-dir",
        default=str(ROOT / "artifacts"),
        help="directory for scenario and capacity JSON files",
    )
    parser.add_argument("--wait", type=float, default=25.0)
    args = parser.parse_args()

    scenario_path = Path(args.scenario).resolve()
    scenario = json.loads(scenario_path.read_text(encoding="utf-8"))
    base_url = (args.base_url or scenario["base_url"]).rstrip("/")
    parsed = urllib.parse.urlparse(base_url)
    port = parsed.port
    process: subprocess.Popen | None = None
    log_handle = None
    output_dir = Path(args.output_dir).resolve()
    output_dir.mkdir(parents=True, exist_ok=True)
    run_id = (
        f"scenario-{scenario['name']}-"
        f"{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}-{os.getpid()}"
    )
    try:
        if not args.no_start_service:
            if port is None:
                raise RuntimeError("scenario base_url must include a port")
            log_handle = open(
                output_dir / f"service-{scenario['name']}.log",
                "a",
                encoding="utf-8",
            )
            env = os.environ.copy()
            env["ORDER_GUARD_RUN_ID"] = run_id
            command = [
                sys.executable,
                "-m",
                "order_guard",
                "--config",
                str(Path(args.service_config).resolve()),
                "--port",
                str(port),
            ]
            process = subprocess.Popen(
                command,
                cwd=str(ROOT),
                env=env,
                stdout=log_handle,
                stderr=subprocess.STDOUT,
            )
        health = wait_for_health(base_url, timeout=args.wait)
        run_id = str(health.get("run_id", run_id))
        initial_status = admin_call(
            base_url, scenario["admin_token"], "GET", "/admin/status"
        )
        initial_audit = fetch_all_audit(
            base_url,
            scenario["admin_token"],
            run_id,
            after_seq=0,
        )
        initial_seq = initial_audit[-1]["seq"] if initial_audit else 0
        result, capacity = run_scenario(
            scenario,
            base_url=base_url,
            run_id=run_id,
            initial_status=initial_status,
            initial_seq=initial_seq,
        )
        scenario_output = output_dir / f"scenario-{scenario['name']}.json"
        capacity_output = output_dir / f"capacity-{scenario['name']}.json"
        scenario_output.write_text(
            json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        capacity_output.write_text(
            json.dumps(capacity, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        print(
            json.dumps(
                {
                    "scenario": scenario["name"],
                    "run_id": run_id,
                    "result": str(scenario_output),
                    "capacity_report": str(capacity_output),
                    "requests": result["traffic"]["completed_requests"],
                    "rate_limit_rejections": result["protection"][
                        "rate_limit_rejections"
                    ]["total"],
                    "circuit_open_events": result["protection"][
                        "circuit_open_events"
                    ],
                    "degraded_seconds": result["protection"][
                        "degraded_seconds"
                    ],
                },
                ensure_ascii=False,
                indent=2,
            )
        )
        return 0
    finally:
        if process is not None:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        if log_handle is not None:
            log_handle.close()


if __name__ == "__main__":
    raise SystemExit(main())
