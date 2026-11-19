from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class AppError(Exception):
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)
    http_status: int = 409

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"

    def as_dict(self) -> dict[str, Any]:
        body: dict[str, Any] = {
            "ok": False,
            "errorCode": self.code,
            "message": self.message,
        }
        if self.details:
            body["details"] = self.details
        return body


def topology_error(message: str, **details: Any) -> AppError:
    return AppError("E_TOPOLOGY", message, details, 503)


def dual_write_error(message: str, **details: Any) -> AppError:
    return AppError("E_DUAL_WRITE", message, details, 409)


def not_migrated(order_id: int, tenant_id: int) -> AppError:
    return AppError(
        "E_NOT_MIGRATED",
        f"order {order_id} has not reached its target shard",
        {"orderId": order_id, "tenantId": tenant_id},
        404,
    )


def check_unavailable(message: str, **details: Any) -> AppError:
    return AppError("E_CHECK_UNAVAILABLE", message, details, 503)


def rollback_error(message: str, **details: Any) -> AppError:
    return AppError("E_ROLLBACK", message, details, 409)


def conn_pool_error() -> AppError:
    return AppError(
        "E_CONN_POOL",
        "timed out waiting for a database connection slot",
        {"timeoutCode": "ACQUIRE_TIMEOUT"},
        503,
    )


def validation_error(message: str, **details: Any) -> AppError:
    return AppError("E_VALIDATION", message, details, 400)
