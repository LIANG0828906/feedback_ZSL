from __future__ import annotations

import copy
import time
from datetime import datetime, timezone
from typing import Any

from ordersync.config import ServiceConfig
from ordersync.errors import AppError
from ordersync.replication import MonitorSnapshot, WatermarkReading
from ordersync.topology import TopologySnapshot


def base_config_dict() -> dict[str, Any]:
    return {
        "primary": {
            "connection_string": "primary-cs",
            "database": "OrderMain",
            "pool_size": 2,
            "pool_acquire_timeout_ms": 10,
        },
        "replica": {
            "connection_string": "replica-cs",
            "database": "OrderReplica",
            "pool_size": 2,
            "pool_acquire_timeout_ms": 10,
        },
        "replication": {
            "mode": "log_shipping",
            "poll_interval_ms": 100,
            "watermark_wait_timeout_ms": 100,
            "lag_threshold_ms": 1000,
            "lag_window_seconds": 30,
            "sync_stall_ms": 300,
        },
        "switch": {
            "timeout_ms": 150,
            "allow_logical_switch": True,
        },
        "faults": {
            "allow_simulated_replication_stall": True,
        },
        "server": {
            "compare_on_replica_read": True,
            "default_page_size": 20,
            "max_page_size": 100,
        },
    }


def base_config() -> ServiceConfig:
    return ServiceConfig.parse(base_config_dict())


def topology_snapshot() -> TopologySnapshot:
    return TopologySnapshot(
        replication_mode="log_shipping",
        schema_digest="a" * 64,
        primary_watermark=0,
        replica_watermark=0,
    )


class FakeProbe:
    def __init__(self) -> None:
        self.primary_watermark = 0
        self.replica_watermark = 0
        self.primary_ok = True
        self.replica_ok = True
        self.hint: bool | None = None
        self.swaps = 0
        self.closed = False

    def read_watermark(self, role: str) -> WatermarkReading:
        if role == "primary":
            if not self.primary_ok:
                raise RuntimeError("primary unavailable")
            value = self.primary_watermark
        else:
            if not self.replica_ok:
                raise RuntimeError("replica unavailable")
            value = self.replica_watermark
        return WatermarkReading(
            watermark=value,
            updated_at_utc=datetime.now(timezone.utc),
            observed_monotonic=time.monotonic(),
        )

    def status_hint(self) -> bool | None:
        return self.hint

    def swap_roles(self) -> None:
        self.swaps += 1
        self.primary_watermark, self.replica_watermark = (
            self.replica_watermark,
            self.primary_watermark,
        )

    def close(self) -> None:
        self.closed = True


def make_order(order_no: str, watermark: int, tenant: str = "T01") -> dict[str, Any]:
    line = {
        "line_no": 1,
        "sku": "SKU-0001",
        "quantity": 1,
        "unit_price": "10.00",
        "line_amount": "10.00",
        "write_watermark": watermark,
    }
    return {
        "order_no": order_no,
        "tenant_id": tenant,
        "status": "CONFIRMED",
        "amount": "10.00",
        "item_count": 1,
        "created_at_utc": "2025-01-01T00:00:00.000Z",
        "updated_at_utc": "2025-01-01T00:00:00.000Z",
        "write_watermark": watermark,
        "lines": [line],
    }


class FakeGateway:
    def __init__(self, watermark: int = 0) -> None:
        self.watermark = watermark
        self.orders: dict[str, dict[str, Any]] = {}
        self.write_error: AppError | None = None
        self.read_error: Exception | None = None
        self.write_calls: list[dict[str, Any]] = []
        self.closed = False

    def write_order(self, order: dict[str, Any]) -> int:
        if self.write_error is not None:
            raise self.write_error
        if order["order_no"] in self.orders:
            raise AppError("E_WRITE_CONFLICT", "duplicate", 409)
        self.watermark += 1
        record = {
            "order_no": order["order_no"],
            "tenant_id": order["tenant_id"],
            "status": order["status"],
            "amount": order["amount"],
            "item_count": order["item_count"],
            "created_at_utc": order["created_at_utc"],
            "updated_at_utc": order["created_at_utc"],
            "write_watermark": self.watermark,
            "lines": [
                {
                    "line_no": line["line_no"],
                    "sku": line["sku"],
                    "quantity": line["quantity"],
                    "unit_price": line["unit_price"],
                    "line_amount": line["line_amount"],
                    "write_watermark": self.watermark,
                }
                for line in order["lines"]
            ],
        }
        self.orders[order["order_no"]] = record
        self.write_calls.append(copy.deepcopy(order))
        return self.watermark

    def fetch_order(self, order_no: str) -> dict[str, Any] | None:
        if self.read_error is not None:
            raise self.read_error
        record = self.orders.get(order_no)
        return copy.deepcopy(record) if record is not None else None

    def list_orders(
        self,
        tenant_id: str,
        status: str,
        *,
        page: int,
        page_size: int,
    ) -> dict[str, Any]:
        if self.read_error is not None:
            raise self.read_error
        matching = [
            copy.deepcopy(item)
            for item in sorted(self.orders.values(), key=lambda item: item["order_no"])
            if item["tenant_id"] == tenant_id and item["status"] == status
        ]
        offset = (page - 1) * page_size
        return {
            "items": matching[offset : offset + page_size],
            "page": page,
            "page_size": page_size,
            "has_more": len(matching) > offset + page_size,
        }

    def current_watermark(self) -> int:
        return self.watermark

    def close(self) -> None:
        self.closed = True


def valid_write(order_no: str = "ORD-X") -> dict[str, Any]:
    return {
        "order_no": order_no,
        "tenant_id": "T01",
        "status": "CONFIRMED",
        "created_at_utc": "2025-03-01T00:00:00.000Z",
        "lines": [
            {
                "line_no": 1,
                "sku": "SKU-X",
                "quantity": 2,
                "unit_price": "9.99",
            }
        ],
    }


def monitor_snapshot(
    *,
    primary: int = 0,
    replica: int = 0,
    reachable: bool = True,
    connected: bool = True,
    reason: str | None = None,
) -> MonitorSnapshot:
    return MonitorSnapshot(
        primary_watermark=primary,
        replica_watermark=replica,
        primary_reachable=True,
        replica_reachable=reachable,
        replication_connected=connected,
        watermark_gap=max(primary - replica, 0),
        avg_lag_ms_30s=0,
        avg_visibility_latency_ms_30s=0,
        degraded_reason=reason,
        forced_disconnected=False,
        status_hint=None,
    )

