from __future__ import annotations

import copy
import math
import threading
import time
from concurrent.futures import Future
from typing import Any

import grpc

from .config import RouterConfig
from .health import HealthManager, ReplicaState
from .metrics import Metrics
from .pb import inference_pb2
from .stats import RequestStats
from .store import RequestStore


def _now_ms() -> int:
    return int(time.time() * 1000)


class InferenceEngine:
    def __init__(
        self,
        config: RouterConfig,
        store: RequestStore,
        health: HealthManager,
        stats: RequestStats,
        metrics: Metrics,
    ) -> None:
        self.config = config
        self.store = store
        self.health = health
        self.stats = stats
        self.metrics = metrics
        self._inflight_lock = threading.RLock()
        self._inflight: dict[str, Future[dict[str, Any]]] = {}
        replicas_by_layer = config.replicas_by_layer()
        self._layer_groups: dict[str, list[str]] = {}
        self._input_dims: dict[str, int] = {}
        self._output_dims: dict[str, int] = {}
        for layer in config.layers:
            replicas = replicas_by_layer[layer.layer_id]
            self._layer_groups[layer.layer_id] = sorted(
                {replica.replica_group for replica in replicas}
            )
            input_dims = {replica.input_dim for replica in replicas}
            output_dims = {replica.output_dim for replica in replicas}
            if len(input_dims) != 1 or len(output_dims) != 1:
                raise ValueError(f"layer {layer.layer_id} has inconsistent declared dimensions")
            self._input_dims[layer.layer_id] = next(iter(input_dims))
            self._output_dims[layer.layer_id] = next(iter(output_dims))
        for current, following in zip(config.layers, config.layers[1:]):
            if self._output_dims[current.layer_id] != self._input_dims[following.layer_id]:
                raise ValueError(
                    f"dimension mismatch between {current.layer_id} output "
                    f"and {following.layer_id} input"
                )
        self.input_dim = self._input_dims[config.layers[0].layer_id]

    def _event_for_request(self, event: dict[str, Any], request_id: str) -> dict[str, Any]:
        event = copy.deepcopy(event)
        event["request_id"] = request_id
        self.stats.record_event(event)
        return event

    def _record_health(
        self,
        state: ReplicaState,
        latency_ms: float,
        *,
        timed_out: bool,
        failed: bool,
        request_id: str,
    ) -> list[dict[str, Any]]:
        if timed_out:
            events = self.health.record_timeout(state.spec.replica_id, latency_ms)
        elif failed:
            events = self.health.record_failure(state.spec.replica_id, latency_ms)
        else:
            events = self.health.record_success(state.spec.replica_id, latency_ms)
        return [self._event_for_request(event, request_id) for event in events]

    @staticmethod
    def _call_record(
        *,
        layer_id: str,
        replica_id: str,
        session_id: str,
        latency_ms: float,
        timed_out: bool,
        attempt: int,
        outcome: str,
        error_code: str | None = None,
    ) -> dict[str, Any]:
        return {
            "layer_id": layer_id,
            "replica_id": replica_id,
            "session_id": session_id,
            "latency_ms": round(latency_ms, 3),
            "timed_out": timed_out,
            "attempt": attempt,
            "outcome": outcome,
            "error_code": error_code,
        }

    def _invoke(
        self,
        state: ReplicaState,
        *,
        request_id: str,
        session_id: str,
        features: list[float],
        timeout_s: float,
        attempt: int,
    ) -> dict[str, Any]:
        started = time.perf_counter()
        public_layer = state.spec.layer
        prefix = "L1" if public_layer == self.config.layers[0].layer_id else "L2"
        timed_out = False
        failed = False
        error_code: str | None = None
        error_message = ""
        output: list[float] = []
        try:
            reply = state.stub.Infer(
                inference_pb2.WorkerInferRequest(
                    request_id=request_id,
                    session_id=session_id,
                    features=features,
                ),
                timeout=max(0.001, timeout_s),
            )
            latency_ms = (time.perf_counter() - started) * 1000.0
            if not reply.ok:
                failed = True
                error_code = reply.error_code or f"{prefix}_CALL_FAILED"
                error_message = reply.error_message
            elif reply.session_id != session_id or reply.request_id != request_id:
                failed = True
                error_code = "SESSION_ID_MISMATCH"
                error_message = "worker response did not preserve request/session identity"
            elif reply.replica_id != state.spec.replica_id:
                failed = True
                error_code = "REPLICA_ID_MISMATCH"
                error_message = "worker response replica id does not match endpoint"
            else:
                output = list(reply.output)
                if len(output) != self._output_dims[public_layer]:
                    failed = True
                    error_code = "WORKER_OUTPUT_DIMENSION_MISMATCH"
                    error_message = (
                        f"worker returned {len(output)} values; expected "
                        f"{self._output_dims[public_layer]}"
                    )
        except grpc.RpcError as exc:
            latency_ms = (time.perf_counter() - started) * 1000.0
            failed = True
            code = exc.code()
            timed_out = code == grpc.StatusCode.DEADLINE_EXCEEDED
            error_code = f"{prefix}_TIMEOUT" if timed_out else f"{prefix}_CALL_FAILED"
            error_message = f"{code.name}: {exc.details()}"
        except Exception as exc:
            latency_ms = (time.perf_counter() - started) * 1000.0
            failed = True
            error_code = f"{prefix}_CALL_FAILED"
            error_message = str(exc)
        health_events = self._record_health(
            state,
            latency_ms,
            timed_out=timed_out,
            failed=failed,
            request_id=request_id,
        )
        return {
            "ok": not failed,
            "output": output,
            "latency_ms": latency_ms,
            "timed_out": timed_out,
            "error_code": error_code,
            "error_message": error_message,
            "call": self._call_record(
                layer_id=public_layer,
                replica_id=state.spec.replica_id,
                session_id=session_id,
                latency_ms=latency_ms,
                timed_out=timed_out,
                attempt=attempt,
                outcome="success" if not failed else ("timeout" if timed_out else "error"),
                error_code=error_code,
            ),
            "events": health_events,
        }

    def _base_reply(self, request_id: str, session_id: str) -> dict[str, Any]:
        return {
            "ok": False,
            "request_id": request_id,
            "session_id": session_id,
            "output": [],
            "l1_replica_id": "",
            "l2_replica_id": "",
            "attempted_replicas": [],
            "calls": [],
            "e2e_ms": 0.0,
            "error_code": "",
            "error_message": "",
            "deduplicated": False,
            "retry_switches": 0,
        }

    def _finish_reply(
        self,
        reply: dict[str, Any],
        *,
        started_perf: float,
        started_wall_ms: int,
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        reply["e2e_ms"] = round((time.perf_counter() - started_perf) * 1000.0, 3)
        reply["output"] = [float(value) for value in reply.get("output", [])]
        observation = {
            "created_at_ms": started_wall_ms,
            "e2e_ms": reply["e2e_ms"],
            "ok": bool(reply["ok"]),
            "error_code": reply.get("error_code") or "",
            "calls": copy.deepcopy(reply["calls"]),
            "retry_switches": int(reply.get("retry_switches", 0)),
            "switch_layer_id": self.config.layers[-1].layer_id
            if int(reply.get("retry_switches", 0))
            else None,
        }
        self.stats.record_request(observation)
        self.metrics.record_request(observation)
        return reply, observation

    def _execute(
        self,
        *,
        request_id: str,
        session_id: str,
        features: list[float],
        started_db_ms: int | None = None,
    ) -> dict[str, Any]:
        started_perf = time.perf_counter()
        started_wall_ms = started_db_ms or _now_ms()
        deadline = started_perf + self.config.service.request_timeout_ms / 1000.0
        reply = self._base_reply(request_id, session_id)
        buffered_events: list[dict[str, Any]] = []

        def remaining_s() -> float:
            return deadline - time.perf_counter()

        def finalize_error(error_code: str, message: str) -> dict[str, Any]:
            reply["ok"] = False
            reply["output"] = []
            reply["l1_replica_id"] = ""
            reply["l2_replica_id"] = ""
            reply["error_code"] = error_code
            reply["error_message"] = message
            finished, _ = self._finish_reply(
                reply, started_perf=started_perf, started_wall_ms=started_wall_ms
            )
            return finished

        current_features = list(features)
        layers = self.config.layers
        for layer_index, layer in enumerate(layers):
            is_last = layer_index == len(layers) - 1
            if is_last:
                l2_state = self.health.select_in_layer(layer.layer_id)
                if l2_state is None:
                    finished = finalize_error(
                        "L2_UNAVAILABLE", "no available replica in second tier"
                    )
                    finished["_buffered_events"] = buffered_events
                    return finished
                group = l2_state.spec.replica_group
                attempted: set[str] = set()
                switches = 0
                l2_reply: dict[str, Any] | None = None
                for attempt in range(1, self.config.service.l2_attempts + 1):
                    remaining = remaining_s()
                    if remaining <= 0.001:
                        break
                    call_timeout = min(layer.call_timeout_ms / 1000.0, remaining)
                    result = self._invoke(
                        l2_state,
                        request_id=request_id,
                        session_id=session_id,
                        features=current_features,
                        timeout_s=call_timeout,
                        attempt=attempt,
                    )
                    attempted.add(l2_state.spec.replica_id)
                    reply["calls"].append(result["call"])
                    reply["attempted_replicas"].append(l2_state.spec.replica_id)
                    buffered_events.extend(result["events"])
                    if result["ok"]:
                        l2_reply = result
                        break
                    if attempt >= self.config.service.l2_attempts:
                        break
                    next_state = self.health.select(
                        layer.layer_id,
                        group,
                        excluded=attempted,
                    )
                    if next_state is None:
                        break
                    if remaining_s() <= 0.001:
                        break
                    switches += 1
                    event = {
                        "event_type": "retry_switch",
                        "layer_id": layer.layer_id,
                        "replica_id": next_state.spec.replica_id,
                        "request_id": request_id,
                        "details": {
                            "from_replica_id": l2_state.spec.replica_id,
                            "to_replica_id": next_state.spec.replica_id,
                            "reason": result["error_code"] or "call_failed",
                        },
                        "created_at_ms": _now_ms(),
                    }
                    buffered_events.append(event)
                    self.stats.record_event(event)
                    l2_state = next_state
                reply["retry_switches"] = switches
                if l2_reply is None:
                    l2_calls = [
                        call for call in reply["calls"] if call["layer_id"] == layer.layer_id
                    ]
                    if not l2_calls:
                        code = "L2_BUDGET_EXHAUSTED"
                        message = "request budget expired before second-tier call"
                    elif all(call["timed_out"] for call in l2_calls):
                        code = "L2_TIMEOUT"
                        message = "all declared second-tier attempts timed out"
                    elif all(call["outcome"] != "success" for call in l2_calls) and len(l2_calls) == 1:
                        code = "L2_CALL_FAILED"
                        message = "second-tier replica call failed"
                    else:
                        code = "L2_ATTEMPTS_EXHAUSTED"
                        message = "all declared second-tier attempts failed"
                    finished = finalize_error(code, message)
                    finished["_buffered_events"] = buffered_events
                    return finished
                reply["l2_replica_id"] = l2_state.spec.replica_id
                reply["output"] = l2_reply["output"]
                current_features = l2_reply["output"]
                reply["ok"] = True
                continue

            selected = self.health.select_in_layer(layer.layer_id)
            if selected is None:
                code = "L1_UNAVAILABLE" if layer_index == 0 else f"{layer.layer_id.upper()}_UNAVAILABLE"
                finished = finalize_error(code, f"no available replica in layer {layer.layer_id}")
                finished["_buffered_events"] = buffered_events
                return finished
            remaining = remaining_s()
            if remaining <= 0.001:
                finished = finalize_error("REQUEST_TIMEOUT", "request budget exhausted")
                finished["_buffered_events"] = buffered_events
                return finished
            call_timeout = min(layer.call_timeout_ms / 1000.0, remaining)
            result = self._invoke(
                selected,
                request_id=request_id,
                session_id=session_id,
                features=current_features,
                timeout_s=call_timeout,
                attempt=1,
            )
            reply["calls"].append(result["call"])
            reply["attempted_replicas"].append(selected.spec.replica_id)
            buffered_events.extend(result["events"])
            if not result["ok"]:
                prefix = "L1" if layer_index == 0 else layer.layer_id.upper()
                code = (
                    f"{prefix}_TIMEOUT"
                    if result["timed_out"]
                    else (
                        result["error_code"]
                        if result["error_code"] and result["error_code"].startswith(prefix)
                        else f"{prefix}_CALL_FAILED"
                    )
                )
                finished = finalize_error(code, result["error_message"])
                finished["_buffered_events"] = buffered_events
                return finished
            reply["l1_replica_id"] = selected.spec.replica_id
            current_features = result["output"]

        reply["ok"] = True
        reply["l2_replica_id"] = reply.get("l2_replica_id") or reply["l1_replica_id"]
        finished, _ = self._finish_reply(
            reply, started_perf=started_perf, started_wall_ms=started_wall_ms
        )
        finished["_buffered_events"] = buffered_events
        return finished

    def _complete(
        self,
        request_id: str,
        reply: dict[str, Any],
        buffered_events: list[dict[str, Any]],
    ) -> dict[str, Any]:
        persisted = copy.deepcopy(reply)
        persisted.pop("_buffered_events", None)
        persisted["deduplicated"] = False
        self.store.complete_request(
            request_id,
            ok=bool(persisted["ok"]),
            response=persisted,
            calls=persisted["calls"],
            events=buffered_events,
        )
        return persisted

    def submit(
        self, request_id: str, session_id: str, features: list[float]
    ) -> tuple[dict[str, Any], bool]:
        if (
            not isinstance(request_id, str)
            or not request_id
            or not isinstance(session_id, str)
            or not session_id
        ):
            raise ValueError("request_id and session_id are required")
        if len(features) != self.input_dim:
            raise ValueError(
                f"features length {len(features)} != expected {self.input_dim}"
            )
        if any(not math.isfinite(float(value)) for value in features):
            raise ValueError("features must all be finite")

        with self._inflight_lock:
            existing = self._inflight.get(request_id)
            if existing is not None:
                future = existing
                leader = False
            else:
                state, row = self.store.claim_request(request_id, session_id, features)
                if state == "final" and row and row.get("response") is not None:
                    reply = copy.deepcopy(row["response"])
                    reply["deduplicated"] = True
                    return reply, True
                future = Future()
                self._inflight[request_id] = future
                leader = True
                if state == "in_progress" and row is not None:
                    features = row["features"]
                    session_id = row["session_id"]

        if not leader:
            timeout_s = self.config.service.request_timeout_ms / 1000.0 + 10.0
            reply = copy.deepcopy(future.result(timeout=timeout_s))
            reply["deduplicated"] = True
            return reply, True

        buffered_events: list[dict[str, Any]] = []
        try:
            reply = self._execute(
                request_id=request_id,
                session_id=session_id,
                features=features,
            )
            buffered_events = reply.pop("_buffered_events", [])
            persisted = self._complete(request_id, reply, buffered_events)
        except Exception as exc:
            failure = self._base_reply(request_id, session_id)
            failure["e2e_ms"] = self.config.service.request_timeout_ms
            failure["error_code"] = "INTERNAL_ERROR"
            failure["error_message"] = str(exc)
            self.stats.record_request(
                {
                    "created_at_ms": _now_ms(),
                    "e2e_ms": failure["e2e_ms"],
                    "ok": False,
                    "error_code": "INTERNAL_ERROR",
                    "calls": [],
                    "retry_switches": 0,
                    "switch_layer_id": None,
                }
            )
            try:
                persisted = self._complete(request_id, failure, buffered_events)
            except Exception:
                persisted = failure
            with self._inflight_lock:
                future.set_result(persisted)
                self._inflight.pop(request_id, None)
            return persisted, False

        with self._inflight_lock:
            future.set_result(persisted)
            self._inflight.pop(request_id, None)
        return persisted, False

    def recover_pending(self) -> int:
        recovered = 0
        for pending in self.store.pending_requests():
            request_id = pending["request_id"]
            with self._inflight_lock:
                if request_id in self._inflight:
                    continue
                future: Future[dict[str, Any]] = Future()
                self._inflight[request_id] = future
            try:
                reply = self._execute(
                    request_id=request_id,
                    session_id=pending["session_id"],
                    features=pending["features"],
                    started_db_ms=pending["created_at_ms"],
                )
                buffered_events = reply.pop("_buffered_events", [])
                persisted = self._complete(request_id, reply, buffered_events)
                future.set_result(persisted)
                recovered += 1
            except Exception as exc:
                failure = self._base_reply(request_id, pending["session_id"])
                failure["error_code"] = "RECOVERY_FAILED"
                failure["error_message"] = str(exc)
                failure["e2e_ms"] = 0.0
                try:
                    persisted = self._complete(request_id, failure, [])
                    future.set_result(persisted)
                except Exception:
                    future.set_exception(exc)
                recovered += 1
            finally:
                with self._inflight_lock:
                    self._inflight.pop(request_id, None)
        return recovered
