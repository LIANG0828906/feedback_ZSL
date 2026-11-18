from __future__ import annotations

import math
import threading
import time
from collections import deque
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

import grpc

from .config import HealthSpec, ReplicaSpec
from .pb import inference_pb2, inference_pb2_grpc


def percentile(values: list[float], quantile: float) -> float:
    if not values:
        return 0.0
    if len(values) == 1:
        return float(values[0])
    ordered = sorted(values)
    rank = max(0, min(len(ordered) - 1, math.ceil(quantile * len(ordered)) - 1))
    return float(ordered[rank])


@dataclass
class ReplicaState:
    spec: ReplicaSpec
    healthy: bool = True
    latencies_ms: deque[float] = field(default_factory=deque)
    call_count: int = 0
    timeout_count: int = 0
    error_count: int = 0
    eviction_count: int = 0
    consecutive_probe_failures: int = 0
    consecutive_probe_successes: int = 0
    last_eviction: dict[str, Any] | None = None
    last_recovery: dict[str, Any] | None = None
    last_probe_at_ms: int | None = None
    last_probe_ok: bool | None = None
    channel: grpc.Channel | None = None
    stub: Any = None

    def p95(self) -> float:
        return percentile(list(self.latencies_ms), 0.95)


class HealthManager:
    def __init__(
        self,
        replicas: list[ReplicaSpec],
        layers: dict[str, Any],
        health: HealthSpec,
        event_callback: Callable[[dict[str, Any]], None],
        *,
        now_ms: Callable[[], int] | None = None,
    ) -> None:
        self.health = health
        self.event_callback = event_callback
        self.now_ms = now_ms or (lambda: int(time.time() * 1000))
        self._lock = threading.RLock()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._round_robin: dict[str, int] = {}
        self._groups: dict[str, list[ReplicaState]] = {}
        self.states: dict[str, ReplicaState] = {}
        self.layer_thresholds = {
            layer_id: float(layer.p95_threshold_ms) for layer_id, layer in layers.items()
        }
        for spec in replicas:
            state = ReplicaState(spec=spec)
            state.channel = grpc.insecure_channel(
                spec.listen_address,
                options=[
                    ("grpc.max_send_message_length", 4 * 1024 * 1024),
                    ("grpc.max_receive_message_length", 4 * 1024 * 1024),
                ],
            )
            state.stub = inference_pb2_grpc.WorkerInferenceStub(state.channel)
            self.states[spec.replica_id] = state
            self._groups.setdefault(self._group_key(spec.layer, spec.replica_group), []).append(state)
        for group_states in self._groups.values():
            group_states.sort(key=lambda state: state.spec.replica_id)

    @staticmethod
    def _group_key(layer_id: str, replica_group: str) -> str:
        return f"{layer_id}/{replica_group}"

    def _evict(self, state: ReplicaState, reason: str, detail: dict[str, Any]) -> dict[str, Any]:
        now = self.now_ms()
        state.healthy = False
        state.eviction_count += 1
        state.latencies_ms.clear()
        state.consecutive_probe_successes = 0
        event = {
            "event_type": "evict",
            "layer_id": state.spec.layer,
            "replica_id": state.spec.replica_id,
            "details": {**detail, "reason": reason},
            "created_at_ms": now,
        }
        state.last_eviction = {
            "reason": reason,
            "created_at_ms": now,
            **detail,
        }
        return event

    def _recover(self, state: ReplicaState) -> dict[str, Any]:
        now = self.now_ms()
        state.healthy = True
        state.consecutive_probe_failures = 0
        state.consecutive_probe_successes = 0
        state.latencies_ms.clear()
        event = {
            "event_type": "recover",
            "layer_id": state.spec.layer,
            "replica_id": state.spec.replica_id,
            "details": {"reason": "consecutive_probe_success"},
            "created_at_ms": now,
        }
        state.last_recovery = {
            "reason": "consecutive_probe_success",
            "created_at_ms": now,
        }
        return event

    def record_call(self, replica_id: str, latency_ms: float, *, timed_out: bool) -> list[dict[str, Any]]:
        with self._lock:
            state = self.states[replica_id]
            state.call_count += 1
            if timed_out:
                state.timeout_count += 1
                state.error_count += 1
            if not state.healthy:
                return []
            state.latencies_ms.append(float(latency_ms))
            while len(state.latencies_ms) > self.health.window_size:
                state.latencies_ms.popleft()
            if (
                len(state.latencies_ms) >= self.health.min_samples
                and state.p95() > float(
                    self._layer_threshold_ms(state.spec.layer)
                )
            ):
                return [
                    self._evict(
                        state,
                        "window_p95_exceeded",
                        {
                            "p95_ms": state.p95(),
                            "threshold_ms": self._layer_threshold_ms(state.spec.layer),
                            "window_samples": self.health.window_size,
                        },
                    )
                ]
            return []

    def _layer_threshold_ms(self, layer_id: str) -> float:
        # The caller injects the threshold lookup at construction time via this attribute.
        return float(self.layer_thresholds[layer_id])

    def record_success(self, replica_id: str, latency_ms: float) -> list[dict[str, Any]]:
        return self.record_call(replica_id, latency_ms, timed_out=False)

    def record_timeout(self, replica_id: str, latency_ms: float) -> list[dict[str, Any]]:
        return self.record_call(replica_id, latency_ms, timed_out=True)

    def record_failure(self, replica_id: str, latency_ms: float) -> list[dict[str, Any]]:
        with self._lock:
            state = self.states[replica_id]
            state.error_count += 1
            state.call_count += 1
            return []

    def on_probe_result(self, replica_id: str, ok: bool, error: str | None = None) -> list[dict[str, Any]]:
        with self._lock:
            state = self.states[replica_id]
            state.last_probe_at_ms = self.now_ms()
            state.last_probe_ok = ok
            if ok:
                state.consecutive_probe_failures = 0
                if state.healthy:
                    state.consecutive_probe_successes = 0
                    return []
                state.consecutive_probe_successes += 1
                if state.consecutive_probe_successes >= self.health.probe_successes_to_recover:
                    return [self._recover(state)]
                return []
            state.consecutive_probe_successes = 0
            state.consecutive_probe_failures += 1
            if (
                state.healthy
                and state.consecutive_probe_failures >= self.health.probe_failures_to_evict
            ):
                return [
                    self._evict(
                        state,
                        "continuous_probe_failure",
                        {
                            "consecutive_failures": state.consecutive_probe_failures,
                            "probe_error": error or "probe failed",
                        },
                    )
                ]
            return []

    def probe_once(self) -> list[dict[str, Any]]:
        with self._lock:
            states = list(self.states.values())
        events: list[dict[str, Any]] = []
        for state in states:
            try:
                reply = state.stub.Health(
                    inference_pb2.HealthRequest(),
                    timeout=self.health.probe_timeout_ms / 1000.0,
                )
                ok = (
                    bool(reply.ok)
                    and reply.replica_id == state.spec.replica_id
                    and reply.layer_id == state.spec.layer
                )
                error = None if ok else "invalid health response"
            except grpc.RpcError as exc:
                ok = False
                error = f"{exc.code().name}: {exc.details()}"
            events.extend(self.on_probe_result(state.spec.replica_id, ok, error))
        for event in events:
            self.event_callback(event)
        return events

    def _probe_loop(self) -> None:
        while not self._stop.is_set():
            self.probe_once()
            self._stop.wait(self.health.probe_interval_ms / 1000.0)

    def start_probes(self) -> None:
        if self._thread and self._thread.is_alive():
            return
        self._stop.clear()
        self._thread = threading.Thread(target=self._probe_loop, name="replica-probes", daemon=True)
        self._thread.start()

    def stop_probes(self) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=2.0)

    def select(
        self,
        layer_id: str,
        replica_group: str,
        *,
        excluded: set[str] | None = None,
    ) -> ReplicaState | None:
        excluded = excluded or set()
        key = self._group_key(layer_id, replica_group)
        with self._lock:
            candidates = [state for state in self._groups.get(key, []) if state.healthy and state.spec.replica_id not in excluded]
            if not candidates:
                return None
            index = self._round_robin.get(key, 0) % len(candidates)
            self._round_robin[key] = index + 1
            return candidates[index]

    def select_in_layer(
        self,
        layer_id: str,
        *,
        excluded: set[str] | None = None,
    ) -> ReplicaState | None:
        excluded = excluded or set()
        with self._lock:
            groups = sorted(
                {
                    state.spec.replica_group
                    for state in self.states.values()
                    if state.spec.layer == layer_id
                }
            )
            eligible_groups = [
                group
                for group in groups
                if any(
                    state.healthy
                    and state.spec.replica_id not in excluded
                    for state in self._groups[self._group_key(layer_id, group)]
                )
            ]
            if not eligible_groups:
                return None
            cursor_key = f"{layer_id}/*"
            index = self._round_robin.get(cursor_key, 0) % len(eligible_groups)
            self._round_robin[cursor_key] = index + 1
            group = eligible_groups[index]
        return self.select(layer_id, group, excluded=excluded)

    def available_count(self, layer_id: str) -> int:
        with self._lock:
            return sum(
                1
                for state in self.states.values()
                if state.spec.layer == layer_id and state.healthy
            )

    def preference(self, layer_id: str, replica_group: str) -> dict[str, Any]:
        key = self._group_key(layer_id, replica_group)
        with self._lock:
            candidates = [
                state.spec.replica_id
                for state in self._groups.get(key, [])
                if state.healthy
            ]
            index = self._round_robin.get(key, 0) % len(candidates) if candidates else 0
            return {
                "next_replica_id": candidates[index] if candidates else None,
                "available_replica_ids": sorted(candidates),
            }

    def status(self) -> dict[str, Any]:
        with self._lock:
            replicas = []
            for state in sorted(self.states.values(), key=lambda item: item.spec.replica_id):
                replicas.append(
                    {
                        "layer_id": state.spec.layer,
                        "replica_group": state.spec.replica_group,
                        "replica_id": state.spec.replica_id,
                        "listen_address": state.spec.listen_address,
                        "available": state.healthy,
                        "status": "available" if state.healthy else "evicted",
                        "window_samples": len(state.latencies_ms),
                        "p95_ms": round(state.p95(), 3),
                        "call_count": state.call_count,
                        "timeout_count": state.timeout_count,
                        "error_count": state.error_count,
                        "eviction_count": state.eviction_count,
                        "consecutive_probe_failures": state.consecutive_probe_failures,
                        "consecutive_probe_successes": state.consecutive_probe_successes,
                        "last_probe_at_ms": state.last_probe_at_ms,
                        "last_probe_ok": state.last_probe_ok,
                        "last_eviction": state.last_eviction,
                        "last_recovery": state.last_recovery,
                    }
                )
            preferences = {}
            seen_groups: set[str] = set()
            for state in sorted(self.states.values(), key=lambda item: (item.spec.layer, item.spec.replica_group)):
                if state.spec.replica_group in seen_groups:
                    continue
                seen_groups.add(state.spec.replica_group)
                preferences[state.spec.replica_group] = {
                    "layer_id": state.spec.layer,
                    **self.preference(state.spec.layer, state.spec.replica_group),
                }
            return {
                "replicas": replicas,
                "routing_preference": preferences,
                "last_eviction": max(
                    (item["last_eviction"] for item in replicas if item["last_eviction"]),
                    key=lambda item: item["created_at_ms"],
                    default=None,
                ),
            }

    def close(self) -> None:
        self.stop_probes()
        with self._lock:
            for state in self.states.values():
                if state.channel is not None:
                    state.channel.close()
