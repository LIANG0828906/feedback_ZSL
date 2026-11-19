from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from decimal import Decimal, InvalidOperation, ROUND_HALF_UP
from typing import Any

from .errors import validation_error


def parse_datetime(value: Any) -> datetime:
    if isinstance(value, datetime):
        result = value
    elif isinstance(value, str):
        normalized = value.strip()
        if normalized.endswith("Z"):
            normalized = normalized[:-1] + "+00:00"
        try:
            result = datetime.fromisoformat(normalized)
        except ValueError as exc:
            raise validation_error("timestamp must be ISO-8601", value=value) from exc
    else:
        raise validation_error("timestamp must be ISO-8601", value=value)
    if result.tzinfo is None:
        result = result.replace(tzinfo=timezone.utc)
    return result.astimezone(timezone.utc)


def iso_time(value: datetime) -> str:
    utc = value.astimezone(timezone.utc)
    text = utc.isoformat(timespec="milliseconds")
    return text.replace("+00:00", "Z")


def money(value: Any) -> Decimal:
    try:
        number = Decimal(str(value))
    except (InvalidOperation, ValueError) as exc:
        raise validation_error("amount must be a decimal value", value=value) from exc
    if number < 0:
        raise validation_error("amount must not be negative", value=value)
    return number.quantize(Decimal("0.01"), rounding=ROUND_HALF_UP)


@dataclass
class OrderItem:
    line_no: int
    sku: str
    quantity: int
    unit_amount: Decimal

    def as_dict(self) -> dict[str, Any]:
        return {
            "lineNo": self.line_no,
            "sku": self.sku,
            "quantity": self.quantity,
            "unitAmount": format(self.unit_amount, ".2f"),
        }


@dataclass
class Order:
    order_id: int
    tenant_id: int
    status: int
    amount: Decimal
    currency: str
    updated_at: datetime
    created_at: datetime
    revision: int
    items: list[OrderItem] = field(default_factory=list)

    def as_dict(self, include_items: bool = True) -> dict[str, Any]:
        body: dict[str, Any] = {
            "orderId": self.order_id,
            "tenantId": self.tenant_id,
            "status": self.status,
            "amount": format(self.amount, ".2f"),
            "currency": self.currency,
            "updatedAt": iso_time(self.updated_at),
            "createdAt": iso_time(self.created_at),
            "revision": self.revision,
            "itemCount": len(self.items),
        }
        if include_items:
            body["items"] = [item.as_dict() for item in self.items]
        return body

    def payload_hash(self) -> str:
        canonical = json.dumps(
            {
                "orderId": self.order_id,
                "tenantId": self.tenant_id,
                "status": self.status,
                "amount": format(self.amount, ".2f"),
                "currency": self.currency,
                "updatedAt": iso_time(self.updated_at),
                "createdAt": iso_time(self.created_at),
                "revision": self.revision,
                "items": [
                    {
                        "lineNo": item.line_no,
                        "sku": item.sku,
                        "quantity": item.quantity,
                        "unitAmount": format(item.unit_amount, ".2f"),
                    }
                    for item in sorted(self.items, key=lambda value: value.line_no)
                ],
            },
            sort_keys=True,
            separators=(",", ":"),
        )
        return hashlib.sha256(canonical.encode("utf-8")).hexdigest()

    @classmethod
    def from_payload(cls, payload: dict[str, Any]) -> "Order":
        if not isinstance(payload, dict):
            raise validation_error("request body must be an object")
        try:
            order_id = int(payload["orderId"])
            tenant_id = int(payload["tenantId"])
            status = int(payload["status"])
        except (KeyError, TypeError, ValueError) as exc:
            raise validation_error(
                "orderId, tenantId and status are required integers"
            ) from exc
        if order_id <= 0 or tenant_id < 0:
            raise validation_error("orderId must be positive and tenantId non-negative")
        if not 0 <= status <= 4:
            raise validation_error("status must be between 0 and 4")
        currency = str(payload.get("currency", "CNY")).upper()
        if currency not in {"CNY", "USD", "EUR"}:
            raise validation_error("currency must be CNY, USD or EUR")
        now = datetime.now(timezone.utc)
        updated_at = parse_datetime(payload.get("updatedAt", now))
        created_at = parse_datetime(payload.get("createdAt", now))
        if updated_at < created_at:
            raise validation_error("updatedAt must not be earlier than createdAt")

        raw_items = payload.get("items", [])
        if not isinstance(raw_items, list):
            raise validation_error("items must be an array")
        items: list[OrderItem] = []
        seen_lines: set[int] = set()
        for index, raw in enumerate(raw_items):
            if not isinstance(raw, dict):
                raise validation_error("each item must be an object", index=index)
            try:
                line_no = int(raw.get("lineNo", index + 1))
                quantity = int(raw["quantity"])
            except (KeyError, TypeError, ValueError) as exc:
                raise validation_error(
                    "item lineNo and quantity must be integers", index=index
                ) from exc
            if line_no <= 0 or quantity <= 0:
                raise validation_error(
                    "item lineNo and quantity must be positive", index=index
                )
            if line_no in seen_lines:
                raise validation_error("duplicate item lineNo", lineNo=line_no)
            seen_lines.add(line_no)
            sku = str(raw.get("sku", "")).strip()
            if not sku or len(sku) > 40:
                raise validation_error(
                    "item sku is required and must be <= 40 characters", index=index
                )
            items.append(
                OrderItem(
                    line_no=line_no,
                    sku=sku,
                    quantity=quantity,
                    unit_amount=money(raw.get("unitAmount", 0)),
                )
            )
        items.sort(key=lambda value: value.line_no)
        return cls(
            order_id=order_id,
            tenant_id=tenant_id,
            status=status,
            amount=money(payload.get("amount", 0)),
            currency=currency,
            updated_at=updated_at,
            created_at=created_at,
            revision=int(payload.get("revision", 0)),
            items=items,
        )

    def with_revision_and_time(self, revision: int, updated_at: datetime) -> "Order":
        return Order(
            order_id=self.order_id,
            tenant_id=self.tenant_id,
            status=self.status,
            amount=self.amount,
            currency=self.currency,
            updated_at=updated_at,
            created_at=self.created_at,
            revision=revision,
            items=list(self.items),
        )


@dataclass
class OrderSummary:
    order_id: int
    tenant_id: int
    status: int
    amount: Decimal
    currency: str
    created_at: datetime
    updated_at: datetime
    revision: int
    item_count: int
    item_fingerprint: str

    def comparable(self) -> dict[str, Any]:
        return {
            "tenantId": self.tenant_id,
            "status": self.status,
            "amount": format(self.amount, ".2f"),
            "currency": self.currency,
            "itemCount": self.item_count,
            "itemFingerprint": self.item_fingerprint,
            "createdAt": iso_time(self.created_at),
            "updatedAt": iso_time(self.updated_at),
            "revision": self.revision,
        }

    def as_dict(self) -> dict[str, Any]:
        return {
            "orderId": self.order_id,
            **self.comparable(),
        }
