from __future__ import annotations

import os
import subprocess
import threading
import time
import uuid
from collections import OrderedDict
from contextlib import contextmanager
from dataclasses import replace
from typing import Any, Callable, Iterator, Mapping

from .config import ServiceConfig
from .db import (
    DatabaseUnavailableError,
    PoolExhaustedError,
    is_connection_error,
)
from .errors import (
    AppError,
    connection_pool_exhausted,
    consistency_error,
    invalid_request,
    read_unavailable,
)
from .metrics import MetricsCollector
from .replication import MonitorSnapshot, ReplicaMonitor
from .repository import (
    OrderGateway,
    classify_difference,
    field_differences,
    prepare_order,
)
from .topology import TopologySnapshot, TopologyValidator


_SESSION_PATTERN_MAX = 64
_SWITCH_ABORTED = 409


def _header(headers: Mapping[str, str], name: str) -> str | None:
    folded = name.casefold()
    for key, value in headers.items():
        if key.casefold() == folded:
            return value
    return None


def _optional_watermark_text(value: int | None) -> str | None:
    return str(value) if value is not None else None


class SessionRegistry:
    def __init__(self, maximum_sessions: int = 10000) -> None:
        self._maximum = maximum_sessions
        self._sessions: OrderedDict[str, int] = OrderedDict()
        self._lock = threading.Lock()

    def advance(self, session_id: str, watermark: int) -> None:
        with self._lock:
            current = self._sessions.get(session_id, 0)
            self._sessions[session_id] = max(current, int(watermark))
            self._sessions.move_to_end(session_id)
            while len(self._sessions) > self._maximum:
                self._sessions.popitem(last=False)

    def watermark(self, session_id: str | None) -> int:
        if not session_id:
            return 0
        with self._lock:
            value = self._sessions.get(session_id, 0)
            if session_id in self._sessions:
                self._sessions.move_to_end(session_id)
            return value


class WriteGate:
    def __init__(self) -> None:
        self._condition = threading.Condition(threading.RLock())
        self._closing = False
        self._active = 0

    @contextmanager
    def slot(self) -> Iterator[None]:
        with self._condition:
            if self._closing:
                raise AppError(
                    "E_SWITCHING",
                    "A controlled role switch is in progress",
                    503,
                )
            self._active += 1
        try:
            yield
        finally:
            with self._condition:
                self._active -= 1
                self._condition.notify_all()

    def close_and_drain(self, timeout_seconds: float) -> bool:
        deadline = time.monotonic() + max(timeout_seconds, 0)
        with self._condition:
            self._closing = True
            while self._active:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    self._closing = False
                    self._condition.notify_all()
                    return False
                self._condition.wait(timeout=remaining)
            return True

    def open(self) -> None:
        with self._condition:
            self._closing = False
            self._condition.notify_all()

    @property
    def closed(self) -> bool:
        with self._condition:
            return self._closing


class OrderService:
    def __init__(
        self,
        config: ServiceConfig,
        primary_gateway: OrderGateway,
        replica_gateway: OrderGateway,
        monitor: ReplicaMonitor,
        metrics: MetricsCollector,
        topology: TopologySnapshot,
        *,
        command_runner: Callable[
            [tuple[str, ...], dict[str, str], int], int
        ]
        | None = None,
        topology_validator_factory: Callable[
            [ServiceConfig], TopologyValidator
        ]
        | None = None,
    ) -> None:
        self.config = config
        self._primary_gateway = primary_gateway
        self._replica_gateway = replica_gateway
        self._monitor = monitor
        self.metrics = metrics
        self.topology = topology
        self.sessions = SessionRegistry()
        self.write_gate = WriteGate()
        self._role_lock = threading.RLock()
        self._switch_lock = threading.Lock()
        self._command_runner = command_runner or self._run_external_command
        self._topology_validator_factory = (
            topology_validator_factory or TopologyValidator
        )
        self._role_generation = 0

    def write_order(
        self, payload: Any, headers: Mapping[str, str]
    ) -> dict[str, Any]:
        started = time.monotonic()
        session_id = self._session_id(headers, generate=True)
        try:
            order = prepare_order(payload)
            with self.write_gate.slot():
                primary = self._primary()
                watermark = primary.write_order(order)
                ack_time = time.monotonic()
                self._monitor.record_write(watermark, ack_time)
                self.sessions.advance(session_id, watermark)
        except AppError as exc:
            self.metrics.record_write(
                int((time.monotonic() - started) * 1000),
                success=False,
                code=exc.code,
            )
            raise
        except Exception:
            self.metrics.record_write(
                int((time.monotonic() - started) * 1000),
                success=False,
                code="E_WRITE_UNAVAILABLE",
            )
            raise AppError(
                "E_WRITE_UNAVAILABLE",
                "Primary write failed",
                503,
            ) from None

        latency_ms = int((time.monotonic() - started) * 1000)
        self.metrics.record_write(latency_ms, success=True)
        return {
            "data": {
                "order_no": order["order_no"],
                "commit_watermark": str(watermark),
                "session_id": session_id,
            },
            "commit_watermark": str(watermark),
            "session_id": session_id,
        }

    def read_order(
        self,
        order_no: str,
        consistency: str,
        headers: Mapping[str, str],
    ) -> dict[str, Any]:
        level = (consistency or "").strip().lower()
        started = time.monotonic()
        if level not in {"strong", "session", "eventual"}:
            self.metrics.record_invalid_consistency(
                int((time.monotonic() - started) * 1000)
            )
            snapshot = self._monitor.snapshot()
            raise consistency_error(
                "consistency must be strong, session, or eventual",
                replica_applied_watermark=snapshot.replica_watermark,
                reason="invalid_consistency_level",
            )
        degraded = False
        try:
            data, meta, degraded = self._route_read(
                order_no, level, headers, detail=True
            )
        except AppError as exc:
            self.metrics.record_read(
                level,
                int((time.monotonic() - started) * 1000),
                success=False,
                consistency_error=exc.code == "E_CONSISTENCY",
                degraded_primary=degraded,
                read_error=exc.code.startswith("E_READ"),
            )
            raise
        self.metrics.record_read(
            level,
            int((time.monotonic() - started) * 1000),
            success=True,
            degraded_primary=degraded,
        )
        return {"data": data, "consistency": meta}

    def list_orders(
        self,
        *,
        tenant_id: str,
        status: str,
        page: int,
        page_size: int,
        consistency: str,
        headers: Mapping[str, str],
    ) -> dict[str, Any]:
        started = time.monotonic()
        level = (consistency or "").strip().lower()
        if level not in {"strong", "session", "eventual"}:
            self.metrics.record_invalid_consistency(
                int((time.monotonic() - started) * 1000)
            )
            snapshot = self._monitor.snapshot()
            raise consistency_error(
                "consistency must be strong, session, or eventual",
                replica_applied_watermark=snapshot.replica_watermark,
                reason="invalid_consistency_level",
            )
        tenant_id = tenant_id.strip()
        status = status.strip().upper()
        if not tenant_id or len(tenant_id) > 32:
            raise invalid_request("tenant_id must contain 1 to 32 characters")
        if status not in {"PENDING", "CONFIRMED", "CANCELLED"}:
            raise invalid_request("status must be PENDING, CONFIRMED, or CANCELLED")
        if page < 1:
            raise invalid_request("page must be at least 1")
        if page_size < 1 or page_size > self.config.server.max_page_size:
            raise invalid_request(
                f"page_size must be between 1 and {self.config.server.max_page_size}"
            )

        degraded = False
        try:
            data, meta, degraded = self._route_read(
                (tenant_id, status, page, page_size),
                level,
                headers,
                detail=False,
            )
        except AppError as exc:
            self.metrics.record_read(
                level,
                int((time.monotonic() - started) * 1000),
                success=False,
                consistency_error=exc.code == "E_CONSISTENCY",
                degraded_primary=degraded,
                read_error=exc.code.startswith("E_READ"),
            )
            raise
        self.metrics.record_read(
            level,
            int((time.monotonic() - started) * 1000),
            success=True,
            degraded_primary=degraded,
        )
        return {"data": data, "consistency": meta}

    def _route_read(
        self,
        key: Any,
        level: str,
        headers: Mapping[str, str],
        *,
        detail: bool,
    ) -> tuple[Any, dict[str, Any], bool]:
        if level == "strong":
            primary = self._primary()
            data = self._read_gateway(primary, key, detail=detail, role="primary")
            snapshot = self._monitor.snapshot()
            return data, self._meta(level, "primary", snapshot), False

        if level == "session":
            required = self._session_required_watermark(headers)
            snapshot = self._monitor.wait_for_watermark(required)
            if (
                not snapshot.replica_reachable
                or snapshot.replica_watermark is None
                or snapshot.replica_watermark < required
            ):
                raise consistency_error(
                    "Session watermark is not available on the replica",
                    required_watermark=required,
                    replica_applied_watermark=snapshot.replica_watermark,
                    reason=(
                        "replica_unavailable"
                        if not snapshot.replica_reachable
                        else "watermark_not_reached"
                    ),
                )
            replica = self._replica()
            try:
                data = self._read_gateway(
                    replica, key, detail=detail, role="replica"
                )
            except AppError as exc:
                if exc.code == "E_CONN_POOL":
                    raise
                raise consistency_error(
                    "Replica read failed after watermark check",
                    required_watermark=required,
                    replica_applied_watermark=snapshot.replica_watermark,
                    reason="replica_read_failed",
                ) from None
            self._compare_if_enabled(data, detail=detail)
            return (
                data,
                self._meta(
                    level,
                    "replica",
                    snapshot,
                    required_watermark=required,
                ),
                False,
            )

        snapshot = self._monitor.snapshot()
        if snapshot.degraded_reason is not None:
            primary = self._primary()
            data = self._read_gateway(primary, key, detail=detail, role="primary")
            return (
                data,
                self._meta(
                    level,
                    "primary",
                    snapshot,
                    degraded_reason=snapshot.degraded_reason,
                ),
                True,
            )
        if not snapshot.replica_reachable:
            primary = self._primary()
            data = self._read_gateway(primary, key, detail=detail, role="primary")
            return (
                data,
                self._meta(
                    level,
                    "primary",
                    snapshot,
                    degraded_reason="replica_unavailable",
                ),
                True,
            )
        replica = self._replica()
        try:
            data = self._read_gateway(
                replica, key, detail=detail, role="replica"
            )
        except AppError:
            primary = self._primary()
            data = self._read_gateway(primary, key, detail=detail, role="primary")
            return (
                data,
                self._meta(
                    level,
                    "primary",
                    snapshot,
                    degraded_reason="replica_read_failed",
                ),
                True,
            )
        self._compare_if_enabled(data, detail=detail)
        return data, self._meta(level, "replica", snapshot), False

    def _read_gateway(
        self,
        gateway: OrderGateway,
        key: Any,
        *,
        detail: bool,
        role: str,
    ) -> Any:
        try:
            if detail:
                return gateway.fetch_order(str(key))
            tenant_id, status, page, page_size = key
            return gateway.list_orders(
                tenant_id, status, page=page, page_size=page_size
            )
        except PoolExhaustedError:
            raise connection_pool_exhausted() from None
        except DatabaseUnavailableError:
            raise read_unavailable(
                f"{role.capitalize()} database is unavailable"
            ) from None
        except Exception as exc:
            if is_connection_error(exc):
                raise read_unavailable(
                    f"{role.capitalize()} database is unavailable"
                ) from None
            raise read_unavailable(
                f"{role.capitalize()} read failed"
            ) from None

    def _compare_if_enabled(self, replica_data: Any, *, detail: bool) -> None:
        if not self.config.server.compare_on_replica_read:
            return
        snapshot = self._monitor.snapshot()
        try:
            primary = self._primary()
            if detail:
                order_no = str(replica_data.get("order_no")) if replica_data else ""
                primary_data = (
                    primary.fetch_order(order_no) if order_no else None
                )
                self._record_comparison(
                    order_no, primary_data, replica_data, snapshot
                )
                return
            items = replica_data.get("items", [])
            if not items:
                return
            page = int(replica_data["page"])
            page_size = int(replica_data["page_size"])
            first = items[0]
            primary_page = primary.list_orders(
                str(first["tenant_id"]),
                str(first["status"]),
                page=page,
                page_size=page_size,
            )
            primary_by_key = {
                str(item["order_no"]): item
                for item in primary_page.get("items", [])
            }
            for replica_item in items:
                order_no = str(replica_item["order_no"])
                self._record_comparison(
                    order_no,
                    primary_by_key.get(order_no),
                    replica_item,
                    snapshot,
                )
        except Exception:
            return

    def _record_comparison(
        self,
        order_no: str,
        primary_data: dict[str, Any] | None,
        replica_data: dict[str, Any] | None,
        snapshot: MonitorSnapshot,
    ) -> None:
        differences = field_differences(primary_data, replica_data)
        if not differences:
            return
        classification = classify_difference(primary_data, replica_data)
        self.metrics.record_inconsistency(
            order_no,
            differences,
            classification=classification,
            primary_watermark=(
                int(primary_data["write_watermark"])
                if primary_data and primary_data.get("write_watermark") is not None
                else snapshot.primary_watermark
            ),
            replica_watermark=snapshot.replica_watermark,
        )

    def compare_order(self, order_no: str) -> dict[str, Any]:
        primary_data = self._read_gateway(
            self._primary(), order_no, detail=True, role="primary"
        )
        replica_data = self._read_gateway(
            self._replica(), order_no, detail=True, role="replica"
        )
        differences = field_differences(primary_data, replica_data)
        snapshot = self._monitor.snapshot()
        classification = classify_difference(primary_data, replica_data)
        if differences:
            self.metrics.record_inconsistency(
                order_no,
                differences,
                classification=classification,
                primary_watermark=(
                    int(primary_data["write_watermark"])
                    if primary_data
                    else snapshot.primary_watermark
                ),
                replica_watermark=snapshot.replica_watermark,
            )
        return {
            "order_no": order_no,
            "equal": not differences,
            "classification": classification if differences else "equal",
            "differences": differences,
            "primary_watermark": _optional_watermark_text(
                snapshot.primary_watermark
            ),
            "replica_applied_watermark": _optional_watermark_text(
                snapshot.replica_watermark
            ),
        }

    def _meta(
        self,
        level: str,
        served_by: str,
        snapshot: MonitorSnapshot,
        *,
        required_watermark: int | None = None,
        degraded_reason: str | None = None,
    ) -> dict[str, Any]:
        return {
            "level": level,
            "served_by": served_by,
            "replication_mode": self.config.replication.mode,
            "primary_watermark": _optional_watermark_text(
                snapshot.primary_watermark
            ),
            "replica_applied_watermark": _optional_watermark_text(
                snapshot.replica_watermark
            ),
            "watermark_gap": _optional_watermark_text(snapshot.watermark_gap),
            "required_watermark": _optional_watermark_text(required_watermark),
            "degraded": degraded_reason is not None,
            "degraded_reason": degraded_reason,
        }

    def _session_id(
        self, headers: Mapping[str, str], *, generate: bool
    ) -> str | None:
        value = _header(headers, "X-Session-Id")
        if value is not None:
            value = value.strip()
            if (
                not value
                or len(value) > _SESSION_PATTERN_MAX
                or any(
                    not (character.isalnum() or character in "_.:-")
                    for character in value
                )
            ):
                raise invalid_request("X-Session-Id has an invalid format")
            return value
        if generate:
            return f"sess-{uuid.uuid4().hex}"
        return None

    def _session_required_watermark(self, headers: Mapping[str, str]) -> int:
        session_id = self._session_id(headers, generate=False)
        observed = self.sessions.watermark(session_id)
        header_value = _header(headers, "X-Min-Watermark")
        if header_value is None:
            return observed
        try:
            requested = int(header_value)
        except ValueError:
            raise invalid_request("X-Min-Watermark must be a non-negative integer") from None
        if requested < 0:
            raise invalid_request("X-Min-Watermark must be a non-negative integer")
        return max(observed, requested)

    def _primary(self) -> OrderGateway:
        with self._role_lock:
            return self._primary_gateway

    def _replica(self) -> OrderGateway:
        with self._role_lock:
            return self._replica_gateway

    def status(self) -> dict[str, Any]:
        snapshot = self._monitor.snapshot()
        return {
            "replication_mode": self.config.replication.mode,
            "role_generation": self._role_generation,
            "write_routing": "primary_only",
            "write_gate_closed": self.write_gate.closed,
            "topology_validated": True,
            "schema_digest": self.topology.schema_digest,
            "connection_pools": {
                "primary": {
                    "max_connections": self.config.primary.pool_size,
                    "acquire_timeout_ms": (
                        self.config.primary.pool_acquire_timeout_ms
                    ),
                },
                "replica": {
                    "max_connections": self.config.replica.pool_size,
                    "acquire_timeout_ms": (
                        self.config.replica.pool_acquire_timeout_ms
                    ),
                },
                "monitor": "dedicated_connection",
            },
            "replication": snapshot.as_dict(),
        }

    def controlled_switch(self) -> dict[str, Any]:
        if not self._switch_lock.acquire(blocking=False):
            raise AppError(
                "E_SWITCHING", "A controlled role switch is already running", 503
            )
        started = time.monotonic()
        gate_closed = False
        try:
            gate_closed = True
            if not self.write_gate.close_and_drain(
                self.config.switch.timeout_ms / 1000.0
            ):
                raise AppError(
                    "E_SWITCH_ABORTED",
                    "In-flight writes did not drain before the switch timeout",
                    _SWITCH_ABORTED,
                    {"reason": "write_drain_timeout"},
                )
            try:
                target = self._primary().current_watermark()
            except Exception:
                raise AppError(
                    "E_SWITCH_ABORTED",
                    "Last primary commit watermark could not be read",
                    _SWITCH_ABORTED,
                    {"reason": "primary_watermark_unavailable"},
                ) from None
            snapshot = self._monitor.wait_for_watermark(
                target, self.config.switch.timeout_ms
            )
            if (
                not snapshot.replica_reachable
                or snapshot.replica_watermark is None
                or snapshot.replica_watermark < target
            ):
                raise AppError(
                    "E_SWITCH_ABORTED",
                    "Replica did not reach the final primary watermark",
                    _SWITCH_ABORTED,
                    {
                        "reason": "watermark_not_reached",
                        "last_primary_watermark": str(target),
                        "replica_applied_watermark": _optional_watermark_text(
                            snapshot.replica_watermark
                        ),
                    },
                )
            if self.config.switch.reverse_command:
                self._invoke_reverse_command(target)
                self._validate_after_switch()
            elif not self.config.switch.allow_logical_switch:
                raise AppError(
                    "E_SWITCH_ABORTED",
                    "No physical role reversal command is configured",
                    _SWITCH_ABORTED,
                    {"reason": "switch_command_missing"},
                )

            with self._role_lock:
                self._primary_gateway, self._replica_gateway = (
                    self._replica_gateway,
                    self._primary_gateway,
                )
                self.config = replace(
                    self.config,
                    primary=self.config.replica,
                    replica=self.config.primary,
                )
                self._role_generation += 1
            self._monitor.swap_roles()
            duration_ms = int((time.monotonic() - started) * 1000)
            self.metrics.record_switch(duration_ms, success=True)
            gate_closed = False
            self.write_gate.open()
            return {
                "switched": True,
                "role_generation": self._role_generation,
                "previous_primary_watermark": str(target),
                "new_primary_watermark": _optional_watermark_text(
                    snapshot.replica_watermark
                ),
                "duration_ms": duration_ms,
                "write_routing": "new_primary_only",
            }
        except AppError as exc:
            self.metrics.record_switch(
                int((time.monotonic() - started) * 1000), success=False
            )
            raise
        except Exception:
            self.metrics.record_switch(
                int((time.monotonic() - started) * 1000), success=False
            )
            raise AppError(
                "E_SWITCH_ABORTED",
                "Controlled switch failed",
                _SWITCH_ABORTED,
                {"reason": "switch_failed"},
            ) from None
        finally:
            if gate_closed:
                self.write_gate.open()
            self._switch_lock.release()

    def _invoke_reverse_command(self, target_watermark: int) -> None:
        environment = os.environ.copy()
        environment.update(
            {
                "ORDERSYNC_OLD_PRIMARY_CONNECTION_STRING": (
                    self.config.primary.connection_string
                ),
                "ORDERSYNC_OLD_PRIMARY_DATABASE": self.config.primary.database,
                "ORDERSYNC_OLD_REPLICA_CONNECTION_STRING": (
                    self.config.replica.connection_string
                ),
                "ORDERSYNC_OLD_REPLICA_DATABASE": self.config.replica.database,
                "ORDERSYNC_REPLICATION_MODE": self.config.replication.mode,
                "ORDERSYNC_LAST_COMMIT_WATERMARK": str(target_watermark),
            }
        )
        try:
            return_code = self._command_runner(
                self.config.switch.reverse_command,
                environment,
                self.config.switch.command_timeout_seconds,
            )
        except subprocess.TimeoutExpired:
            self._rollback_after_failed_switch()
            raise AppError(
                "E_SWITCH_ABORTED",
                "Physical role reversal timed out",
                _SWITCH_ABORTED,
                {"reason": "switch_command_timeout"},
            ) from None
        except Exception:
            self._rollback_after_failed_switch()
            raise AppError(
                "E_SWITCH_ABORTED",
                "Physical role reversal command failed",
                _SWITCH_ABORTED,
                {"reason": "switch_command_failed"},
            ) from None
        if return_code != 0:
            self._rollback_after_failed_switch()
            raise AppError(
                "E_SWITCH_ABORTED",
                "Physical role reversal command returned a failure code",
                _SWITCH_ABORTED,
                {"reason": "switch_command_failed"},
            )

    def _rollback_after_failed_switch(self) -> None:
        if not self.config.switch.rollback_command:
            return
        environment = os.environ.copy()
        environment.update(
            {
                "ORDERSYNC_OLD_PRIMARY_CONNECTION_STRING": (
                    self.config.primary.connection_string
                ),
                "ORDERSYNC_OLD_PRIMARY_DATABASE": self.config.primary.database,
                "ORDERSYNC_OLD_REPLICA_CONNECTION_STRING": (
                    self.config.replica.connection_string
                ),
                "ORDERSYNC_OLD_REPLICA_DATABASE": self.config.replica.database,
            }
        )
        try:
            self._command_runner(
                self.config.switch.rollback_command,
                environment,
                self.config.switch.command_timeout_seconds,
            )
        except Exception:
            pass

    def _validate_after_switch(self) -> None:
        reversed_config = replace(
            self.config,
            primary=self.config.replica,
            replica=self.config.primary,
        )
        try:
            self._topology_validator_factory(reversed_config).validate()
        except Exception:
            self._rollback_after_failed_switch()
            raise AppError(
                "E_SWITCH_ABORTED",
                "Role reversal did not pass the reversed topology validation",
                _SWITCH_ABORTED,
                {"reason": "post_switch_topology_invalid"},
            ) from None

    @staticmethod
    def _run_external_command(
        command: tuple[str, ...],
        environment: dict[str, str],
        timeout_seconds: int,
    ) -> int:
        completed = subprocess.run(
            list(command),
            env=environment,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=timeout_seconds,
            check=False,
        )
        return int(completed.returncode)

    def close(self) -> None:
        with self._role_lock:
            self._primary_gateway.close()
            self._replica_gateway.close()

