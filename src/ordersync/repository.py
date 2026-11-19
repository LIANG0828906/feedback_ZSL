from __future__ import annotations

from datetime import datetime, timezone
from decimal import Decimal, InvalidOperation, ROUND_HALF_UP
from typing import Any, Protocol

from .db import DatabaseUnavailableError, PoolExhaustedError, SqlClient, is_write_conflict
from .errors import (
    connection_pool_exhausted,
    invalid_request,
    write_conflict,
    write_unavailable,
)


WATERMARK_NAME = "orders"
WATERMARK_QUERY = (
    "SELECT AppliedWatermark AS applied_watermark, "
    "UpdatedAtUtc AS updated_at_utc "
    "FROM dbo.ConsistencyWatermark WHERE Name = ?"
)
WATERMARK_ADVANCE = (
    "UPDATE dbo.ConsistencyWatermark "
    "SET AppliedWatermark = AppliedWatermark + 1, "
    "UpdatedAtUtc = SYSUTCDATETIME() "
    "OUTPUT inserted.AppliedWatermark "
    "WHERE Name = ?"
)

ORDER_COLUMNS = """
    OrderNo AS order_no,
    TenantId AS tenant_id,
    Status AS status,
    Amount AS amount,
    ItemCount AS item_count,
    CreatedAtUtc AS created_at_utc,
    UpdatedAtUtc AS updated_at_utc,
    LastWriteWatermark AS write_watermark
"""

LINE_COLUMNS = """
    OrderNo AS order_no,
    LineNo AS line_no,
    TenantId AS tenant_id,
    Sku AS sku,
    Quantity AS quantity,
    UnitPrice AS unit_price,
    LineAmount AS line_amount,
    WriteWatermark AS write_watermark
"""


class OrderGateway(Protocol):
    def write_order(self, order: dict[str, Any]) -> int: ...

    def fetch_order(self, order_no: str) -> dict[str, Any] | None: ...

    def list_orders(
        self,
        tenant_id: str,
        status: str,
        *,
        page: int,
        page_size: int,
    ) -> dict[str, Any]: ...

    def current_watermark(self) -> int: ...

    def close(self) -> None: ...


def _utc_iso(value: Any) -> str:
    if not isinstance(value, datetime):
        raise ValueError("expected datetime")
    if value.tzinfo is None:
        value = value.replace(tzinfo=timezone.utc)
    value = value.astimezone(timezone.utc)
    return value.strftime("%Y-%m-%dT%H:%M:%S.") + f"{value.microsecond // 1000:03d}Z"


def _decimal_text(value: Any, places: int = 2) -> str:
    decimal_value = Decimal(str(value)).quantize(
        Decimal(1).scaleb(-places), rounding=ROUND_HALF_UP
    )
    return format(decimal_value, f".{places}f")


def _normalize_order_row(row: dict[str, Any]) -> dict[str, Any]:
    return {
        "order_no": str(row["order_no"]),
        "tenant_id": str(row["tenant_id"]),
        "status": str(row["status"]),
        "amount": _decimal_text(row["amount"]),
        "item_count": int(row["item_count"]),
        "created_at_utc": _utc_iso(row["created_at_utc"]),
        "updated_at_utc": _utc_iso(row["updated_at_utc"]),
        "write_watermark": int(row["write_watermark"]),
    }


def _normalize_line_row(row: dict[str, Any]) -> dict[str, Any]:
    return {
        "line_no": int(row["line_no"]),
        "sku": str(row["sku"]),
        "quantity": int(row["quantity"]),
        "unit_price": _decimal_text(row["unit_price"]),
        "line_amount": _decimal_text(row["line_amount"]),
        "write_watermark": int(row["write_watermark"]),
    }


def _parse_utc_text(value: Any) -> str:
    if not isinstance(value, str):
        raise invalid_request("created_at_utc must be an ISO-8601 string")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        raise invalid_request("created_at_utc is not valid ISO-8601") from None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return _utc_iso(parsed.astimezone(timezone.utc))


def _db_utc_datetime(value: str) -> datetime:
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc).replace(tzinfo=None)


def _require_text(
    raw: dict[str, Any],
    key: str,
    *,
    maximum: int,
    allow_empty: bool = False,
) -> str:
    value = raw.get(key)
    if not isinstance(value, str):
        raise invalid_request(f"{key} must be a string")
    value = value.strip()
    if (not allow_empty and not value) or len(value) > maximum:
        raise invalid_request(f"{key} must contain 1 to {maximum} characters")
    return value


def _as_positive_int(value: Any, key: str, maximum: int) -> int:
    if isinstance(value, bool):
        raise invalid_request(f"{key} must be an integer")
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        raise invalid_request(f"{key} must be an integer") from None
    if parsed < 1 or parsed > maximum:
        raise invalid_request(f"{key} must be between 1 and {maximum}")
    return parsed


def _as_money(value: Any, key: str) -> Decimal:
    try:
        parsed = Decimal(str(value))
    except (InvalidOperation, ValueError):
        raise invalid_request(f"{key} must be a decimal number") from None
    if not parsed.is_finite() or parsed < 0:
        raise invalid_request(f"{key} must be a finite non-negative number")
    if parsed >= Decimal("1000000000"):
        raise invalid_request(f"{key} is out of range")
    return parsed.quantize(Decimal("0.01"), rounding=ROUND_HALF_UP)


def prepare_order(raw: Any) -> dict[str, Any]:
    if not isinstance(raw, dict):
        raise invalid_request("request body must be a JSON object")
    order_no = _require_text(raw, "order_no", maximum=32)
    tenant_id = _require_text(raw, "tenant_id", maximum=32)
    status = _require_text(raw, "status", maximum=16).upper()
    if status not in {"PENDING", "CONFIRMED", "CANCELLED"}:
        raise invalid_request("status must be PENDING, CONFIRMED, or CANCELLED")

    lines_raw = raw.get("lines")
    if not isinstance(lines_raw, list) or not lines_raw:
        raise invalid_request("lines must be a non-empty array")
    if len(lines_raw) > 100:
        raise invalid_request("an order may contain at most 100 lines")

    lines: list[dict[str, Any]] = []
    seen_line_numbers: set[int] = set()
    for index, line_raw in enumerate(lines_raw):
        if not isinstance(line_raw, dict):
            raise invalid_request(f"lines[{index}] must be an object")
        line_no = _as_positive_int(line_raw.get("line_no"), "line_no", 1_000_000)
        if line_no in seen_line_numbers:
            raise invalid_request("line_no values must be unique")
        seen_line_numbers.add(line_no)
        sku = _require_text(line_raw, "sku", maximum=64)
        quantity = _as_positive_int(line_raw.get("quantity"), "quantity", 100000)
        unit_price = _as_money(line_raw.get("unit_price"), "unit_price")
        line_amount = (unit_price * quantity).quantize(
            Decimal("0.01"), rounding=ROUND_HALF_UP
        )
        lines.append(
            {
                "line_no": line_no,
                "sku": sku,
                "quantity": quantity,
                "unit_price": format(unit_price, ".2f"),
                "line_amount": format(line_amount, ".2f"),
            }
        )

    total = sum(Decimal(line["line_amount"]) for line in lines).quantize(
        Decimal("0.01"), rounding=ROUND_HALF_UP
    )
    created_at = raw.get("created_at_utc")
    if created_at is None:
        created_at_text = _utc_iso(datetime.now(timezone.utc))
    else:
        created_at_text = _parse_utc_text(created_at)
    return {
        "order_no": order_no,
        "tenant_id": tenant_id,
        "status": status,
        "amount": format(total, ".2f"),
        "item_count": len(lines),
        "created_at_utc": created_at_text,
        "lines": lines,
    }


class SqlOrderGateway:
    def __init__(self, client: SqlClient) -> None:
        self.client = client

    def write_order(self, order: dict[str, Any]) -> int:
        try:
            created_at_utc = _db_utc_datetime(order["created_at_utc"])
            with self.client.transaction() as cursor:
                cursor.execute(WATERMARK_ADVANCE, (WATERMARK_NAME,))
                row = cursor.fetchone()
                if row is None:
                    raise RuntimeError("watermark row is missing")
                watermark = int(row[0])
                cursor.execute(
                    """
                    INSERT INTO dbo.Orders
                        (OrderNo, TenantId, Status, Amount, ItemCount,
                         CreatedAtUtc, UpdatedAtUtc, LastWriteWatermark)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        order["order_no"],
                        order["tenant_id"],
                        order["status"],
                        Decimal(order["amount"]),
                        order["item_count"],
                        created_at_utc,
                        created_at_utc,
                        watermark,
                    ),
                )
                for line in order["lines"]:
                    cursor.execute(
                        """
                        INSERT INTO dbo.OrderLines
                            (OrderNo, LineNo, TenantId, Sku, Quantity,
                             UnitPrice, LineAmount, WriteWatermark)
                        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                        """,
                        (
                            order["order_no"],
                            line["line_no"],
                            order["tenant_id"],
                            line["sku"],
                            line["quantity"],
                            Decimal(line["unit_price"]),
                            Decimal(line["line_amount"]),
                            watermark,
                        ),
                    )
            return watermark
        except PoolExhaustedError:
            raise connection_pool_exhausted() from None
        except (DatabaseUnavailableError, OSError) as exc:
            if is_write_conflict(exc):
                raise write_conflict() from None
            raise write_unavailable() from None
        except Exception as exc:
            if is_write_conflict(exc):
                raise write_conflict() from None
            state = str(getattr(exc, "args", ("",))[0])
            if state[:2] in {"08", "HY"} or "operational" in type(exc).__name__.lower():
                raise write_unavailable() from None
            raise

    def fetch_order(self, order_no: str) -> dict[str, Any] | None:
        order_row = self.client.query_one(
            f"SELECT {ORDER_COLUMNS} FROM dbo.Orders WHERE OrderNo = ?",
            (order_no,),
        )
        if order_row is None:
            return None
        order = _normalize_order_row(order_row)
        line_rows = self.client.query(
            f"SELECT {LINE_COLUMNS} FROM dbo.OrderLines "
            "WHERE OrderNo = ? ORDER BY LineNo",
            (order_no,),
        )
        order["lines"] = [_normalize_line_row(row) for row in line_rows]
        return order

    def list_orders(
        self,
        tenant_id: str,
        status: str,
        *,
        page: int,
        page_size: int,
    ) -> dict[str, Any]:
        offset = (page - 1) * page_size
        rows = self.client.query(
            f"""
            SELECT {ORDER_COLUMNS}
            FROM dbo.Orders
            WHERE TenantId = ? AND Status = ?
            ORDER BY OrderNo
            OFFSET ? ROWS FETCH NEXT ? ROWS ONLY
            """,
            (tenant_id, status, offset, page_size + 1),
        )
        has_more = len(rows) > page_size
        items = [_normalize_order_row(row) for row in rows[:page_size]]
        return {
            "items": items,
            "page": page,
            "page_size": page_size,
            "has_more": has_more,
        }

    def current_watermark(self) -> int:
        row = self.client.query_one(WATERMARK_QUERY, (WATERMARK_NAME,))
        if row is None:
            raise RuntimeError("watermark row is missing")
        return int(row["applied_watermark"])

    def close(self) -> None:
        self.client.close()


def field_differences(
    primary_record: dict[str, Any] | None,
    replica_record: dict[str, Any] | None,
) -> list[dict[str, Any]]:
    differences: list[dict[str, Any]] = []
    if primary_record is None and replica_record is None:
        return differences
    if primary_record is None:
        order_no = (
            replica_record.get("order_no", "") if replica_record else ""
        )
        return [
            {
                "field": "orders",
                "primary": "<absent>",
                "replica": f"<present:{order_no}>",
            }
        ]
    if replica_record is None:
        return [
            {
                "field": "orders",
                "primary": f"<present:{primary_record.get('order_no', '')}>",
                "replica": "<absent>",
            }
        ]

    order_fields = [
        "tenant_id",
        "status",
        "amount",
        "item_count",
        "created_at_utc",
        "updated_at_utc",
        "write_watermark",
    ]
    for field in order_fields:
        primary_value = primary_record.get(field)
        replica_value = replica_record.get(field)
        if primary_value != replica_value:
            differences.append(
                {
                    "field": f"orders.{field}",
                    "primary": _diff_text(primary_value),
                    "replica": _diff_text(replica_value),
                }
            )

    primary_lines = {
        int(line["line_no"]): line
        for line in primary_record.get("lines", [])
        if "line_no" in line
    }
    replica_lines = {
        int(line["line_no"]): line
        for line in replica_record.get("lines", [])
        if "line_no" in line
    }
    line_fields = [
        "sku",
        "quantity",
        "unit_price",
        "line_amount",
        "write_watermark",
    ]
    for line_no in sorted(set(primary_lines) | set(replica_lines)):
        primary_line = primary_lines.get(line_no)
        replica_line = replica_lines.get(line_no)
        if primary_line is None or replica_line is None:
            differences.append(
                {
                    "field": f"lines.{line_no}",
                    "primary": (
                        "<present>" if primary_line is not None else "<absent>"
                    ),
                    "replica": (
                        "<present>" if replica_line is not None else "<absent>"
                    ),
                }
            )
            continue
        for field in line_fields:
            if primary_line.get(field) != replica_line.get(field):
                differences.append(
                    {
                        "field": f"lines.{line_no}.{field}",
                        "primary": _diff_text(primary_line.get(field)),
                        "replica": _diff_text(replica_line.get(field)),
                    }
                )
    differences.sort(key=lambda item: item["field"])
    return differences


def _diff_text(value: Any) -> str:
    if value is None:
        return "<null>"
    if isinstance(value, str):
        return value
    return str(value)


def classify_difference(
    primary_record: dict[str, Any] | None,
    replica_record: dict[str, Any] | None,
) -> str:
    if primary_record is None:
        return "missing_on_primary"
    if replica_record is None:
        return "missing_on_replica"
    primary_watermark = int(primary_record.get("write_watermark", 0))
    replica_watermark = int(replica_record.get("write_watermark", 0))
    if replica_watermark < primary_watermark:
        return "replica_behind"
    if replica_watermark > primary_watermark:
        return "replica_ahead_of_primary"
    return "field_mismatch"

