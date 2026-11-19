from __future__ import annotations

import logging
import threading
import time
from typing import Any

from .audit import AuditStore, utc_now
from .config import Config
from .db import DatabaseSet
from .logic import json_safe

LOGGER = logging.getLogger(__name__)


class Observability:
    def __init__(
        self,
        config: Config,
        databases: DatabaseSet,
        audit: AuditStore,
        sync_engine: Any,
        validation_manager: Any,
    ):
        self.config = config
        self.databases = databases
        self.audit = audit
        self.sync_engine = sync_engine
        self.validation_manager = validation_manager
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._cache: dict[str, Any] = {
            "captured_at": None,
            "positions": {},
            "pending_rows": [],
            "pending_by_target": {name: 0 for name in config.sites},
            "counters": {
                name: {
                    "applied": 0,
                    "stale": 0,
                    "conflicts": 0,
                    "point_merges": 0,
                    "status_overrides": 0,
                }
                for name in config.sites
            },
            "pool": {},
            "last_error": None,
        }

    def start(self) -> None:
        if self._thread and self._thread.is_alive():
            return
        self._stop.clear()
        self._refresh()
        self._thread = threading.Thread(
            target=self._run, name="dualwrite-metrics", daemon=True
        )
        self._thread.start()

    def stop(self, timeout: float = 3.0) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=timeout)

    def _run(self) -> None:
        while not self._stop.wait(1.0):
            self._refresh()

    def _refresh(self) -> None:
        counters = self.audit.counters_by_site()
        positions: dict[str, Any] = {}
        pending_rows: list[dict[str, Any]] = []
        pending_by_target = {name: 0 for name in self.config.sites}
        errors: list[str] = []
        sources: dict[str, dict[str, Any]] = {}
        for source, target in self.sync_engine.directions():
            cursor = self.audit.cursor_position(source, target)
            try:
                pool = self.databases.pool(source)
                with pool.connection(self.config.limits.api_acquire_timeout_ms) as conn:
                    with conn.transaction():
                        max_row = conn.execute(
                            "SELECT COALESCE(MAX(change_id), 0) AS max_id FROM sync_change"
                        ).fetchone()
                        count_row = conn.execute(
                            """
                            SELECT COUNT(*) AS n FROM (
                                SELECT 1 FROM sync_change
                                WHERE change_id > %s
                                GROUP BY table_name, row_id
                            ) pending
                            """,
                            (cursor,),
                        ).fetchone()
                        preview = conn.execute(
                            """
                            SELECT table_name, row_id,
                                   MIN(change_id) AS min_change_id,
                                   MAX(change_id) AS max_change_id,
                                   COUNT(*) AS change_count,
                                   MIN(created_at) AS oldest_at
                            FROM sync_change
                            WHERE change_id > %s
                            GROUP BY table_name, row_id
                            ORDER BY MIN(change_id)
                            LIMIT %s
                            """,
                            (
                                cursor,
                                self.config.validation.pending_preview_limit,
                            ),
                        ).fetchall()
                max_id = int(max_row["max_id"])
                pending_count = int(count_row["n"])
                sources[source] = {
                    "outbox_change_id": max_id,
                    "confirmed_change_id": cursor,
                }
                pending_by_target[target] += pending_count
                for row in preview:
                    pending_rows.append(
                        {
                            "source_site": source,
                            "target_site": target,
                            "table_name": row["table_name"],
                            "row_id": int(row["row_id"]),
                            "min_change_id": int(row["min_change_id"]),
                            "max_change_id": int(row["max_change_id"]),
                            "change_count": int(row["change_count"]),
                            "oldest_at": json_safe(row["oldest_at"]),
                        }
                    )
                positions[f"{source}->{target}"] = {
                    "source_site": source,
                    "target_site": target,
                    "confirmed_change_id": cursor,
                    "source_outbox_change_id": max_id,
                    "pending_rows": pending_count,
                }
            except Exception as exc:
                errors.append(f"{source}->{target}: {type(exc).__name__}: {exc}")
                positions[f"{source}->{target}"] = {
                    "source_site": source,
                    "target_site": target,
                    "confirmed_change_id": cursor,
                    "source_outbox_change_id": None,
                    "pending_rows": None,
                }

        pending_rows.sort(
            key=lambda item: (
                str(item.get("oldest_at") or ""),
                item["source_site"],
                item["table_name"],
                item["row_id"],
            )
        )
        pool_stats = {
            name: {
                "limit": value.limit,
                "open": value.open,
                "idle": value.idle,
            }
            for name, value in self.databases.stats().items()
        }
        normalized_counters = {
            name: {
                "applied": int(counters.get(name, {}).get("applied", 0)),
                "stale": int(counters.get(name, {}).get("stale", 0)),
                "conflicts": int(counters.get(name, {}).get("conflicts", 0)),
                "point_merges": int(counters.get(name, {}).get("point_merges", 0)),
                "status_overrides": int(
                    counters.get(name, {}).get("status_overrides", 0)
                ),
            }
            for name in self.config.sites
        }
        with self._lock:
            self._cache = {
                "captured_at": utc_now(),
                "positions": positions,
                "sources": sources,
                "pending_rows": pending_rows,
                "pending_by_target": pending_by_target,
                "counters": normalized_counters,
                "pool": pool_stats,
                "last_error": "; ".join(errors) if errors else None,
            }

    def status_payload(self) -> dict[str, Any]:
        with self._lock:
            cache = dict(self._cache)
        latest_validation = self.audit.latest_validation()
        latest_unconverged = (
            self.audit.validation_mismatches(
                latest_validation["batch_id"],
                self.config.validation.pending_preview_limit,
                category="unconverged",
            )
            if latest_validation is not None
            else []
        )
        return {
            "captured_at": cache["captured_at"],
            "current_positions": cache["positions"],
            "database_pools": cache["pool"],
            "in_flight_deliveries": self.sync_engine.in_flight_deliveries,
            "last_error": self.sync_engine.last_error or cache["last_error"],
            "latest_validation": latest_validation,
            "latest_validation_unconverged": latest_unconverged,
            "pending_rows": cache["pending_rows"],
            "pending_rows_by_target": cache["pending_by_target"],
            "running_validation_batch": self.validation_manager.active_batch,
        }

    def metrics_text(self) -> str:
        with self._lock:
            cache = dict(self._cache)
        counters = cache["counters"]
        pending = cache["pending_by_target"]
        positions = cache["positions"]
        pools = cache["pool"]
        lines = [
            "# HELP sync_applied_changes_total Changes applied by the target site.",
            "# TYPE sync_applied_changes_total counter",
        ]
        for site in sorted(self.config.sites):
            lines.append(
                f'sync_applied_changes_total{{site="{site}"}} '
                f'{counters[site]["applied"]}'
            )
        lines += [
            "# HELP sync_stale_changes_total Applied changes that lost an exclusive-field comparison.",
            "# TYPE sync_stale_changes_total counter",
        ]
        for site in sorted(self.config.sites):
            lines.append(
                f'sync_stale_changes_total{{site="{site}"}} '
                f'{counters[site]["stale"]}'
            )
        lines += [
            "# HELP sync_conflicts_total Conflict decisions recorded at the target site.",
            "# TYPE sync_conflicts_total counter",
        ]
        for site in sorted(self.config.sites):
            lines.append(
                f'sync_conflicts_total{{site="{site}"}} '
                f'{counters[site]["conflicts"]}'
            )
        lines += [
            "# HELP sync_points_merged_total Changes merged into points by addition.",
            "# TYPE sync_points_merged_total counter",
        ]
        for site in sorted(self.config.sites):
            lines.append(
                f'sync_points_merged_total{{site="{site}"}} '
                f'{counters[site]["point_merges"]}'
            )
        lines += [
            "# HELP sync_status_overridden_total Order statuses replaced by the version/priority rule.",
            "# TYPE sync_status_overridden_total counter",
        ]
        for site in sorted(self.config.sites):
            lines.append(
                f'sync_status_overridden_total{{site="{site}"}} '
                f'{counters[site]["status_overrides"]}'
            )
        lines += [
            "# HELP sync_pending_convergence_rows Distinct rows with unconfirmed source changes.",
            "# TYPE sync_pending_convergence_rows gauge",
        ]
        for site in sorted(self.config.sites):
            lines.append(
                f'sync_pending_convergence_rows{{site="{site}"}} {pending[site]}'
            )
        lines += [
            "# HELP sync_confirmed_change_id Last contiguous source change confirmed at the peer.",
            "# TYPE sync_confirmed_change_id gauge",
        ]
        for source in sorted(self.config.sites):
            value = next(
                (
                    item["confirmed_change_id"]
                    for key, item in positions.items()
                    if key.startswith(f"{source}->")
                ),
                0,
            )
            lines.append(
                f'sync_confirmed_change_id{{site="{source}"}} {value}'
            )
        lines += [
            "# HELP sync_source_outbox_change_id Latest local outbox change at the site.",
            "# TYPE sync_source_outbox_change_id gauge",
        ]
        for source in sorted(self.config.sites):
            value = next(
                (
                    item["source_outbox_change_id"]
                    for key, item in positions.items()
                    if key.startswith(f"{source}->")
                ),
                None,
            )
            if value is not None:
                lines.append(
                    f'sync_source_outbox_change_id{{site="{source}"}} {value}'
                )
        lines += [
            "# HELP sync_connections_open Open PostgreSQL connections held by the service.",
            "# TYPE sync_connections_open gauge",
        ]
        for site in sorted(self.config.sites):
            lines.append(
                f'sync_connections_open{{site="{site}"}} '
                f'{pools.get(site, {}).get("open", 0)}'
            )
        lines += [
            "# HELP sync_scrape_timestamp_seconds Unix time when the metrics cache was refreshed.",
            "# TYPE sync_scrape_timestamp_seconds gauge",
        ]
        lines.append(
            f"sync_scrape_timestamp_seconds {time.time():.3f}"
        )
        return "\n".join(lines) + "\n"
