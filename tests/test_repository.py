from __future__ import annotations

import unittest

from ordersync.errors import AppError
from ordersync.repository import (
    classify_difference,
    field_differences,
    prepare_order,
)

from .helpers import make_order


class RepositoryTests(unittest.TestCase):
    def test_prepare_order_derives_amount_and_line_amount(self) -> None:
        order = prepare_order(
            {
                "order_no": "ORD-1",
                "tenant_id": "T01",
                "status": "confirmed",
                "lines": [
                    {
                        "line_no": 2,
                        "sku": "SKU-2",
                        "quantity": 3,
                        "unit_price": "2.50",
                    },
                    {
                        "line_no": 1,
                        "sku": "SKU-1",
                        "quantity": 1,
                        "unit_price": "1.25",
                    },
                ],
            }
        )
        self.assertEqual(order["status"], "CONFIRMED")
        self.assertEqual(order["amount"], "8.75")
        self.assertEqual(order["lines"][0]["line_amount"], "7.50")

    def test_duplicate_line_numbers_are_invalid(self) -> None:
        with self.assertRaises(AppError) as raised:
            prepare_order(
                {
                    "order_no": "ORD-1",
                    "tenant_id": "T01",
                    "status": "CONFIRMED",
                    "lines": [
                        {
                            "line_no": 1,
                            "sku": "A",
                            "quantity": 1,
                            "unit_price": "1",
                        },
                        {
                            "line_no": 1,
                            "sku": "B",
                            "quantity": 1,
                            "unit_price": "1",
                        },
                    ],
                }
            )
        self.assertEqual(raised.exception.code, "E_INVALID_REQUEST")

    def test_field_differences_are_sorted_and_classified(self) -> None:
        primary = make_order("ORD-1", 4)
        replica = make_order("ORD-1", 3)
        replica["status"] = "CANCELLED"
        replica["lines"][0]["sku"] = "OLD"
        differences = field_differences(primary, replica)
        fields = [item["field"] for item in differences]
        self.assertEqual(fields, sorted(fields))
        self.assertIn("orders.status", fields)
        self.assertIn("orders.write_watermark", fields)
        self.assertIn("lines.1.sku", fields)
        self.assertEqual(
            classify_difference(primary, replica), "replica_behind"
        )


if __name__ == "__main__":
    unittest.main()

