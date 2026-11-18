"""Process-pool worker for key-window Join and aggregation.

This module deliberately uses only the Python standard library so spawned
Windows workers stay small.  The master process owns SQLite; workers receive
immutable key-window groups and return immutable aggregate rows.
"""

from __future__ import annotations

from typing import Any


def aggregate_group(group: dict[str, Any]) -> tuple[dict[str, Any], int, int]:
    payment_id = str(group["payment_id"])
    window_start = int(group["window_start"])
    pay: dict[str, Any] | None = None
    refund: dict[str, Any] | None = None
    for event in group["events"]:
        side = str(event["side"])
        if side == "PAYMENT":
            pay = event
        elif side == "REFUND":
            refund = event

    unmatched = 0
    mismatched_currency = 0
    if pay is None:
        # A refund without a payment is not part of an inner window Join.
        if refund is not None:
            unmatched = 1
        return (
            {
                "payment_id": payment_id,
                "window_start": window_start,
                "pay_count": 0,
                "pay_amount_minor": 0,
                "refund_count": 0,
                "refund_amount_minor": 0,
                "currency": str(refund["currency"]) if refund else "",
            },
            unmatched,
            mismatched_currency,
        )

    pay_amount = int(pay["amount_minor"])
    refund_count = 0
    refund_amount = 0
    currency = str(pay["currency"])
    if refund is not None:
        if int(refund["event_time"]) < int(pay["event_time"]):
            unmatched = 1
        else:
            refund_count = 1
            refund_amount = int(refund["amount_minor"])
            if str(refund["currency"]) != currency:
                mismatched_currency = 1
                currency = "MIXED"
    return (
        {
            "payment_id": payment_id,
            "window_start": window_start,
            "pay_count": 1,
            "pay_amount_minor": pay_amount,
            "refund_count": refund_count,
            "refund_amount_minor": refund_amount,
            "currency": currency,
        },
        unmatched,
        mismatched_currency,
    )


def aggregate_groups(payload: list[dict[str, Any]]) -> dict[str, Any]:
    rows: list[dict[str, Any]] = []
    unmatched = 0
    mismatched_currency = 0
    for group in payload:
        row, group_unmatched, group_mismatch = aggregate_group(group)
        rows.append(row)
        unmatched += group_unmatched
        mismatched_currency += group_mismatch
    return {
        "rows": rows,
        "unmatched_refunds": unmatched,
        "currency_mismatches": mismatched_currency,
    }
