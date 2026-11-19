"""Typed configuration loading for the supervisor, proxies and workload."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path


@dataclass(frozen=True)
class EgressConfig:
    egress_id: str
    listen_host: str
    listen_port: int
    rtt_ms: float
    loss_pct: float
    bandwidth_mbps: float
    concurrency: int


@dataclass(frozen=True)
class Injection:
    """A fault window expressed in delivered-MB milestones.

    Milestones (instead of wall-clock times) keep injection sequences
    deterministic regardless of host speed.  ``duration_mb`` is the amount of
    delivered link traffic the fault stays active for.
    """

    egress_id: str
    start_mb: float
    duration_mb: float
    rtt_ms: float | None = None
    loss_pct: float | None = None


@dataclass(frozen=True)
class HealthConfig:
    window: int = 20
    rtt_median_ms: float = 120.0
    retrans_rate: float = 0.08
    min_bad_samples: int = 4
    jitter_tolerance_ms: float = 500.0
    cooldown_ms: float = 1500.0
    # "link" uses the proxy link-emulation signals (required on loopback,
    # where the OS stack never sees real congestion); "os" uses TCP_INFO.
    source: str = "link"


@dataclass(frozen=True)
class WorkloadConfig:
    tasks: int = 4
    shards_per_task: int = 3000
    shard_sizes_kb: tuple[int, ...] = (4, 8, 16, 32, 64)
    task_concurrency: int = 4
    shard_max_attempts: int = 5
    request_timeout_s: float = 30.0


@dataclass(frozen=True)
class Config:
    seed: int
    target_host: str
    target_port: int
    service_host: str
    service_port: int
    egresses: tuple[EgressConfig, ...]
    workload: WorkloadConfig
    health: HealthConfig
    injections: tuple[Injection, ...] = field(default_factory=tuple)
    db_path: str = "data/edge_sync.db"
    target_db_path: str = "data/target.db"
    report_path: str = "artifacts/sync_report.json"

    @property
    def total_concurrency(self) -> int:
        return min(self.workload.tasks * self.workload.task_concurrency, 16)

    def egress(self, egress_id: str) -> EgressConfig:
        for egress in self.egresses:
            if egress.egress_id == egress_id:
                return egress
        raise KeyError(egress_id)


def _as_injection(raw: dict) -> Injection:
    return Injection(
        egress_id=raw["egress"],
        start_mb=float(raw["at_delivered_mb"]),
        duration_mb=float(raw["duration_mb"]),
        rtt_ms=raw.get("rtt_ms"),
        loss_pct=raw.get("loss_pct"),
    )


def load_config(path: str | Path) -> Config:
    raw = json.loads(Path(path).read_text(encoding="utf-8"))
    egresses = tuple(
        EgressConfig(
            egress_id=item["id"],
            listen_host=item.get("host", "127.0.0.1"),
            listen_port=int(item["listen_port"]),
            rtt_ms=float(item["rtt_ms"]),
            loss_pct=float(item["loss_pct"]),
            bandwidth_mbps=float(item["bandwidth_mbps"]),
            concurrency=int(item["concurrency"]),
        )
        for item in raw["egresses"]
    )
    health_raw = raw.get("health", {})
    health = HealthConfig(
        window=int(health_raw.get("window", 20)),
        rtt_median_ms=float(health_raw.get("rtt_median_ms", 120.0)),
        retrans_rate=float(health_raw.get("retrans_rate", 0.08)),
        min_bad_samples=int(health_raw.get("min_bad_samples", 4)),
        jitter_tolerance_ms=float(health_raw.get("jitter_tolerance_ms", 500.0)),
        cooldown_ms=float(health_raw.get("cooldown_ms", 1500.0)),
        source=str(health_raw.get("source", "link")),
    )
    workload_raw = raw.get("workload", {})
    workload = WorkloadConfig(
        tasks=int(workload_raw.get("tasks", 4)),
        shards_per_task=int(workload_raw.get("shards_per_task", 3000)),
        shard_sizes_kb=tuple(workload_raw.get("shard_sizes_kb", [4, 8, 16, 32, 64])),
        task_concurrency=int(workload_raw.get("task_concurrency", 4)),
        shard_max_attempts=int(workload_raw.get("shard_max_attempts", 5)),
        request_timeout_s=float(workload_raw.get("request_timeout_s", 30.0)),
    )
    injections = tuple(_as_injection(item) for item in raw.get("injections", []))
    target = raw.get("target", {})
    service = raw.get("service", {})
    return Config(
        seed=int(raw.get("seed", 291)),
        target_host=target.get("host", "127.0.0.1"),
        target_port=int(target.get("port", 8090)),
        service_host=service.get("host", "127.0.0.1"),
        service_port=int(service.get("port", 8080)),
        egresses=egresses,
        workload=workload,
        health=health,
        injections=injections,
        db_path=raw.get("db_path", "data/edge_sync.db"),
        target_db_path=raw.get("target_db_path", "data/target.db"),
        report_path=raw.get("report_path", "artifacts/sync_report.json"),
    )
