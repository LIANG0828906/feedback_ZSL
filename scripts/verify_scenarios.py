from __future__ import annotations

import argparse
import asyncio
import json
import socket
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parent.parent
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from connectlab.config import load_config
from connectlab.http import encode_response, read_request_head, read_response_head
from connectlab.service import ConnectLabService
from connectlab.util import atomic_write_json, utc_now


def free_port() -> int:
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    return port


def unique_free_ports(count: int) -> list[int]:
    ports: list[int] = []
    while len(ports) < count:
        candidate = free_port()
        if candidate not in ports:
            ports.append(candidate)
    return ports


async def api_json(
    port: int,
    method: str,
    path: str,
    payload: dict[str, Any] | None = None,
) -> tuple[int, dict[str, Any]]:
    reader, writer = await asyncio.open_connection("127.0.0.1", port)
    body = b""
    headers = {
        "Host": f"127.0.0.1:{port}",
        "Connection": "close",
    }
    if payload is not None:
        body = json.dumps(payload, sort_keys=True).encode("utf-8")
        headers["Content-Type"] = "application/json"
        headers["Content-Length"] = str(len(body))
    head = f"{method} {path} HTTP/1.1\r\n" + "".join(
        f"{key}: {value}\r\n" for key, value in headers.items()
    ) + "\r\n"
    writer.write(head.encode("latin-1") + body)
    await writer.drain()
    status, _reason, response_headers = await read_response_head(reader, 30_000)
    length = int(response_headers.get("content-length", "0"))
    response_body = await reader.readexactly(length) if length else b""
    writer.close()
    await writer.wait_closed()
    return status, json.loads(response_body.decode("utf-8"))


async def api_text(port: int, path: str) -> str:
    reader, writer = await asyncio.open_connection("127.0.0.1", port)
    writer.write(
        f"GET {path} HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\nConnection: close\r\n\r\n".encode(
            "latin-1"
        )
    )
    await writer.drain()
    status, _reason, headers = await read_response_head(reader, 10_000)
    assert status == 200
    length = int(headers.get("content-length", "0"))
    body = await reader.readexactly(length)
    writer.close()
    await writer.wait_closed()
    return body.decode("utf-8")


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


async def run_verification(config_path: str | None, output: Path) -> dict[str, Any]:
    config, _ = load_config(config_path)
    api_port, proxy_port, upstream_port, alternate_upstream_port, dead_port = (
        unique_free_ports(5)
    )
    config["service"]["port"] = api_port
    config["server"] = {"host": config["service"]["host"], "port": config["service"]["port"]}
    config["proxy"]["listen_port"] = proxy_port
    config["upstream"]["listen_port"] = upstream_port
    config["upstream"]["extra_ports"] = [alternate_upstream_port]
    database = ROOT / "var" / f"verify-{utc_now().replace(':', '').replace('.', '')}.sqlite3"
    injection = ROOT / "var" / f"verify-injection-{database.stem}.json"
    config["service"]["database"] = str(database.relative_to(ROOT))
    config["injection"]["file"] = str(injection.relative_to(ROOT))
    config["batch"]["batch_timeout_ms"] = 15_000
    config["batch"]["per_hop_timeout_ms"] = 800
    config["batch"]["request_timeout_ms"] = 3_000
    config["targets"] = {
        "local-a": {
            "proxy_host": "127.0.0.1",
            "proxy_port": config["proxy"]["listen_port"],
            "upstream_host": "127.0.0.1",
            "upstream_port": config["upstream"]["listen_port"],
        },
        "dead-upstream": {
            "proxy_host": "127.0.0.1",
            "proxy_port": config["proxy"]["listen_port"],
            "upstream_host": "127.0.0.1",
            "upstream_port": dead_port,
        },
        "local-b": {
            "proxy_host": "127.0.0.1",
            "proxy_port": config["proxy"]["listen_port"],
            "upstream_host": "127.0.0.1",
            "upstream_port": alternate_upstream_port,
        },
    }
    config["default_target"] = "local-a"

    report: dict[str, Any] = {
        "assertions": [],
        "ports": {
            "api": config["service"]["port"],
            "proxy": config["proxy"]["listen_port"],
            "upstream": config["upstream"]["listen_port"],
            "upstream_b": alternate_upstream_port,
        },
        "started_at": utc_now(),
    }
    service = ConnectLabService(config)
    api_port = config["service"]["port"]
    try:
        await service.start()
        await _scenario_baseline(api_port, report)
        await _scenario_alternate_target(api_port, report)
        await _scenario_syn(api_port, report)
        await _scenario_upstream_reuse(api_port, report)
        await _scenario_timeout(api_port, report)
        await _scenario_unaffected_paths(api_port, report)
        await _scenario_unexplained(api_port, report)
        await _scenario_duplicate_and_metrics(api_port, report)
        await _scenario_deadline(api_port, report)

        await service.stop()
        await asyncio.sleep(0.1)
        service = ConnectLabService(config)
        await service.start()
        history = await _verify_restart(api_port, report)
        report["restart_history_ok"] = history
    finally:
        try:
            await service.stop()
        except Exception:
            pass
        report["finished_at"] = utc_now()
        report["ok"] = all(item["ok"] for item in report["assertions"])
        atomic_write_json(output, report)
        try:
            database.unlink(missing_ok=True)
            injection.unlink(missing_ok=True)
            Path(str(database) + "-wal").unlink(missing_ok=True)
            Path(str(database) + "-shm").unlink(missing_ok=True)
        except OSError:
            pass
    return report


async def submit_batch(port: int, payload: dict[str, Any]) -> dict[str, Any]:
    status, result = await api_json(port, "POST", "/v1/batches", payload)
    require(status == 201, f"batch {payload.get('batch_id')} failed: {status} {result}")
    return result


async def switch_injection(port: int, payload: dict[str, Any]) -> None:
    status, result = await api_json(port, "POST", "/v1/injections", payload)
    require(status == 201, f"injection switch failed: {status} {result}")


def assert_case(report: dict[str, Any], name: str, condition: bool, detail: Any) -> None:
    report["assertions"].append(
        {
            "detail": detail,
            "name": name,
            "ok": bool(condition),
        }
    )
    require(condition, f"{name}: {detail}")


async def _scenario_baseline(port: int, report: dict[str, Any]) -> None:
    await switch_injection(
        port,
        {
            "duration_ms": 600_000,
            "id": "verify-baseline",
            "path": "all",
            "ratio": 0.0,
            "type": "none",
        },
    )
    result = await submit_batch(
        port,
        {
            "batch_id": "verify-baseline",
            "count": 12,
            "concurrency": 3,
            "paths": ["short", "keepalive", "upload"],
        },
    )
    summary = result["summary"]
    assert_case(
        report,
        "baseline_no_resets",
        summary["resets"] == 0 and summary["timeouts"] == 0,
        summary,
    )
    assert_case(
        report,
        "baseline_covers_three_paths",
        all(summary["by_path"][path]["requests"] == 4 for path in ("short", "keepalive", "upload")),
        summary["by_path"],
    )
    assert_case(
        report,
        "baseline_keepalive_reuse_observed",
        summary["reuse_attempts"] > 0 and summary["reuse_failures"] == 0,
        {
            "reuse_attempts": summary["reuse_attempts"],
            "reuse_failures": summary["reuse_failures"],
        },
    )


async def _scenario_syn(port: int, report: dict[str, Any]) -> None:
    await switch_injection(
        port,
        {
            "duration_ms": 600_000,
            "id": "verify-syn",
            "path": "short",
            "ratio": 1.0,
            "type": "syn_drop",
        },
    )
    result = await submit_batch(
        port,
        {
            "batch_id": "verify-syn",
            "count": 4,
            "concurrency": 2,
            "paths": ["short"],
        },
    )
    status, detail = await api_json(port, "GET", "/v1/batches/verify-syn")
    require(status == 200, "status query failed")
    first = detail["requests"][0]
    summary = result["summary"]
    assert_case(
        report,
        "syn_attribution",
        summary["attribution_by_fault"]["syn_not_reached_upstream"] == 4
        and first["attribution_hop"] == "proxy"
        and first["details"]["attribution"]["evidence"]["kind"] == "syn_dropped",
        {
            "attribution": first["details"]["attribution"],
            "missing_hops": first["missing_hops"],
        },
    )
    assert_case(
        report,
        "syn_missing_upstream_is_explicit",
        first["missing_hops"] == ["upstream"],
        first["coverage"],
    )


async def _scenario_alternate_target(port: int, report: dict[str, Any]) -> None:
    await switch_injection(
        port,
        {
            "duration_ms": 600_000,
            "id": "verify-alt-none",
            "path": "all",
            "ratio": 0.0,
            "type": "none",
        },
    )
    clean = await submit_batch(
        port,
        {
            "batch_id": "verify-alt-clean",
            "count": 3,
            "concurrency": 1,
            "paths": ["short"],
            "target": "local-b",
        },
    )
    status, detail = await api_json(port, "GET", "/v1/batches/verify-alt-clean")
    require(status == 200, "alternate target status query failed")
    assert_case(
        report,
        "alternate_local_target_is_healthy",
        clean["summary"]["successes"] == 3
        and detail["requests"][0]["coverage"]["upstream"]["status"] == "observed",
        {
            "coverage": detail["requests"][0]["coverage"],
            "summary": clean["summary"],
        },
    )
    await switch_injection(
        port,
        {
            "duration_ms": 600_000,
            "id": "verify-alt-upstream",
            "path": "short",
            "ratio": 1.0,
            "type": "upstream_disconnect",
        },
    )
    faulty = await submit_batch(
        port,
        {
            "batch_id": "verify-alt-fault",
            "count": 2,
            "concurrency": 1,
            "paths": ["short"],
            "target": "local-b",
        },
    )
    assert_case(
        report,
        "alternate_target_keeps_fault_conclusion",
        faulty["summary"]["attribution_by_fault"]["upstream_early_disconnect"] == 2,
        faulty["summary"],
    )


async def _scenario_upstream_reuse(port: int, report: dict[str, Any]) -> None:
    await switch_injection(
        port,
        {
            "duration_ms": 600_000,
            "id": "verify-upstream",
            "path": "keepalive",
            "ratio": 1.0,
            "type": "upstream_disconnect",
        },
    )
    result = await submit_batch(
        port,
        {
            "batch_id": "verify-upstream",
            "count": 10,
            "concurrency": 1,
            "paths": ["keepalive"],
        },
    )
    status, detail = await api_json(port, "GET", "/v1/batches/verify-upstream")
    require(status == 200, "status query failed")
    reset_request = next(
        request for request in detail["requests"] if request["outcome"] == "reset"
    )
    summary = result["summary"]
    assert_case(
        report,
        "upstream_reuse_failure_attribution",
        summary["attribution_by_fault"]["upstream_early_disconnect"] == 5
        and summary["reuse_failures"] == 5
        and reset_request["details"]["attribution"]["subtype"]
        == "reuse_failed_connection",
        {
            "attribution": reset_request["details"]["attribution"],
            "reuse_failures": summary["reuse_failures"],
        },
    )
    assert_case(
        report,
        "proxy_awareness_is_recorded",
        any(
            event["kind"] == "reuse_attempt"
            and event["details"].get("proxy_aware_before_send") is False
            for event in reset_request["event_sequence"]
        ),
        [
            {
                "details": event["details"],
                "kind": event["kind"],
            }
            for event in reset_request["event_sequence"]
            if event["kind"] in ("reuse_attempt", "reuse_failure")
        ],
    )
    proxy_connection_states: dict[str, set[str]] = {}
    state_violations = []
    closed_connections: set[tuple[str, str]] = set()
    for event in reset_request["event_sequence"]:
        key = (event["hop"], event["connection_id"])
        if event["kind"] == "connect_success" and key in closed_connections:
            state_violations.append({"event": event, "violation": "open_after_close"})
        if event["kind"] in ("close", "reset"):
            if key in closed_connections:
                state_violations.append(
                    {"event": event, "violation": "duplicate_terminal_event"}
                )
            closed_connections.add(key)
        if event["hop"] == "proxy" and event["kind"] in (
            "connect_success",
            "close",
            "reset",
        ):
            proxy_connection_states.setdefault(event["connection_id"], set()).add(
                event["kind"]
            )
    assert_case(
        report,
        "proxy_connection_state_is_monotonic",
        not state_violations,
        {
            "state_violations": state_violations,
            "states": {
                connection: sorted(kinds)
                for connection, kinds in proxy_connection_states.items()
            },
        },
    )


async def _scenario_timeout(port: int, report: dict[str, Any]) -> None:
    await switch_injection(
        port,
        {
            "duration_ms": 600_000,
            "id": "verify-timeout",
            "path": "upload",
            "ratio": 1.0,
            "type": "proxy_read_timeout",
        },
    )
    result = await submit_batch(
        port,
        {
            "batch_id": "verify-timeout",
            "count": 2,
            "concurrency": 1,
            "paths": ["upload"],
            "per_hop_timeout_ms": 500,
        },
    )
    status, detail = await api_json(port, "GET", "/v1/batches/verify-timeout")
    require(status == 200, "status query failed")
    first = detail["requests"][0]
    summary = result["summary"]
    assert_case(
        report,
        "proxy_read_timeout_attribution",
        summary["timeouts"] == 2
        and summary["attribution_by_fault"]["proxy_read_timeout"] == 2
        and first["details"]["attribution"]["evidence"]["initiator"] == "timer",
        {
            "attribution": first["details"]["attribution"],
            "summary": summary,
        },
    )
    assert_case(
        report,
        "timeout_keeps_available_upstream_events",
        "upstream" in first["coverage"]
        and first["coverage"]["upstream"]["event_count"] > 0,
        first["coverage"],
    )


async def _scenario_unaffected_paths(port: int, report: dict[str, Any]) -> None:
    await switch_injection(
        port,
        {
            "duration_ms": 600_000,
            "id": "verify-scoped",
            "path": "keepalive",
            "ratio": 1.0,
            "type": "upstream_disconnect",
        },
    )
    result = await submit_batch(
        port,
        {
            "batch_id": "verify-scoped",
            "count": 18,
            "concurrency": 1,
            "paths": ["short", "keepalive", "upload"],
        },
    )
    summary = result["summary"]
    assert_case(
        report,
        "uninjected_paths_are_clean",
        summary["by_path"]["short"]["resets"] == 0
        and summary["by_path"]["upload"]["resets"] == 0
        and summary["by_path"]["keepalive"]["resets"] > 0,
        summary["by_path"],
    )


async def _scenario_unexplained(port: int, report: dict[str, Any]) -> None:
    await switch_injection(
        port,
        {
            "duration_ms": 600_000,
            "id": "verify-none-before-unexplained",
            "path": "all",
            "ratio": 0.0,
            "type": "none",
        },
    )
    result = await submit_batch(
        port,
        {
            "batch_id": "verify-unexplained",
            "count": 1,
            "concurrency": 1,
            "paths": ["short"],
            "target": "dead-upstream",
        },
    )
    status, detail = await api_json(port, "GET", "/v1/batches/verify-unexplained")
    require(status == 200, "status query failed")
    request = detail["requests"][0]
    assert_case(
        report,
        "unexplained_reset_is_not_forced_into_known_class",
        result["summary"]["attribution_by_fault"]["unexplained_reset"] == 1
        and request["details"]["attribution"]["fault_type"] == "unexplained_reset",
        {
            "attribution": request["details"]["attribution"],
            "summary": result["summary"],
        },
    )
    assert_case(
        report,
        "unexplained_reset_sequence_is_queryable",
        len(detail["unexplained_reset_sequences"]) == 1
        and len(detail["unexplained_reset_sequences"][0]["events"]) > 0,
        detail["unexplained_reset_sequences"],
    )


async def _scenario_duplicate_and_metrics(port: int, report: dict[str, Any]) -> None:
    status, result = await api_json(
        port,
        "POST",
        "/v1/batches",
        {
            "batch_id": "verify-baseline",
            "count": 1,
            "concurrency": 1,
            "paths": ["short"],
        },
    )
    assert_case(
        report,
        "duplicate_batch_stable_error",
        status == 409 and result.get("code") == "BATCH_ALREADY_EXISTS",
        {"body": result, "status": status},
    )
    metrics = await api_text(port, "/metrics")
    assert_case(
        report,
        "prometheus_metrics_exposed",
        all(
            token in metrics
            for token in (
                "connectlab_batch_requests_total",
                "connectlab_batch_attributions_total",
                'fault_type="syn_not_reached_upstream"',
                'hop="upstream"',
                "connectlab_batch_unexplained_resets_total",
                "connectlab_batch_timeouts_total",
                "connectlab_batch_connection_reuses_total",
                "connectlab_batch_reuse_failures_total",
            )
        ),
        metrics,
    )


async def _scenario_deadline(port: int, report: dict[str, Any]) -> None:
    await switch_injection(
        port,
        {
            "duration_ms": 600_000,
            "id": "verify-deadline",
            "path": "upload",
            "ratio": 1.0,
            "type": "proxy_read_timeout",
        },
    )
    result = await submit_batch(
        port,
        {
            "batch_id": "verify-deadline",
            "batch_timeout_ms": 300,
            "count": 2,
            "concurrency": 1,
            "paths": ["upload"],
            "per_hop_timeout_ms": 2_000,
            "request_timeout_ms": 5_000,
        },
    )
    status, detail = await api_json(port, "GET", "/v1/batches/verify-deadline")
    require(status == 200, "status query failed")
    assert_case(
        report,
        "batch_deadline_preserves_partial_batch",
        result["status"] == "partial_timeout"
        and detail["batch"]["unfinished_count"] > 0,
        {
            "status": result["status"],
            "unfinished_count": detail["batch"]["unfinished_count"],
            "requests": [
                {
                    "outcome": request["outcome"],
                    "request_id": request["request_id"],
                }
                for request in detail["requests"]
            ],
        },
    )


async def _verify_restart(port: int, report: dict[str, Any]) -> bool:
    status, detail = await api_json(port, "GET", "/v1/batches/verify-syn")
    latest_injection = detail.get("latest_injection", {})
    ok = (
        status == 200
        and detail["batch"]["status"] == "completed"
        and detail["attribution_summary"]["attribution_by_fault"][
            "syn_not_reached_upstream"
        ]
        == 4
        and latest_injection.get("id") == "verify-deadline"
    )
    assert_case(
        report,
        "restart_preserves_batch_and_injection_history",
        ok,
        {
            "injection_id": latest_injection.get("id"),
            "status": detail.get("batch", {}).get("status"),
        },
    )
    return ok


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", default=None)
    parser.add_argument(
        "--output",
        default=str(ROOT / "artifacts" / "verification.json"),
    )
    args = parser.parse_args()
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    try:
        report = asyncio.run(run_verification(args.config, output))
    except Exception as exc:
        print(f"VERIFICATION FAILED: {exc}", file=sys.stderr)
        return 1
    print(json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True))
    return 0 if report.get("ok") else 1


if __name__ == "__main__":
    raise SystemExit(main())
