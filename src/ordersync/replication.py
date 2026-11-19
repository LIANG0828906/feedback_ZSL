from __future__ import annotations

import threading
import time
from collections import deque
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Protocol

from .config import EndpointConfig, ReplicationConfig
from .db import Connector, DedicatedConnection, pyodbc_connector
from .repository import WATERMARK_NAME, WATERMARK_QUERY


@dataclass(frozen=True)
class WatermarkReading:
    watermark: int
    updated_at_utc: datetime | None
    observed_monotonic: float


@dataclass(frozen=True)
class MonitorSnapshot:
    primary_watermark: int | None
    replica_watermark: int | None
    primary_reachable: bool
    replica_reachable: bool
    replication_connected: bool
    watermark_gap: int | None
    avg_lag_ms_30s: int | None
    avg_visibility_latency_ms_30s: int | None
    degraded_reason: str | None
    forced_disconnected: bool
    status_hint: bool | None

    def as_dict(self) -> dict[str, Any]:
        return {
            "primary_watermark": _optional_text(self.primary_watermark),
            "replica_applied_watermark": _optional_text(self.replica_watermark),
            "primary_reachable": self.primary_reachable,
            "replica_reachable": self.replica_reachable,
            "replication_connected": self.replication_connected,
            "watermark_gap": _optional_text(self.watermark_gap),
            "avg_lag_ms_30s": self.avg_lag_ms_30s,
            "avg_visibility_latency_ms_30s": (
                self.avg_visibility_latency_ms_30s
            ),
            "degraded_reason": self.degraded_reason,
            "forced_disconnected": self.forced_disconnected,
            "status_hint": self.status_hint,
        }


def _optional_text(value: int | None) -> str | None:
    return str(value) if value is not None else None


class ReplicationProbe(Protocol):
    def read_watermark(self, role: str) -> WatermarkReading: ...

    def status_hint(self) -> bool | None: ...

    def swap_roles(self) -> None: ...

    def close(self) -> None: ...


class SqlReplicationProbe:
    """Uses dedicated monitoring connections, isolated from business pools."""

    def __init__(
        self,
        primary: EndpointConfig,
        replica: EndpointConfig,
        replication: ReplicationConfig,
        connector: Connector = pyodbc_connector,
    ) -> None:
        self._primary_endpoint = primary
        self._replica_endpoint = replica
        self._replication = replication
        self._primary = DedicatedConnection(primary, connector)
        self._replica = DedicatedConnection(replica, connector)
        self._lock = threading.Lock()

    def read_watermark(self, role: str) -> WatermarkReading:
        with self._lock:
            connection = self._primary if role == "primary" else self._replica
            rows = connection.query(
                WATERMARK_QUERY, (WATERMARK_NAME,)
            )
        if not rows:
            raise RuntimeError("watermark row is missing")
        row = rows[0]
        value = _row_lookup(row, "applied_watermark")
        updated = _row_lookup(row, "updated_at_utc")
        if isinstance(updated, datetime):
            if updated.tzinfo is None:
                updated = updated.replace(tzinfo=timezone.utc)
            updated = updated.astimezone(timezone.utc)
        else:
            updated = None
        return WatermarkReading(
            watermark=int(value),
            updated_at_utc=updated,
            observed_monotonic=time.monotonic(),
        )

    def status_hint(self) -> bool | None:
        if self._replication.mode == "log_shipping":
            return self._log_shipping_status()
        return self._transactional_status()

    def _log_shipping_status(self) -> bool | None:
        sql = """
            SELECT TOP (1)
                last_restored_date,
                last_restore_latency,
                restore_threshold
            FROM msdb.dbo.log_shipping_monitor_secondary
            WHERE secondary_database = ?
            ORDER BY last_restored_date DESC
        """
        try:
            with self._lock:
                row = self._replica.query_one(
                    sql, (self._replica_endpoint.database,)
                )
        except Exception:
            return None
        if not row:
            return None
        restored = _row_lookup(row, "last_restored_date")
        latency = _row_lookup(row, "last_restore_latency")
        threshold = _row_lookup(row, "restore_threshold")
        if restored is None:
            return None
        if isinstance(restored, datetime):
            if restored.tzinfo is None:
                restored = restored.replace(tzinfo=timezone.utc)
            age_seconds = (
                datetime.now(timezone.utc) - restored.astimezone(timezone.utc)
            ).total_seconds()
        else:
            return None
        threshold_seconds = (
            float(threshold) * 60.0
            if threshold is not None
            else self._replication.sync_stall_ms / 1000.0
        )
        # An alert threshold is a lag warning, not proof of disconnection.
        # The monitor's no-progress rule makes the final connected decision.
        if latency is not None and threshold is not None and float(latency) > float(
            threshold
        ):
            return None
        if age_seconds > max(threshold_seconds * 3.0, 60.0):
            return None
        return None

    def _transactional_status(self) -> bool | None:
        sql = """
            SELECT TOP (1) [Time] AS last_agent_update
            FROM dbo.MSreplication_subscriptions
            WHERE publisher_db = ? AND publication = ?
        """
        try:
            with self._lock:
                row = self._replica.query_one(
                    sql,
                    (
                        self._primary_endpoint.database,
                        self._replication.publication,
                    ),
                )
        except Exception:
            return None
        if not row:
            return None
        updated = _row_lookup(row, "last_agent_update")
        if not isinstance(updated, datetime):
            return None
        if updated.tzinfo is None:
            updated = updated.replace(tzinfo=timezone.utc)
        age_ms = (
            datetime.now(timezone.utc) - updated.astimezone(timezone.utc)
        ).total_seconds() * 1000.0
        # Recent agent activity is a positive signal only. Old activity is not
        # treated as a disconnect because Agent may legitimately be idle.
        return True if age_ms <= self._replication.sync_stall_ms else None

    def swap_roles(self) -> None:
        with self._lock:
            self._primary_endpoint, self._replica_endpoint = (
                self._replica_endpoint,
                self._primary_endpoint,
            )
            self._primary.close_locked()
            self._replica.close_locked()
            self._primary, self._replica = self._replica, self._primary

    def close(self) -> None:
        with self._lock:
            self._primary.close_locked()
            self._replica.close_locked()


def _row_lookup(row: dict[str, Any], key: str) -> Any:
    if key in row:
        return row[key]
    for candidate, value in row.items():
        if str(candidate).casefold() == key.casefold():
            return value
    return None


class ReplicaMonitor:
    def __init__(
        self,
        probe: ReplicationProbe,
        replication: ReplicationConfig,
    ) -> None:
        self._probe = probe
        self._replication = replication
        self._condition = threading.Condition(threading.RLock())
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._forced_disconnected = False

        self._primary_watermark: int | None = None
        self._replica_watermark: int | None = None
        self._primary_reachable = False
        self._replica_reachable = False
        self._replication_connected = False
        self._degraded_reason: str | None = "not_collected"
        self._status_hint: bool | None = None

        self._last_replica_watermark: int | None = None
        self._last_replica_progress_monotonic = time.monotonic()
        self._last_replica_updated_at_utc: datetime | None = None

        self._pending: list[tuple[int, float]] = []
        self._lag_window: deque[tuple[float, float]] = deque()
        self._visibility_window: deque[tuple[float, float]] = deque()
        self._all_gap_samples: list[int] = []
        self._all_visibility_latencies: list[int] = []

    def start(self) -> None:
        if self._thread is not None and self._thread.is_alive():
            return
        self._stop.clear()
        self._thread = threading.Thread(
            target=self._run, name="replica-monitor", daemon=True
        )
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        with self._condition:
            self._condition.notify_all()
        if self._thread is not None:
            self._thread.join(timeout=5)
        self._probe.close()

    def _run(self) -> None:
        interval = self._replication.poll_interval_ms / 1000.0
        while not self._stop.is_set():
            started = time.monotonic()
            try:
                self.collect_once()
            except Exception:
                with self._condition:
                    self._replica_reachable = False
                    self._replication_connected = False
                    self._degraded_reason = "replica_unavailable"
                    self._condition.notify_all()
            elapsed = time.monotonic() - started
            self._stop.wait(max(interval - elapsed, 0.01))

    def collect_once(self) -> MonitorSnapshot:
        now = time.monotonic()
        primary_reading: WatermarkReading | None = None
        replica_reading: WatermarkReading | None = None
        hint: bool | None = None

        try:
            primary_reading = self._probe.read_watermark("primary")
        except Exception:
            primary_reading = None
        try:
            replica_reading = self._probe.read_watermark("replica")
        except Exception:
            replica_reading = None
        try:
            hint = self._probe.status_hint()
        except Exception:
            hint = None

        with self._condition:
            primary_reachable = primary_reading is not None
            replica_reachable = replica_reading is not None
            self._primary_reachable = primary_reachable
            self._replica_reachable = replica_reachable
            if primary_reading is not None:
                self._primary_watermark = primary_reading.watermark
            if replica_reading is not None:
                self._replica_watermark = replica_reading.watermark
                self._last_replica_updated_at_utc = (
                    replica_reading.updated_at_utc
                )

            gap: int | None
            if (
                self._primary_watermark is not None
                and self._replica_watermark is not None
            ):
                gap = max(
                    self._primary_watermark - self._replica_watermark, 0
                )
            else:
                gap = None
            if gap is not None:
                self._all_gap_samples.append(gap)

            if replica_reachable and self._replica_watermark is not None:
                if self._last_replica_watermark != self._replica_watermark:
                    self._last_replica_watermark = self._replica_watermark
                    self._last_replica_progress_monotonic = now
                if gap == 0:
                    self._last_replica_progress_monotonic = now

            stalled = False
            if (
                replica_reachable
                and gap is not None
                and gap > 0
                and (now - self._last_replica_progress_monotonic) * 1000.0
                >= self._replication.sync_stall_ms
            ):
                stalled = True

            forced = self._forced_disconnected
            connected = (
                replica_reachable
                and not forced
                and not stalled
                and hint is not False
            )
            previous_connected = self._replication_connected
            self._replication_connected = connected
            self._status_hint = hint

            if connected and gap == 0:
                self._lag_window.clear()
                self._lag_window.append((now, 0.0))
            elif connected and previous_connected and not stalled:
                pass

            if replica_reachable and self._replica_watermark is not None:
                self._resolve_pending(now)

            current_lag_ms: float | None = None
            if (
                replica_reachable
                and gap is not None
                and gap > 0
                and self._pending
            ):
                oldest_ack = min(ack for _, ack in self._pending)
                current_lag_ms = max((now - oldest_ack) * 1000.0, 0.0)
            elif (
                replica_reachable
                and primary_reachable
                and gap is not None
                and gap > 0
                and self._last_replica_updated_at_utc is not None
            ):
                current_lag_ms = max(
                    (
                        datetime.now(timezone.utc)
                        - self._last_replica_updated_at_utc
                    ).total_seconds()
                    * 1000.0,
                    0.0,
                )
            elif gap == 0:
                current_lag_ms = 0.0

            if current_lag_ms is not None:
                self._lag_window.append((now, current_lag_ms))
            self._trim_windows(now)

            avg_lag = self._window_average(self._lag_window)
            avg_visibility = self._window_average(self._visibility_window)

            if not replica_reachable:
                reason = "replica_unavailable"
            elif forced or stalled or hint is False:
                reason = "replication_disconnected"
            elif (
                gap is not None
                and gap > 0
                and avg_lag is not None
                and avg_lag > self._replication.lag_threshold_ms
            ):
                reason = "replication_lag"
            else:
                reason = None
            self._degraded_reason = reason
            self._condition.notify_all()
            return self._snapshot_locked()

    def _resolve_pending(self, now: float) -> None:
        if self._replica_watermark is None:
            return
        remaining: list[tuple[int, float]] = []
        for watermark, ack in self._pending:
            if watermark <= self._replica_watermark:
                latency_ms = max(int((now - ack) * 1000), 0)
                self._visibility_window.append((now, float(latency_ms)))
                self._all_visibility_latencies.append(latency_ms)
            else:
                remaining.append((watermark, ack))
        self._pending = remaining

    def _trim_windows(self, now: float) -> None:
        cutoff = now - self._replication.lag_window_seconds
        while self._lag_window and self._lag_window[0][0] < cutoff:
            self._lag_window.popleft()
        while (
            self._visibility_window
            and self._visibility_window[0][0] < cutoff
        ):
            self._visibility_window.popleft()

    @staticmethod
    def _window_average(window: deque[tuple[float, float]]) -> int | None:
        if not window:
            return None
        return int(round(sum(value for _, value in window) / len(window)))

    def _snapshot_locked(self) -> MonitorSnapshot:
        gap: int | None = None
        if (
            self._primary_watermark is not None
            and self._replica_watermark is not None
        ):
            gap = max(self._primary_watermark - self._replica_watermark, 0)
        return MonitorSnapshot(
            primary_watermark=self._primary_watermark,
            replica_watermark=self._replica_watermark,
            primary_reachable=self._primary_reachable,
            replica_reachable=self._replica_reachable,
            replication_connected=self._replication_connected,
            watermark_gap=gap,
            avg_lag_ms_30s=self._window_average(self._lag_window),
            avg_visibility_latency_ms_30s=self._window_average(
                self._visibility_window
            ),
            degraded_reason=self._degraded_reason,
            forced_disconnected=self._forced_disconnected,
            status_hint=self._status_hint,
        )

    def snapshot(self) -> MonitorSnapshot:
        with self._condition:
            return self._snapshot_locked()

    def record_write(self, watermark: int, ack_monotonic: float | None = None) -> None:
        if ack_monotonic is None:
            ack_monotonic = time.monotonic()
        with self._condition:
            self._pending.append((int(watermark), ack_monotonic))

    def wait_for_watermark(
        self, required_watermark: int, timeout_ms: int | None = None
    ) -> MonitorSnapshot:
        if timeout_ms is None:
            timeout_ms = self._replication.watermark_wait_timeout_ms
        deadline = time.monotonic() + timeout_ms / 1000.0
        with self._condition:
            while True:
                snapshot = self._snapshot_locked()
                if (
                    snapshot.replica_reachable
                    and snapshot.replica_watermark is not None
                    and snapshot.replica_watermark >= required_watermark
                ):
                    return snapshot
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    return snapshot
                self._condition.wait(timeout=min(remaining, 0.25))

    def set_forced_disconnected(self, value: bool) -> None:
        with self._condition:
            self._forced_disconnected = value
            self._condition.notify_all()

    def swap_roles(self) -> None:
        with self._condition:
            self._probe.swap_roles()
            self._primary_watermark, self._replica_watermark = (
                self._replica_watermark,
                self._primary_watermark,
            )
            self._last_replica_watermark = self._replica_watermark
            self._last_replica_progress_monotonic = time.monotonic()
            self._pending.clear()
            self._lag_window.clear()
            self._visibility_window.clear()
            self._forced_disconnected = False
            self._degraded_reason = None
            self._replication_connected = True
            self._condition.notify_all()

    def observation_report(self) -> dict[str, Any]:
        with self._condition:
            gaps = list(self._all_gap_samples)
            latencies = list(self._all_visibility_latencies)
            return {
                "watermark_gap_samples": gaps,
                "visibility_latency_samples_ms": latencies,
            }

    def reset_statistics(self) -> None:
        with self._condition:
            self._all_gap_samples.clear()
            self._all_visibility_latencies.clear()
            self._lag_window.clear()
            self._visibility_window.clear()

