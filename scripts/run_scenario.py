#!/usr/bin/env python3
"""Build and run the deterministic local acceptance scenario."""

from __future__ import annotations

import argparse
import json
import os
import platform
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
GO = os.environ.get("GO", shutil.which("go") or "/home/liang/go-sdk/bin/go")


def free_port(kind: str) -> int:
    if kind == "udp":
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
            sock.bind(("127.0.0.1", 0))
            return int(sock.getsockname()[1])
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def port_free(port: int, kind: str = "tcp") -> bool:
    try:
        if kind == "udp":
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
                sock.bind(("127.0.0.1", port))
            return True
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            sock.bind(("127.0.0.1", port))
        return True
    except OSError:
        return False


def choose_port(preferred: int, kind: str) -> int:
    return preferred if port_free(preferred, kind) else free_port(kind)


def wait_http(url: str, timeout_s: float) -> None:
    deadline = time.monotonic() + timeout_s
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=0.5) as response:
                if response.status == 200:
                    return
        except Exception as exc:  # noqa: BLE001
            last_error = exc
        time.sleep(0.1)
    raise RuntimeError(f"timed out waiting for {url}: {last_error}")


def stop(proc: subprocess.Popen[bytes] | None) -> None:
    if proc is None or proc.poll() is not None:
        return
    proc.terminate()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(timeout=5)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--duration", type=float, default=5.0)
    parser.add_argument("--seed", type=int, default=309)
    parser.add_argument("--devices", type=int, default=24)
    parser.add_argument("--keep-run", action="store_true")
    args = parser.parse_args()

    if not args.keep_run:
        shutil.rmtree(ROOT / "artifacts" / "run", ignore_errors=True)
    run_dir = ROOT / "artifacts" / "run"
    run_dir.mkdir(parents=True, exist_ok=True)
    bin_dir = run_dir / "bin"
    bin_dir.mkdir(parents=True, exist_ok=True)
    report_path = ROOT / "artifacts" / "scenario-report.json"
    config_path = run_dir / "edgegateway.json"

    subprocess.run(
        [GO, "build", "-trimpath", "-o", str(bin_dir / "edge-gateway"), "./cmd/edge-gateway"],
        cwd=ROOT,
        check=True,
    )
    subprocess.run(
        [GO, "build", "-trimpath", "-o", str(bin_dir / "loadgen"), "./cmd/loadgen"],
        cwd=ROOT,
        check=True,
    )

    udp_port = choose_port(4433, "udp")
    tcp_port = choose_port(8443, "tcp")
    backend_port = choose_port(18080, "tcp")
    metrics_port = choose_port(19090, "tcp")
    if len({udp_port, tcp_port, backend_port, metrics_port}) < 4:
        raise RuntimeError("could not allocate four distinct ports")

    config = {
        "udp_listen": f"127.0.0.1:{udp_port}",
        "tcp_listen": f"127.0.0.1:{tcp_port}",
        "metrics_listen": f"127.0.0.1:{metrics_port}",
        "backend_url": f"http://127.0.0.1:{backend_port}",
        "db_path": str(run_dir / "edgegateway.db"),
        "cert_file": str(run_dir / "cert.pem"),
        "key_file": str(run_dir / "key.pem"),
        # Exactly 24 active sessions makes the post-run over-limit probe real.
        "max_sessions": args.devices,
        "max_session_inflight": 8,
        "migration_window_ms": 1500,
        "rtt_threshold_ms": 80,
        "backlog_threshold": 4,
        "rate_limit_per_sec": 12,
        "backend_timeout_ms": 3000,
        "feedback_recovery_samples": 3,
        "allow_peer_override": True,
        "max_fragment_bytes": 4096,
        "max_body_bytes": 16384,
        "shutdown_timeout_ms": 5000,
    }
    config_path.write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")

    backend_log = (run_dir / "backend.log").open("wb")
    gateway_log = (run_dir / "gateway.log").open("wb")
    loadgen_log = (run_dir / "loadgen.log").open("wb")
    backend = gateway = None
    started = time.time()
    try:
        backend = subprocess.Popen(
            [
                sys.executable,
                str(ROOT / "backend" / "backend.py"),
                "--host",
                "127.0.0.1",
                "--port",
                str(backend_port),
            ],
            cwd=ROOT,
            stdout=backend_log,
            stderr=subprocess.STDOUT,
        )
        wait_http(f"http://127.0.0.1:{backend_port}/healthz", 5)

        gateway = subprocess.Popen(
            [str(bin_dir / "edge-gateway"), "-config", str(config_path)],
            cwd=ROOT,
            stdout=gateway_log,
            stderr=subprocess.STDOUT,
        )
        try:
            wait_http(f"http://127.0.0.1:{metrics_port}/healthz", 8)
        except Exception as exc:  # noqa: BLE001
            if gateway.poll() is not None:
                raise RuntimeError("gateway exited early; see artifacts/run/gateway.log") from exc
            raise

        loadgen = subprocess.run(
            [
                str(bin_dir / "loadgen"),
                "-tcp",
                f"127.0.0.1:{tcp_port}",
                "-quic",
                f"127.0.0.1:{udp_port}",
                "-backend",
                f"http://127.0.0.1:{backend_port}",
                "-duration",
                f"{args.duration}s",
                "-seed",
                str(args.seed),
                "-devices",
                str(args.devices),
                "-report",
                str(report_path),
            ],
            cwd=ROOT,
            stdout=loadgen_log,
            stderr=subprocess.STDOUT,
            timeout=max(30, args.duration * 6),
        )
        if loadgen.returncode != 0:
            raise RuntimeError("load generator failed; see artifacts/run/loadgen.log")
        with urllib.request.urlopen(
            f"http://127.0.0.1:{metrics_port}/metrics", timeout=3
        ) as response:
            (run_dir / "metrics.txt").write_bytes(response.read())
    finally:
        stop(gateway)
        stop(backend)
        backend_log.close()
        gateway_log.close()
        loadgen_log.close()

    with report_path.open(encoding="utf-8") as handle:
        report = json.load(handle)
    report["runner"] = {
        "elapsed_seconds": round(time.time() - started, 3),
        "platform": platform.platform(),
        "python": sys.version.split()[0],
        "logical_cpus": os.cpu_count(),
        "affinity_cpus": len(os.sched_getaffinity(0)) if hasattr(os, "sched_getaffinity") else None,
        "go": GO,
        "udp_port": udp_port,
        "tcp_port": tcp_port,
        "backend_port": backend_port,
        "metrics_port": metrics_port,
        "config": str(config_path.relative_to(ROOT)),
    }
    report_path.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    summary = {
        "report": str(report_path.relative_to(ROOT)),
        "ingresses": report.get("ingresses", {}),
        "session_limit": report.get("session_concurrency_limit", {}),
        "backend": report.get("backend", {}),
    }
    print(json.dumps(summary, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
