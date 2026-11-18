from __future__ import annotations

import json
import random
import socket
import statistics
import subprocess
import sys
import tempfile
import time
from collections import Counter
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import httpx

from rec_gateway.assetgen import generate_assets, percentile


ROOT = Path(__file__).resolve().parent.parent


def utc_now() -> str:
    return (
        datetime.now(timezone.utc)
        .isoformat(timespec="milliseconds")
        .replace("+00:00", "Z")
    )


@dataclass
class ScenarioOptions:
    output: Path
    tenant_id: str = "tenant-d"
    protocol_version: str = "mixed"
    count: int = 100
    rate: float = 20.0
    delay_rate: float = 0.0
    delay_ms: float = 0.0
    misalign_rate: float = 0.0
    canary_weight: float = 1.0
    seed: int = 294
    samples_path: Path = ROOT / "generated" / "samples" / "requests.jsonl"
    models_dir: Path = ROOT / "generated" / "models"
    base_url: str | None = None
    inject_load_failure: bool = False
    timeout_seconds: float = 5.0


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def _wait_ready(base_url: str, process: subprocess.Popen[bytes], timeout: float) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"gateway exited during startup with code {process.returncode}")
        try:
            response = httpx.get(
                f"{base_url}/healthz", timeout=0.5, trust_env=False
            )
            if response.status_code == 200:
                return
        except httpx.HTTPError:
            pass
        time.sleep(0.1)
    raise RuntimeError("gateway did not become ready before timeout")


def _load_samples(
    path: Path,
    tenant_id: str,
) -> dict[str, list[dict[str, Any]]]:
    grouped: dict[str, list[dict[str, Any]]] = {"v3": [], "v4": []}
    with path.open("r", encoding="utf-8") as handle:
        for line in handle:
            sample = json.loads(line)
            if sample.get("tenant_id") != tenant_id:
                continue
            protocol = sample.get("protocol_version")
            if protocol in grouped:
                grouped[protocol].append(sample)
    if not grouped["v3"] or not grouped["v4"]:
        raise RuntimeError("samples do not cover both v3 and v4 for the selected tenant")
    return grouped


def _percentiles(values: list[float]) -> dict[str, float | None]:
    return {
        "p50": percentile(values, 0.50),
        "p95": percentile(values, 0.95),
        "p99": percentile(values, 0.99),
        "max": round(max(values), 3) if values else None,
        "mean": round(statistics.fmean(values), 3) if values else None,
    }


def _write_report(path: Path, report: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(report, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )


def run_scenario(options: ScenarioOptions) -> dict[str, Any]:
    grouped = _load_samples(options.samples_path, options.tenant_id)
    rng = random.Random(options.seed)
    temporary: tempfile.TemporaryDirectory[str] | None = None
    process: subprocess.Popen[bytes] | None = None
    base_url = options.base_url
    try:
        if base_url is None:
            temporary = tempfile.TemporaryDirectory(prefix="rec-gateway-scenario-")
            temp_root = Path(temporary.name)
            models_dir = options.models_dir
            if options.inject_load_failure:
                broken_root = temp_root / "generated"
                generate_assets(
                    broken_root,
                    sample_count=20,
                    seed=options.seed,
                    inject_v4_load_failure=True,
                )
                models_dir = broken_root / "models"
            port = _free_port()
            base_url = f"http://127.0.0.1:{port}"
            config = {
                "host": "127.0.0.1",
                "port": port,
                "model_dir": str(models_dir),
                "database": str(temp_root / "scenario.sqlite3"),
                "stable_version": "v3",
                "canary_version": "v4",
                "max_batch_size": 64,
                "max_concurrency": 2,
                "request_timeout_ms": 600,
                "fault_injection_enabled": True,
                "tenants": [
                    {"tenant_id": tenant_id, "canary_weight": 0.0}
                    for tenant_id in ("tenant-a", "tenant-b", "tenant-c", "tenant-d")
                ],
            }
            config_path = temp_root / "config.json"
            config_path.write_text(
                json.dumps(config, ensure_ascii=False, sort_keys=True, indent=2),
                encoding="utf-8",
            )
            log_path = temp_root / "gateway.log"
            with log_path.open("wb") as log_handle:
                process = subprocess.Popen(
                    [
                        sys.executable,
                        "-m",
                        "rec_gateway",
                        "--config",
                        str(config_path),
                        "--log-level",
                        "warning",
                    ],
                    cwd=str(ROOT),
                    stdout=log_handle,
                    stderr=subprocess.STDOUT,
                )
            try:
                _wait_ready(base_url, process, 15.0)
            except RuntimeError:
                if log_path.exists():
                    tail = log_path.read_text(encoding="utf-8", errors="replace")[-3000:]
                    if tail:
                        print(tail, file=sys.stderr)
                raise

        with httpx.Client(
            base_url=base_url,
            timeout=options.timeout_seconds,
            trust_env=False,
        ) as client:
            weight_response = client.put(
                f"/admin/tenants/{options.tenant_id}/weights",
                json={"canary_weight": options.canary_weight},
            )
            weight_response.raise_for_status()
            baseline = client.get("/status").json()
            rollback_ids_before = {
                event["id"] for event in baseline.get("recent_rollbacks", [])
            }

            started_at = utc_now()
            scenario_started = time.perf_counter()
            targets: Counter[str] = Counter()
            served: Counter[str] = Counter()
            declared: Counter[str] = Counter()
            errors_by_code: Counter[str] = Counter()
            latencies: dict[str, list[float]] = {}
            total_errors = 0
            total_timeouts = 0
            protocol_mismatches = 0
            column_mismatches = 0
            feature_validation_rejections = 0
            index_by_protocol = {"v3": 0, "v4": 0}

            for index in range(options.count):
                if options.rate > 0:
                    target_time = scenario_started + index / options.rate
                    remaining = target_time - time.perf_counter()
                    if remaining > 0:
                        time.sleep(remaining)
                protocol = options.protocol_version
                if protocol == "mixed":
                    protocol = "v3" if index % 2 == 0 else "v4"
                candidate_samples = grouped[protocol]
                sample = candidate_samples[
                    index_by_protocol[protocol] % len(candidate_samples)
                ]
                index_by_protocol[protocol] += 1
                request = {
                    "client_id": f"scenario-client-{index % 7:02d}",
                    "feature_names": list(sample["feature_names"]),
                    "features": list(sample["features"]),
                    "protocol_version": protocol,
                    "request_id": (
                        f"scenario-{int(time.time() * 1000)}-{options.seed}-{index:06d}"
                    ),
                    "tenant_id": options.tenant_id,
                }
                if options.delay_rate > 0 and rng.random() < options.delay_rate:
                    request["fault_injection"] = {
                        "inference_delay_ms": options.delay_ms
                    }
                if options.misalign_rate > 0 and rng.random() < options.misalign_rate:
                    request["feature_names"][0], request["feature_names"][1] = (
                        request["feature_names"][1],
                        request["feature_names"][0],
                    )
                declared[protocol] += 1
                request_started = time.perf_counter()
                try:
                    response = client.post("/v1/infer", json=request)
                    elapsed_ms = round(
                        (time.perf_counter() - request_started) * 1000.0, 3
                    )
                    body = response.json()
                except httpx.HTTPError:
                    elapsed_ms = round(
                        (time.perf_counter() - request_started) * 1000.0, 3
                    )
                    response = None
                    body = {
                        "ok": False,
                        "error": {
                            "code": "CLIENT_TRANSPORT_ERROR",
                            "message": "scenario client transport failed",
                            "details": {},
                        },
                        "target_version": "unknown",
                    }
                target_version = str(
                    body.get("target_version")
                    or body.get("served_version")
                    or "unknown"
                )
                targets[target_version] += 1
                served_version = body.get("served_version")
                if served_version:
                    served[str(served_version)] += 1
                latencies.setdefault(target_version, []).append(elapsed_ms)
                if response is None or response.status_code >= 400 or not body.get("ok"):
                    total_errors += 1
                    code = str(body.get("error", {}).get("code", "UNKNOWN_ERROR"))
                    errors_by_code[code] += 1
                    if code == "INFERENCE_TIMEOUT":
                        total_timeouts += 1
                    if code == "FEATURE_PROTOCOL_MISMATCH":
                        protocol_mismatches += 1
                    if code.startswith("FEATURE_"):
                        feature_validation_rejections += 1
                    if code in {
                        "FEATURE_COLUMN_ORDER_MISMATCH",
                        "FEATURE_COLUMN_COUNT_MISMATCH",
                        "FEATURE_COLUMN_MISSING",
                        "FEATURE_COLUMN_UNEXPECTED",
                    }:
                        column_mismatches += 1

            duration_seconds = round(time.perf_counter() - scenario_started, 3)
            final_status = client.get("/status").json()
            rollback_events = [
                event
                for event in final_status.get("recent_rollbacks", [])
                if event["id"] not in rollback_ids_before
            ]
            version_states = {
                version: {
                    "status": value["status"],
                    "protocol_version": value["protocol_version"],
                    "feature_dimension": value["feature_dimension"],
                    "error_code": value["error_code"],
                }
                for version, value in final_status["versions"].items()
            }
            policies = final_status["routing"]["tenants"]

        report = {
            "schema_version": 1,
            "scenario": {
                "tenant_id": options.tenant_id,
                "protocol_version": options.protocol_version,
                "request_count": options.count,
                "configured_rate_per_second": options.rate,
                "inference_delay_rate": options.delay_rate,
                "inference_delay_ms": options.delay_ms,
                "feature_misalignment_rate": options.misalign_rate,
                "load_failure_injected": options.inject_load_failure,
                "seed": options.seed,
                "started_at": started_at,
                "finished_at": utc_now(),
                "duration_seconds": duration_seconds,
                "achieved_rate_per_second": (
                    round(options.count / duration_seconds, 3)
                    if duration_seconds
                    else None
                ),
            },
            "requests_by_declared_protocol": dict(sorted(declared.items())),
            "requests_by_target_version": dict(sorted(targets.items())),
            "responses_by_served_version": dict(sorted(served.items())),
            "latency_ms_by_target_version": {
                version: _percentiles(values)
                for version, values in sorted(latencies.items())
            },
            "errors_total": total_errors,
            "errors_by_code": dict(sorted(errors_by_code.items())),
            "timeouts_total": total_timeouts,
            "protocol_mismatch_rejections": protocol_mismatches,
            "feature_validation_rejections": feature_validation_rejections,
            "column_mismatch_rejections": column_mismatches,
            "rollback_events": sorted(
                rollback_events, key=lambda event: event["id"]
            ),
            "version_states": version_states,
            "final_weights": {
                tenant: value["canary_weight"]
                for tenant, value in sorted(policies.items())
            },
        }
        _write_report(options.output, report)
        return report
    finally:
        if process is not None:
            process.terminate()
            try:
                process.wait(timeout=5.0)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5.0)
        if temporary is not None:
            temporary.cleanup()
