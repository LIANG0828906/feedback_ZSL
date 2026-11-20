from __future__ import annotations

import unittest
from decimal import Decimal

from docagg.errors import ErrorCode, NumericParseError
from docagg.numeric import parse_numeric


class NumericParsingTests(unittest.TestCase):
    def test_fixed_decimal_preserves_scale_and_digits(self) -> None:
        parsed = parse_numeric("0.1200")
        self.assertEqual(parsed.source_type, "decimal")
        self.assertEqual(parsed.scale, 4)
        self.assertEqual(parsed.precision_digits, 4)
        self.assertEqual(parsed.decimal_exact, Decimal("0.1200"))
        self.assertGreaterEqual(parsed.conversion_error, 0)

    def test_long_integer_marks_downgrade_instead_of_truncating(self) -> None:
        raw = 2**53 + 1
        parsed = parse_numeric(raw)
        self.assertEqual(parsed.source_type, "integer")
        self.assertTrue(parsed.downgraded)
        self.assertEqual(parsed.exact.numerator, raw)
        self.assertIn("integer-precision-loss", parsed.precision_path)
        self.assertGreater(parsed.conversion_error, Decimal(0))

    def test_power_of_two_integer_is_binary64_exact(self) -> None:
        parsed = parse_numeric(2**60)
        self.assertEqual(parsed.source_type, "integer")
        self.assertFalse(parsed.downgraded)
        self.assertEqual(parsed.conversion_error, Decimal(0))

    def test_float_is_tracked_as_binary64(self) -> None:
        parsed = parse_numeric(0.1)
        self.assertEqual(parsed.source_type, "float")
        self.assertFalse(parsed.downgraded)
        self.assertEqual(parsed.conversion_error, Decimal(0))
        self.assertEqual(parsed.decimal_exact, Decimal.from_float(0.1))

    def test_bool_and_non_numeric_string_are_incomparable(self) -> None:
        for raw in (True, "not-a-number", {"x": 1}, float("nan"), float("inf")):
            with self.subTest(raw=raw):
                with self.assertRaises(NumericParseError) as raised:
                    parse_numeric(raw)
                self.assertEqual(
                    raised.exception.code, ErrorCode.TYPE_INCOMPARABLE
                )

    def test_overflow_is_explicit(self) -> None:
        with self.assertRaises(NumericParseError) as raised:
            parse_numeric(10**400)
        self.assertEqual(raised.exception.code, ErrorCode.NUMERIC_OVERFLOW)


if __name__ == "__main__":
    unittest.main()
