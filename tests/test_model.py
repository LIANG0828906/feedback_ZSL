from __future__ import annotations

from decimal import Decimal

import pytest

from orders_migration.errors import AppError
from orders_migration.model import Order


def test_order_payload_is_normalized_and_hashed_deterministically() -> None:
    order = Order.from_payload(
        {
            "orderId": 7,
            "tenantId": 2,
            "status": 3,
            "amount": "12.3",
            "currency": "usd",
            "createdAt": "2026-01-01T00:00:00Z",
            "updatedAt": "2026-01-02T00:00:00Z",
            "items": [
                {
                    "lineNo": 2,
                    "sku": "B",
                    "quantity": 2,
                    "unitAmount": "1.25",
                },
                {
                    "lineNo": 1,
                    "sku": "A",
                    "quantity": 1,
                    "unitAmount": "9.80",
                },
            ],
        }
    )
    assert order.amount == Decimal("12.30")
    assert order.currency == "USD"
    assert [item.line_no for item in order.items] == [1, 2]
    first_hash = order.payload_hash()
    payload = order.as_dict(include_items=True)
    payload.pop("revision")
    payload["revision"] = 0
    restored = Order.from_payload(payload)
    assert restored.payload_hash() == first_hash


def test_invalid_order_is_rejected() -> None:
    with pytest.raises(AppError) as caught:
        Order.from_payload(
            {
                "orderId": 7,
                "tenantId": 2,
                "status": 9,
                "amount": "1.00",
                "currency": "USD",
            }
        )
    assert caught.value.code == "E_VALIDATION"


def test_items_must_have_unique_line_numbers() -> None:
    with pytest.raises(AppError, match="duplicate item lineNo"):
        Order.from_payload(
            {
                "orderId": 7,
                "tenantId": 2,
                "status": 1,
                "amount": "1.00",
                "currency": "USD",
                "items": [
                    {"lineNo": 1, "sku": "A", "quantity": 1, "unitAmount": 1},
                    {"lineNo": 1, "sku": "B", "quantity": 1, "unitAmount": 1},
                ],
            }
        )

