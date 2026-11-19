from __future__ import annotations

import argparse
import ctypes
import json
import math
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from http.client import HTTPConnection
from pathlib import Path

from scripts.generate_dataset import _content_for, build_specs


class PeakRssSampler:
    def __init__(self, pid: int, interval: float = 0.05):
        self.pid = pid
        self.interval = interval
        self.peak = 0
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, daemon=True)

    def start(self) -> "PeakRssSampler":
        self._thread.start()
        return self

    def stop(self) -> int:
        self._stop.set()
        self._thread.join(timeout=2)
        return max(self.peak, self._read_once())

    def _read_once(self) -> int:
        if sys.platform.startswith("win"):
            return self._read_windows()
        try:
            with open(f"/proc/{self.pid}/status", encoding="ascii") as handle:
                for line in handle:
                    if line.startswith("VmRSS:"):
                        return int(line.split()[1]) * 1024
        except OSError:
            pass
        return 0

    def _read_windows(self) -> int:
        from ctypes import POINTER, wintypes

        process_query_limited_information = 0x1000
        kernel32 = ctypes.windll.kernel32
        psapi = ctypes.windll.psapi
        kernel32.OpenProcess.argtypes = [
            wintypes.DWORD,
            wintypes.BOOL,
            wintypes.DWORD,
        ]
        kernel32.OpenProcess.restype = wintypes.HANDLE
        kernel32.CloseHandle.argtypes = [wintypes.HANDLE]
        kernel32.CloseHandle.restype = wintypes.BOOL

        class ProcessMemoryCounters(ctypes.Structure):
            _fields_ = [
                ("cb", wintypes.DWORD),
                ("PageFaultCount", wintypes.DWORD),
                ("PeakWorkingSetSize", ctypes.c_size_t),
                ("WorkingSetSize", ctypes.c_size_t),
                ("QuotaPeakPagedPoolUsage", ctypes.c_size_t),
                ("QuotaPagedPoolUsage", ctypes.c_size_t),
                ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t),
                ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
                ("PagefileUsage", ctypes.c_size_t),
                ("PeakPagefileUsage", ctypes.c_size_t),
            ]

        psapi.GetProcessMemoryInfo.argtypes = [
            wintypes.HANDLE,
            POINTER(ProcessMemoryCounters),
            wintypes.DWORD,
        ]
        psapi.GetProcessMemoryInfo.restype = wintypes.BOOL
        handle = kernel32.OpenProcess(
            process_query_limited_information, False, self.pid
        )
        if not handle:
            return 0
        try:
            counters = ProcessMemoryCounters()
            counters.cb = ctypes.sizeof(counters)
            if psapi.GetProcessMemoryInfo(
                handle, ctypes.byref(counters), counters.cb
            ):
                return int(counters.PeakWorkingSetSize)
        finally:
            kernel32.CloseHandle(handle)
        return 0

    def _run(self) -> None:
        while not self._stop.wait(self.interval):
            self.peak = max(self.peak, self._read_once())


class Client:
    def __init__(self, host: str, port: int, timeout: float = 180.0):
        self.host = host
        self.port = port
        self.timeout = timeout
        self.connection = HTTPConnection(host, port, timeout=timeout)

    def close(self) -> None:
        self.connection.close()

    def put(self, object_key: str, category: str, content: bytes) -> dict:
        from urllib.parse import quote

        path = (
            "/api/v1/objects?key="
            + quote(object_key, safe="")
            + f"&category={category}"
        )
        self.connection.request(
            "PUT",
            path,
            body=content,
            headers={"Content-Type": "application/octet-stream"},
        )
        response = self.connection.getresponse()
        raw = response.read()
        if response.status != 201:
            raise RuntimeError(f"write failed: {response.status} {raw[:500]!r}")
        return json.loads(raw.decode("utf-8"))

    def get(self, version_id: str) -> bytes:
        self.connection.request("GET", f"/api/v1/versions/{version_id}")
        response = self.connection.getresponse()
        body = response.read()
        if response.status != 200:
            raise RuntimeError(f"read failed: {response.status} {body[:500]!r}")
        return body

    def post_json(self, path: str, payload: dict) -> dict:
        body = json.dumps(payload).encode("utf-8")
        self.connection.request(
            "POST",
            path,
            body=body,
            headers={"Content-Type": "application/json"},
        )
        response = self.connection.getresponse()
        raw = response.read()
        if response.status != 200:
            raise RuntimeError(f"POST {path} failed: {response.status} {raw[:500]!r}")
        return json.loads(raw.decode("utf-8"))


def p95(samples: list[float]) -> float:
    ordered = sorted(samples)
    index = max(0, math.ceil(0.95 * len(ordered)) - 1)
    return ordered[index]


def start_server(data_dir: Path) -> tuple[subprocess.Popen, Client]:
    command = [
        sys.executable,
        "-m",
        "archive_service",
        "--data-dir",
        str(data_dir),
        "--host",
        "127.0.0.1",
        "--port",
        "0",
        "--quiet",
    ]
    process = subprocess.Popen(
        command,
        cwd=str(Path(__file__).resolve().parents[1]),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    assert process.stdout is not None
    line = process.stdout.readline()
    match = re.search(r"http://([^:]+):(\d+)", line)
    if not match:
        stderr = process.stderr.read() if process.stderr is not None else ""
        process.terminate()
        raise RuntimeError(f"server failed to start: {line!r} {stderr}")
    host, port = match.group(1), int(match.group(2))
    return process, Client(host, port)


def run(args: argparse.Namespace) -> dict:
    data_dir = Path(tempfile.mkdtemp(prefix="archive-benchmark-"))
    process, client = start_server(data_dir)
    sampler = PeakRssSampler(process.pid).start()
    try:
        write_samples: list[float] = []
        write_size = args.write_size_mib * 1024 * 1024
        content = b"W" * write_size
        written = None
        for index in range(args.write_samples):
            started = time.perf_counter()
            written = client.put(
                f"bench/write-{index}.bin", "regular", content
            )
            write_samples.append(time.perf_counter() - started)

        read_samples: list[float] = []
        read_version = client.put(
            "bench/read.bin", "regular", b"R" * (args.read_size_mib * 1024 * 1024)
        )
        for _ in range(args.read_samples):
            started = time.perf_counter()
            body = client.get(read_version["version_id"])
            elapsed = time.perf_counter() - started
            if len(body) != args.read_size_mib * 1024 * 1024:
                raise RuntimeError("read returned an unexpected length")
            read_samples.append(elapsed)

        dataset = None
        reconcile_seconds = None
        if args.full_dataset:
            specs, dataset_summary = build_specs(
                seed=args.seed,
                small_objects=120,
                medium_objects=30,
                target_mib=150,
                written_days_ago=0,
                reference_time=None,
            )
            started = time.perf_counter()
            for index, spec in enumerate(specs, start=1):
                client.put(
                    spec.object_key,
                    spec.category,
                    _content_for(args.seed, spec),
                )
                if not args.quiet and (index % 30 == 0 or index == len(specs)):
                    print(
                        f"dataset {index}/{len(specs)}",
                        file=sys.stderr,
                        flush=True,
                    )
            load_seconds = time.perf_counter() - started
            cleanup_client = Client(client.host, client.port, timeout=client.timeout)
            started = time.perf_counter()
            with ThreadPoolExecutor(max_workers=2) as executor:
                reconciliation_future = executor.submit(
                    client.post_json, "/api/v1/reconcile", {}
                )
                cleanup_future = executor.submit(
                    cleanup_client.post_json, "/api/v1/cleanup", {}
                )
                reconciliation = reconciliation_future.result()
                cleanup = cleanup_future.result()
            reconcile_seconds = time.perf_counter() - started
            cleanup_client.close()
            dataset = {
                **dataset_summary,
                "cleanup": cleanup,
                "load_seconds": round(load_seconds, 3),
                "reconciliation": reconciliation,
            }

        peak_rss = sampler.stop()
        result = {
            "dataset": dataset,
            "peak_rss_bytes": peak_rss,
            "peak_rss_mib": round(peak_rss / (1024 * 1024), 2),
            "read": {
                "p95_ms": round(p95(read_samples) * 1000, 3),
                "sample_count": len(read_samples),
                "size_mib": args.read_size_mib,
            },
            "reconciliation_seconds": (
                round(reconcile_seconds, 3)
                if reconcile_seconds is not None
                else None
            ),
            "thresholds": {
                "peak_rss_under_250_mib": peak_rss <= 250 * 1024 * 1024,
                "read_p95_under_150_ms": p95(read_samples) <= 0.150,
                "reconciliation_under_180_seconds": (
                    reconcile_seconds is None or reconcile_seconds <= 180.0
                ),
                "write_p95_under_400_ms": p95(write_samples) <= 0.400,
            },
            "write": {
                "p95_ms": round(p95(write_samples) * 1000, 3),
                "sample_count": len(write_samples),
                "size_mib": args.write_size_mib,
                "sample_version": written,
            },
        }
        return result
    finally:
        client.close()
        process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
        shutil.rmtree(data_dir, ignore_errors=True)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Measure write/read/reconcile latency and service RSS"
    )
    parser.add_argument("--write-size-mib", type=int, default=20)
    parser.add_argument("--write-samples", type=int, default=6)
    parser.add_argument("--read-size-mib", type=int, default=20)
    parser.add_argument("--read-samples", type=int, default=20)
    parser.add_argument("--full-dataset", action="store_true")
    parser.add_argument("--seed", type=int, default=283)
    parser.add_argument("--quiet", action="store_true")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    result = run(args)
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0 if all(result["thresholds"].values()) else 1


if __name__ == "__main__":
    raise SystemExit(main())
