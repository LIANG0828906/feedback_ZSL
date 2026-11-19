from __future__ import annotations

import hashlib
import json
from datetime import datetime, timezone
from decimal import Decimal
from typing import Any, Iterable, Sequence

from .model import Order, OrderItem, OrderSummary
from .sqlclient import DatabaseHub, Session


def _datetime(value: Any) -> datetime:
    if isinstance(value, datetime):
        return value.replace(tzinfo=value.tzinfo or timezone.utc)
    return datetime.fromisoformat(str(value).replace("Z", "+00:00"))


def row_to_order(row: dict[str, Any], items: list[OrderItem]) -> Order:
    return Order(
        order_id=int(row["OrderId"]),
        tenant_id=int(row["TenantId"]),
        status=int(row["Status"]),
        amount=Decimal(str(row["Amount"])).quantize(Decimal("0.01")),
        currency=str(row["Currency"]).strip(),
        updated_at=_datetime(row["UpdatedAt"]),
        created_at=_datetime(row["CreatedAt"]),
        revision=int(row["Revision"]),
        items=items,
    )


def row_to_item(row: dict[str, Any]) -> OrderItem:
    return OrderItem(
        line_no=int(row["LineNumber"]),
        sku=str(row["Sku"]),
        quantity=int(row["Quantity"]),
        unit_amount=Decimal(str(row["UnitAmount"])).quantize(Decimal("0.01")),
    )


class OrderRepository:
    def __init__(self, hub: DatabaseHub):
        self.hub = hub

    def fetch_order(
        self, database: str, order_id: int, include_items: bool = True
    ) -> Order | None:
        with self.hub.read(database) as session:
            return self.fetch_order_in_session(
                session, order_id, include_items=include_items
            )

    def fetch_order_in_session(
        self, session: Session, order_id: int, include_items: bool = True
    ) -> Order | None:
        row = session.query_one(
            """
            SELECT OrderId, TenantId, Status, Amount, Currency,
                   UpdatedAt, CreatedAt, Revision,
                   ItemsJson =
                   (
                       SELECT i.LineNumber, i.Sku, i.Quantity, i.UnitAmount
                       FROM dbo.OrderItems i
                       WHERE i.OrderId = o.OrderId
                       ORDER BY i.LineNumber
                       FOR JSON PATH, INCLUDE_NULL_VALUES
                   )
            FROM dbo.Orders o WHERE o.OrderId=?
            """,
            (order_id,),
        )
        if row is None:
            return None
        items: list[OrderItem] = []
        if include_items:
            raw_items = row.get("ItemsJson")
            if raw_items:
                parsed_items = json.loads(str(raw_items))
                items = [
                    OrderItem(
                        line_no=int(item["LineNumber"]),
                        sku=str(item["Sku"]),
                        quantity=int(item["Quantity"]),
                        unit_amount=Decimal(str(item["UnitAmount"])).quantize(
                            Decimal("0.01")
                        ),
                    )
                    for item in parsed_items
                ]
        return row_to_order(row, items)

    def fetch_orders_for_tenant(
        self,
        database: str,
        tenant_id: int,
        after_order_id: int,
        limit: int,
        *,
        max_order_id: int | None = None,
    ) -> list[Order]:
        upper = max_order_id if max_order_id is not None else 9_223_372_036_854_775_807
        with self.hub.read(database) as session:
            rows = session.query(
                """
                SELECT TOP (?)
                    OrderId, TenantId, Status, Amount, Currency,
                    UpdatedAt, CreatedAt, Revision
                FROM dbo.Orders
                WHERE TenantId=? AND OrderId>? AND OrderId<=?
                ORDER BY OrderId
                """,
                (limit, tenant_id, after_order_id, upper),
            )
            if not rows:
                return []
            ids = [int(row["OrderId"]) for row in rows]
            placeholders = ",".join("?" for _ in ids)
            item_rows = session.query(
                f"""
                SELECT OrderId, LineNumber, Sku, Quantity, UnitAmount
                FROM dbo.OrderItems
                WHERE OrderId IN ({placeholders})
                ORDER BY OrderId, LineNumber
                """,
                ids,
            )
        by_order: dict[int, list[OrderItem]] = {order_id: [] for order_id in ids}
        for item in item_rows:
            by_order[int(item["OrderId"])].append(row_to_item(item))
        return [row_to_order(row, by_order[int(row["OrderId"])]) for row in rows]

    def fetch_tenant_orders_for_range(
        self,
        database: str,
        tenant_id: int,
        after_order_id: int,
        through_order_id: int,
        limit: int,
    ) -> list[Order]:
        with self.hub.read(database) as session:
            rows = session.query(
                """
                SELECT TOP (?)
                    OrderId, TenantId, Status, Amount, Currency,
                    UpdatedAt, CreatedAt, Revision
                FROM dbo.Orders
                WHERE TenantId=? AND OrderId>? AND OrderId<=?
                ORDER BY OrderId
                """,
                (limit, tenant_id, after_order_id, through_order_id),
            )
            if not rows:
                return []
            ids = [int(row["OrderId"]) for row in rows]
            placeholders = ",".join("?" for _ in ids)
            item_rows = session.query(
                f"""
                SELECT OrderId, LineNumber, Sku, Quantity, UnitAmount
                FROM dbo.OrderItems
                WHERE OrderId IN ({placeholders})
                ORDER BY OrderId, LineNumber
                """,
                ids,
            )
        by_order: dict[int, list[OrderItem]] = {order_id: [] for order_id in ids}
        for item in item_rows:
            by_order[int(item["OrderId"])].append(row_to_item(item))
        return [row_to_order(row, by_order[int(row["OrderId"])]) for row in rows]

    def summaries(
        self,
        database: str,
        order_ids: Sequence[int],
    ) -> dict[int, OrderSummary]:
        if not order_ids:
            return {}
        placeholders = ",".join("?" for _ in order_ids)
        with self.hub.read(database) as session:
            rows = session.query(
                f"""
                SELECT o.OrderId, o.TenantId, o.Status, o.Amount,
                       o.Currency, o.CreatedAt, o.UpdatedAt, o.Revision,
                       ItemCount = COUNT(i.LineNumber),
                       ItemsJson =
                       (
                           SELECT x.LineNumber, x.Sku, x.Quantity, x.UnitAmount
                           FROM dbo.OrderItems x
                           WHERE x.OrderId = o.OrderId
                           ORDER BY x.LineNumber
                           FOR JSON PATH, INCLUDE_NULL_VALUES
                       )
                FROM dbo.Orders o
                LEFT JOIN dbo.OrderItems i ON i.OrderId=o.OrderId
                WHERE o.OrderId IN ({placeholders})
                GROUP BY o.OrderId, o.TenantId, o.Status, o.Amount,
                         o.Currency, o.CreatedAt, o.UpdatedAt, o.Revision
                """,
                list(order_ids),
            )
        result: dict[int, OrderSummary] = {}
        for row in rows:
            raw_items = row.get("ItemsJson")
            items = json.loads(str(raw_items)) if raw_items else []
            fingerprint = hashlib.sha256(
                json.dumps(
                    items, sort_keys=True, separators=(",", ":")
                ).encode("utf-8")
            ).hexdigest()
            result[int(row["OrderId"])] = OrderSummary(
                order_id=int(row["OrderId"]),
                tenant_id=int(row["TenantId"]),
                status=int(row["Status"]),
                amount=Decimal(str(row["Amount"])).quantize(Decimal("0.01")),
                currency=str(row["Currency"]).strip(),
                created_at=_datetime(row["CreatedAt"]),
                updated_at=_datetime(row["UpdatedAt"]),
                revision=int(row["Revision"]),
                item_count=int(row["ItemCount"]),
                item_fingerprint=fingerprint,
            )
        return result

    def order_ids_between(
        self,
        database: str,
        after_order_id: int,
        through_order_id: int,
        limit: int,
        *,
        tenant_id: int | None = None,
    ) -> list[int]:
        clauses = ["OrderId>?", "OrderId<=?"]
        params: list[Any] = [limit, after_order_id, through_order_id]
        if tenant_id is not None:
            clauses.append("TenantId=?")
            params.append(tenant_id)
        with self.hub.read(database) as session:
            rows = session.query(
                f"""
                SELECT TOP (?) OrderId FROM dbo.Orders
                WHERE {' AND '.join(clauses)}
                ORDER BY OrderId
                """,
                params,
            )
        return [int(row["OrderId"]) for row in rows]

    def distinct_tenants(self, database: str) -> list[int]:
        with self.hub.read(database) as session:
            rows = session.query(
                "SELECT DISTINCT TenantId FROM dbo.Orders ORDER BY TenantId"
            )
        return [int(row["TenantId"]) for row in rows]

    def fetch_orders_by_ids(
        self, database: str, order_ids: Sequence[int]
    ) -> dict[int, Order]:
        if not order_ids:
            return {}
        ids = list(order_ids)
        placeholders = ",".join("?" for _ in ids)
        with self.hub.read(database) as session:
            rows = session.query(
                f"""
                SELECT OrderId, TenantId, Status, Amount, Currency,
                       UpdatedAt, CreatedAt, Revision
                FROM dbo.Orders
                WHERE OrderId IN ({placeholders})
                """,
                ids,
            )
            item_rows = session.query(
                f"""
                SELECT OrderId, LineNumber, Sku, Quantity, UnitAmount
                FROM dbo.OrderItems
                WHERE OrderId IN ({placeholders})
                ORDER BY OrderId, LineNumber
                """,
                ids,
            )
        by_order: dict[int, list[OrderItem]] = {order_id: [] for order_id in ids}
        for item in item_rows:
            by_order[int(item["OrderId"])].append(row_to_item(item))
        return {
            int(row["OrderId"]): row_to_order(
                row, by_order[int(row["OrderId"])]
            )
            for row in rows
        }

    def max_order_id(self, database: str, tenant_id: int | None = None) -> int:
        with self.hub.read(database) as session:
            if tenant_id is None:
                row = session.query_one("SELECT ISNULL(MAX(OrderId),0) AS MaxId FROM dbo.Orders")
            else:
                row = session.query_one(
                    "SELECT ISNULL(MAX(OrderId),0) AS MaxId FROM dbo.Orders WHERE TenantId=?",
                    (tenant_id,),
                )
        return int(row["MaxId"]) if row else 0

    def next_order_id(self) -> int:
        with self.hub.read("legacy") as session:
            row = session.query_one(
                "SELECT ISNULL(MAX(OrderId),0)+1 AS NextId FROM dbo.Orders"
            )
        return int(row["NextId"])

    def write_order(
        self,
        session: Session,
        order: Order,
        *,
        receipt: dict[str, Any] | None = None,
        conditional: bool = False,
        known_exists: bool | None = None,
        check_receipt: bool = True,
    ) -> tuple[bool, str | None]:
        if known_exists is None:
            current_row = session.query_one(
                "SELECT Revision FROM dbo.Orders WHERE OrderId=?",
                (order.order_id,),
            )
        else:
            current_row = {"Revision": 1} if known_exists else None
        if conditional and current_row is not None:
            if int(current_row["Revision"]) >= order.revision:
                return False, None

        if current_row is None:
            sql = """
                INSERT INTO dbo.Orders
                    (TenantId, Status, Amount, Currency, UpdatedAt,
                     CreatedAt, Revision, OrderId)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?)
            """
            params: list[Any] = [
                order.tenant_id,
                order.status,
                order.amount,
                order.currency,
                order.updated_at,
                order.created_at,
                order.revision,
                order.order_id,
            ]
        else:
            sql = """
                UPDATE dbo.Orders
                SET TenantId=?, Status=?, Amount=?, Currency=?,
                    UpdatedAt=?, CreatedAt=?, Revision=?
                WHERE OrderId=?
            """
            params = [
                order.tenant_id,
                order.status,
                order.amount,
                order.currency,
                order.updated_at,
                order.created_at,
                order.revision,
                order.order_id,
            ]
        sql += " DELETE FROM dbo.OrderItems WHERE OrderId=?;"
        params.append(order.order_id)
        receipt_exists = False
        if receipt and check_receipt:
            existing = session.query_one(
                "SELECT WriteId FROM dbo.WriteReceipts WHERE WriteId=?",
                (receipt["writeId"],),
            )
            receipt_exists = existing is not None
        if receipt and not receipt_exists:
            sql += """
                INSERT INTO dbo.WriteReceipts
                    (WriteId, OrderId, TenantId, Revision, Kind,
                     PayloadHash, CommittedAt)
                VALUES (?, ?, ?, ?, ?, ?, ?);
            """
            params.extend(
                [
                    receipt["writeId"],
                    order.order_id,
                    order.tenant_id,
                    order.revision,
                    receipt["kind"],
                    receipt["payloadHash"],
                    receipt.get("committedAt") or datetime.now(timezone.utc),
                ]
            )
        session.execute(sql, params)
        if order.items:
            session.executemany(
                """
                INSERT INTO dbo.OrderItems
                    (OrderId, LineNumber, Sku, Quantity, UnitAmount)
                VALUES (?, ?, ?, ?, ?)
                """,
                [
                    (
                        order.order_id,
                        item.line_no,
                        item.sku,
                        item.quantity,
                        item.unit_amount,
                    )
                    for item in order.items
                ],
            )
        return True, receipt["writeId"] if receipt else None

    def delete_order(self, session: Session, order_id: int) -> None:
        session.execute("DELETE FROM dbo.OrderItems WHERE OrderId=?", (order_id,))
        session.execute("DELETE FROM dbo.Orders WHERE OrderId=?", (order_id,))

    def receipt(self, database: str, write_id: str) -> dict[str, Any] | None:
        with self.hub.read(database) as session:
            row = session.query_one(
                """
                SELECT WriteId, OrderId, TenantId, Revision, Kind,
                       PayloadHash, CommittedAt
                FROM dbo.WriteReceipts WHERE WriteId=?
                """,
                (write_id,),
            )
        if row is None:
            return None
        return {
            "writeId": str(row["WriteId"]),
            "orderId": int(row["OrderId"]),
            "tenantId": int(row["TenantId"]),
            "revision": int(row["Revision"]),
            "kind": str(row["Kind"]),
            "payloadHash": str(row["PayloadHash"]),
            "committedAt": str(row["CommittedAt"]),
            "database": database,
        }

    def receipt_max_time(self, database: str, tenant_id: int) -> str:
        with self.hub.read(database) as session:
            row = session.query_one(
                """
                SELECT CONVERT(varchar(33), ISNULL(MAX(CommittedAt), SYSUTCDATETIME()), 126)
                    AS CommitTime
                FROM dbo.WriteReceipts WHERE TenantId=?
                """,
                (tenant_id,),
            )
        value = str(row["CommitTime"]) if row else datetime.now(timezone.utc).isoformat()
        return f"{database}@{value}Z"

    def restore_snapshot(
        self, session: Session, snapshot: dict[str, Any] | None, order_id: int
    ) -> None:
        if snapshot is None:
            self.delete_order(session, order_id)
            return
        order = Order.from_payload(snapshot)
        self.write_order(session, order)

    def items_by_order(self, database: str, order_ids: Iterable[int]) -> dict[int, int]:
        ids = list(order_ids)
        if not ids:
            return {}
        placeholders = ",".join("?" for _ in ids)
        with self.hub.read(database) as session:
            rows = session.query(
                f"""
                SELECT OrderId, COUNT(*) AS ItemCount
                FROM dbo.OrderItems
                WHERE OrderId IN ({placeholders})
                GROUP BY OrderId
                """,
                ids,
            )
        return {int(row["OrderId"]): int(row["ItemCount"]) for row in rows}
