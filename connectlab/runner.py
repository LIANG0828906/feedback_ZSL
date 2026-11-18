from __future__ import annotations

import asyncio
import copy
import json
import logging
from pathlib import Path
from typing import Any

from .attribution import (
    FAULT_PROXY_READ_TIMEOUT,
    FAULT_SYN_NOT_REACHED_UPSTREAM,
    FAULT_UNEXPLAINED_RESET,
    FAULT_UNEXPLAINED_TIMEOUT,
    FAULT_UPSTREAM_EARLY_DISCONNECT,
    attribute,
    build_coverage,
)
from .client import ClientSession, RequestPlan
from .config import sample_path, target_profile
from .db import Database, DuplicateBatchError
from .events import EventStore
from .injection import InjectionController, InjectionValidationError
from .proxy import ProxyServer
from .util import SequentialIds, mono_ms, read_json, utc_now


LOGGER = logging.getLogger("connectlab.runner")

VALID_PATHS = ("short", "keepalive", "upload")


class BatchValidationError(Exception):
    code = "INVALID_BATCH"
    http_status = 400
    message = "invalid batch request"


class BatchBusyError(Exception):
    code = "BATCH_ALREADY_RUNNING"
    http_status = 409
    message = "the same batch id is currently running"


class Runner:
    def __init__(
        self,
        config: dict[str, Any],
        database: Database,
        events: EventStore,
        injections: InjectionController,
        proxy: ProxyServer,
    ) -> None:
        self.config = config
        self.database = database
        self.events = events
        self.injections = injections
        self.proxy = proxy
        self._running: set[str] = set()
        self._running_lock = asyncio.Lock()
        self._ids = SequentialIds("c")

    async def run_batch(self, payload: dict[str, Any]) -> dict[str, Any]:
        if not isinstance(payload, dict):
            raise BatchValidationError("request body must be a JSON object")
        batch_id = str(payload.get("batch_id", "")).strip()
        if not batch_id:
            raise BatchValidationError("batch_id is required")
        if len(batch_id) > 128:
            raise BatchValidationError("batch_id must be at most 128 characters")
        if self.database.batch_exists(batch_id):
            raise DuplicateBatchError()

        async with self._running_lock:
            if batch_id in self._running:
                raise BatchBusyError()
            self._running.add(batch_id)

        try:
            return await self._run_batch_inner(batch_id, payload)
        finally:
            async with self._running_lock:
                self._running.discard(batch_id)

    async def _run_batch_inner(
        self, batch_id: str, payload: dict[str, Any]
    ) -> dict[str, Any]:
        batch_cfg = self.config["batch"]
        count = _positive_int(
            payload.get(
                "count",
                payload.get(
                    "total",
                    payload.get("total_requests", batch_cfg["default_count"]),
                ),
            ),
            "count",
        )
        if count > int(batch_cfg["max_count"]):
            raise BatchValidationError(
                f"count must not exceed {batch_cfg['max_count']}"
            )
        concurrency = _positive_int(
            payload.get("concurrency", batch_cfg["default_concurrency"]),
            "concurrency",
        )
        if concurrency > int(batch_cfg["max_concurrency"]):
            raise BatchValidationError(
                f"concurrency must not exceed {batch_cfg['max_concurrency']}"
            )
        concurrency = min(concurrency, count)
        batch_timeout_ms = _positive_int(
            payload.get("batch_timeout_ms", batch_cfg["batch_timeout_ms"]),
            "batch_timeout_ms",
        )
        per_hop_timeout_ms = _positive_int(
            payload.get("per_hop_timeout_ms", batch_cfg["per_hop_timeout_ms"]),
            "per_hop_timeout_ms",
        )
        request_timeout_ms = _positive_int(
            payload.get("request_timeout_ms", batch_cfg["request_timeout_ms"]),
            "request_timeout_ms",
        )
        explicit_paths = (
            payload.get("paths") is not None
            or payload.get("path") is not None
            or payload.get("intent") is not None
        )
        paths = payload.get("paths")
        if paths is None and payload.get("path"):
            paths = [payload["path"]]
        if paths is None and payload.get("intent"):
            intent = str(payload["intent"])
            if intent == "mixed":
                paths = list(VALID_PATHS)
            else:
                paths = [intent]
        if paths is None:
            paths = list(VALID_PATHS)
        if not isinstance(paths, list) or not paths:
            raise BatchValidationError("paths must be a non-empty array")
        paths = [str(path) for path in paths]
        unknown_paths = [path for path in paths if path not in VALID_PATHS]
        if unknown_paths:
            raise BatchValidationError(
                f"unknown paths {unknown_paths}; expected {list(VALID_PATHS)}"
            )

        try:
            target_name, target = target_profile(
                self.config, payload.get("target")
            )
        except ValueError as exc:
            raise BatchValidationError(str(exc)) from exc
        samples = self._sample_specs()
        sequence = payload.get("sequence")
        if sequence is not None:
            if not isinstance(sequence, list) or len(sequence) < count:
                raise BatchValidationError(
                    "sequence must be an array with at least count entries"
                )
            selected_paths = [str(sequence[index]) for index in range(count)]
            if any(path not in VALID_PATHS for path in selected_paths):
                raise BatchValidationError("sequence contains an unknown path")
            batch_config_sequence_source = "inline"
        elif not explicit_paths:
            seeded = read_json(
                str(self.config["samples"]["sequence_file"]),
                {},
            )
            seeded_paths = seeded.get("paths", []) if isinstance(seeded, dict) else []
            if (
                isinstance(seeded_paths, list)
                and seeded_paths
                and all(path in VALID_PATHS for path in seeded_paths)
            ):
                selected_paths = [
                    seeded_paths[index % len(seeded_paths)] for index in range(count)
                ]
                paths = sorted(set(selected_paths))
                batch_config_sequence_source = "seeded"
            else:
                selected_paths = [
                    paths[index % len(paths)] for index in range(count)
                ]
                batch_config_sequence_source = "round_robin"
        else:
            selected_paths = [paths[index % len(paths)] for index in range(count)]
            batch_config_sequence_source = "explicit"

        batch_started_at = utc_now()
        plans: list[RequestPlan] = []
        for index, path in enumerate(selected_paths):
            sample = samples[path]
            plans.append(
                RequestPlan(
                    seq=index,
                    request_id=f"{batch_id}-r{index + 1:04d}",
                    path=path,
                    method="POST" if path == "upload" else "GET",
                    body_size=sample["body_size"],
                    body_file=sample["body_file"],
                    worker_index=index % concurrency,
                    started_at=batch_started_at,
                )
            )

        injection_plan = self.injections.plan(
            batch_id=batch_id,
            request_plans=plans,
            requested_id=payload.get("injection_id"),
        )
        batch_config = {
            "batch_timeout_ms": batch_timeout_ms,
            "concurrency": concurrency,
            "count": count,
            "paths": paths,
            "per_hop_timeout_ms": per_hop_timeout_ms,
            "request_timeout_ms": request_timeout_ms,
            "sequence_source": batch_config_sequence_source,
            "target_profile": target_name,
        }

        started_mono = mono_ms()
        deadline_mono = started_mono + batch_timeout_ms
        workers: list[asyncio.Task[Any]] = []
        sessions: list[ClientSession] = []
        for worker_index in range(concurrency):
            worker_plans = [
                plan for plan in plans if plan.worker_index == worker_index
            ]
            session = ClientSession(
                batch_id=batch_id,
                proxy_host=target["proxy_host"],
                proxy_port=int(target["proxy_port"]),
                upstream_host=target["upstream_host"],
                upstream_port=int(target["upstream_port"]),
                events=self.events,
                ids=self._ids,
                per_hop_timeout_ms=per_hop_timeout_ms,
                injection=injection_plan.profile,
            )
            sessions.append(session)
            workers.append(
                asyncio.create_task(
                    self._run_worker(
                        session=session,
                        plans=worker_plans,
                        injection_plan=injection_plan,
                        deadline_mono=deadline_mono,
                        request_timeout_ms=request_timeout_ms,
                    ),
                    name=f"worker-{worker_index}",
                )
            )

        remaining = max(0.0, (deadline_mono - mono_ms()) / 1000.0)
        done, pending = await asyncio.wait(workers, timeout=remaining)
        for task in pending:
            task.cancel()
        if pending:
            await asyncio.gather(*pending, return_exceptions=True)
        await asyncio.gather(*done, return_exceptions=True)

        for worker_index, session in enumerate(sessions):
            worker_plans = [
                plan for plan in plans if plan.worker_index == worker_index
            ]
            last_request = (
                worker_plans[-1].request_id if worker_plans else f"{batch_id}-r0000"
            )
            try:
                await session.close(last_request)
            except Exception:
                LOGGER.debug("failed to close client session", exc_info=True)

        await self.proxy.close_batch(batch_id)
        await asyncio.sleep(min(0.05, max(0.0, (deadline_mono - mono_ms()) / 1000.0)))

        request_records: list[dict[str, Any]] = []
        request_event_map: dict[str, list[dict[str, Any]]] = {}
        for plan in plans:
            events = await self._collect_events(batch_id, plan)
            request_event_map[plan.request_id] = events
            record = self._request_record(batch_id, plan, events, injection_plan)
            request_records.append(record)

        unfinished_count = sum(
            1 for record in request_records if record["outcome"] == "incomplete"
        )
        completed_count = len(request_records) - unfinished_count
        summary = self._summary(
            batch_id=batch_id,
            batch_config=batch_config,
            injection_snapshot=injection_plan.snapshot(),
            request_records=request_records,
            unfinished_count=unfinished_count,
        )
        status = "partial_timeout" if unfinished_count else "completed"
        all_events = await self.events.snapshot(batch_id)
        batch_row = {
            "batch_id": batch_id,
            "status": status,
            "started_at": batch_started_at,
            "finished_at": utc_now(),
            "target_profile": target_name,
            "target": target,
            "batch_config": batch_config,
            "injection": injection_plan.snapshot(),
            "summary": summary,
            "request_count": len(request_records),
            "completed_count": completed_count,
            "unfinished_count": unfinished_count,
            "error": None
            if not unfinished_count
            else {
                "code": "BATCH_DEADLINE_REACHED",
                "message": "batch deadline reached; unfinished requests were retained",
            },
        }
        try:
            self.database.commit_batch(
                batch=batch_row,
                requests=request_records,
                events=all_events,
            )
        finally:
            await self.events.discard(batch_id)
        return {
            "batch_id": batch_id,
            "status": status,
            "summary": summary,
        }

    async def _run_worker(
        self,
        *,
        session: ClientSession,
        plans: list[RequestPlan],
        injection_plan: Any,
        deadline_mono: int,
        request_timeout_ms: int,
    ) -> None:
        try:
            for plan in plans:
                remaining_ms = deadline_mono - mono_ms()
                if remaining_ms <= 0:
                    plan.outcome = "incomplete"
                    plan.error_code = "BATCH_DEADLINE_EXCEEDED"
                    continue
                timeout_ms = min(request_timeout_ms, remaining_ms)
                try:
                    await asyncio.wait_for(
                        session.execute(plan, injection_plan.applies(plan.request_id)),
                        timeout=timeout_ms / 1000.0,
                    )
                except asyncio.TimeoutError:
                    if request_timeout_ms < remaining_ms:
                        plan.outcome = "timeout"
                        plan.error_code = "REQUEST_DEADLINE_EXCEEDED"
                    else:
                        plan.outcome = "incomplete"
                        plan.error_code = "BATCH_DEADLINE_EXCEEDED"
                    plan.finished_at = utc_now()
                    try:
                        await self.events.add(
                            batch_id=session.batch_id,
                            request_id=plan.request_id,
                            hop="client",
                            connection_id=plan.client_connection_id or "request-timeout",
                            correlation_id=(
                                f"corr-{plan.client_connection_id}"
                                if plan.client_connection_id
                                else "corr-request-timeout"
                            ),
                            kind="timeout",
                            initiator="timer",
                            details={
                                "phase": "request_deadline",
                                "timeout_ms": timeout_ms,
                            },
                        )
                    except ValueError:
                        pass
        except asyncio.CancelledError:
            raise
        finally:
            last_request_id = plans[-1].request_id if plans else session.batch_id
            try:
                await session.close(last_request_id)
            except Exception:
                LOGGER.debug("worker session close failed", exc_info=True)

    async def _collect_events(
        self, batch_id: str, plan: RequestPlan
    ) -> list[dict[str, Any]]:
        connection_ids = {
            value
            for value in (
                plan.client_connection_id,
                plan.proxy_downstream_connection_id,
                plan.proxy_upstream_connection_id,
                plan.upstream_connection_id,
            )
            if value
        }
        events = await self.events.for_request(batch_id, plan.request_id, connection_ids)
        for _ in range(3):
            before = len(connection_ids)
            for event in events:
                connection_ids.add(event["connection_id"])
                if event.get("peer_connection_id"):
                    connection_ids.add(event["peer_connection_id"])
                connection_ids.add(event["correlation_id"])
            events = await self.events.for_request(
                batch_id, plan.request_id, connection_ids
            )
            if len(connection_ids) == before:
                break
        return events

    def _request_record(
        self,
        batch_id: str,
        plan: RequestPlan,
        events: list[dict[str, Any]],
        injection_plan: Any,
    ) -> dict[str, Any]:
        proxy_down = plan.proxy_downstream_connection_id
        proxy_up = plan.proxy_upstream_connection_id
        upstream_id = plan.upstream_connection_id
        for event in events:
            if event["hop"] == "proxy" and event["kind"] in (
                "connect_success",
                "request_received",
            ):
                direction = event.get("details", {}).get("direction")
                if direction == "client_to_proxy" or event["connection_id"].startswith(
                    "p-down"
                ):
                    proxy_down = proxy_down or event["connection_id"]
            if event["hop"] == "proxy" and event["kind"] in (
                "request_forwarded",
                "reuse_attempt",
                "connect_start",
                "syn_dropped",
                "timeout",
            ):
                if event["connection_id"].startswith("p-up"):
                    proxy_up = proxy_up or event["connection_id"]
            if event["hop"] == "upstream":
                upstream_id = upstream_id or event["connection_id"]

        coverage, missing_hops, missing_events = build_coverage(
            events, plan.request_id, plan.outcome
        )
        attribution = attribute(
            request_id=plan.request_id,
            outcome=plan.outcome,
            events=events,
        )
        reuse_attempted = any(
            event["request_id"] == plan.request_id
            and event["kind"] == "reuse_attempt"
            for event in events
        )
        reuse_failure = any(
            event["request_id"] == plan.request_id
            and (
                event["kind"] == "reuse_failure"
                or event.get("details", {}).get("reuse_failure", False)
            )
            for event in events
        )
        details = dict(plan.details or {})
        details["error_code"] = plan.error_code
        details["missing_events"] = missing_events
        if not plan.finished_at:
            details["not_finished"] = True
        return {
            "batch_id": batch_id,
            "request_id": plan.request_id,
            "seq": plan.seq,
            "path": plan.path,
            "method": plan.method,
            "body_size": plan.body_size,
            "started_at": plan.started_at,
            "finished_at": plan.finished_at,
            "client_connection_id": plan.client_connection_id,
            "proxy_downstream_connection_id": proxy_down,
            "proxy_upstream_connection_id": proxy_up,
            "upstream_connection_id": upstream_id,
            "outcome": plan.outcome,
            "status_code": plan.status_code,
            "attribution_type": attribution["fault_type"],
            "attribution_hop": attribution["hop"],
            "evidence_event_id": attribution["evidence_event_id"],
            "injection_applied": plan.injection_applied,
            "reuse_attempted": reuse_attempted,
            "reuse_failure": reuse_failure,
            "missing_hops": missing_hops,
            "coverage": coverage,
            "details": {
                **details,
                "attribution": attribution,
                "injection_id": injection_plan.profile["id"],
            },
        }

    def _summary(
        self,
        *,
        batch_id: str,
        batch_config: dict[str, Any],
        injection_snapshot: dict[str, Any],
        request_records: list[dict[str, Any]],
        unfinished_count: int,
    ) -> dict[str, Any]:
        fault_counts = {
            FAULT_SYN_NOT_REACHED_UPSTREAM: 0,
            FAULT_UPSTREAM_EARLY_DISCONNECT: 0,
            FAULT_PROXY_READ_TIMEOUT: 0,
            FAULT_UNEXPLAINED_RESET: 0,
            FAULT_UNEXPLAINED_TIMEOUT: 0,
        }
        hop_counts: dict[str, int] = {}
        by_path: dict[str, dict[str, Any]] = {}
        for path in VALID_PATHS:
            by_path[path] = {
                "http_errors": 0,
                "requests": 0,
                "resets": 0,
                "successes": 0,
                "timeouts": 0,
                "unfinished": 0,
            }
        reset_count = 0
        timeout_count = 0
        success_count = 0
        http_error_count = 0
        reuse_attempts = 0
        reuse_failures = 0
        injection_applied_count = 0
        for record in request_records:
            path_stats = by_path.setdefault(
                record["path"],
                {
                    "http_errors": 0,
                    "requests": 0,
                    "resets": 0,
                    "successes": 0,
                    "timeouts": 0,
                    "unfinished": 0,
                },
            )
            path_stats["requests"] += 1
            outcome = record["outcome"]
            if outcome == "reset":
                reset_count += 1
                path_stats["resets"] += 1
            elif outcome == "timeout":
                timeout_count += 1
                path_stats["timeouts"] += 1
            elif outcome == "success":
                success_count += 1
                path_stats["successes"] += 1
            elif outcome == "http_error":
                http_error_count += 1
                path_stats["http_errors"] += 1
            elif outcome == "incomplete":
                path_stats["unfinished"] += 1
            fault = record["attribution_type"]
            if fault:
                fault_counts[fault] = fault_counts.get(fault, 0) + 1
                hop = record["attribution_hop"] or "unknown"
                hop_counts[hop] = hop_counts.get(hop, 0) + 1
            reuse_attempts += 1 if record["reuse_attempted"] else 0
            reuse_failures += 1 if record["reuse_failure"] else 0
            injection_applied_count += 1 if record["injection_applied"] else 0

        total = len(request_records)
        return {
            "attribution_by_fault": fault_counts,
            "attribution_by_hop": dict(sorted(hop_counts.items())),
            "by_path": by_path,
            "completed": total - unfinished_count,
            "deadline_exhausted": unfinished_count > 0,
            "http_errors": http_error_count,
            "injected_requests": injection_applied_count,
            "reuse_attempts": reuse_attempts,
            "reuse_failures": reuse_failures,
            "requests": total,
            "reset_rate": round(reset_count / total, 6) if total else 0.0,
            "resets": reset_count,
            "successes": success_count,
            "timeouts": timeout_count,
            "unfinished": unfinished_count,
            "unexplained_resets": fault_counts[FAULT_UNEXPLAINED_RESET],
        }

    def _sample_specs(self) -> dict[str, dict[str, Any]]:
        samples = self.config["samples"]
        specs = {
            "short": {
                "body_file": sample_path(self.config, f"short_body_{self.config['service']['seed']}.bin"),
                "body_size": int(samples["short_bytes"]),
            },
            "keepalive": {
                "body_file": sample_path(self.config, f"keepalive_body_{self.config['service']['seed']}.bin"),
                "body_size": int(samples["keepalive_bytes"]),
            },
            "upload": {
                "body_file": sample_path(self.config, f"upload_body_{self.config['service']['seed']}.bin"),
                "body_size": int(samples["upload_bytes"]),
            },
        }
        missing = [
            path
            for path, spec in specs.items()
            if spec["body_size"] and not Path(spec["body_file"]).exists()
        ]
        if missing:
            raise BatchValidationError(
                f"sample bodies are missing for {missing}; run scripts/init_workspace.py"
            )
        return specs


def _positive_int(value: Any, name: str) -> int:
    try:
        parsed = int(value)
    except (TypeError, ValueError) as exc:
        raise BatchValidationError(f"{name} must be an integer") from exc
    if parsed <= 0:
        raise BatchValidationError(f"{name} must be positive")
    return parsed
