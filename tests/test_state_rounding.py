from __future__ import annotations

import unittest
import random
from decimal import Decimal

from docagg.config import RoundingSpec
from docagg.numeric import parse_numeric
from docagg.reference import _aggregate_reference
from docagg.rounding import round_significant
from docagg.state import MetricState


class StateTests(unittest.TestCase):
    def test_abs_sorted_compensated_sum_survives_cancellation(self) -> None:
        state = MetricState(name="total", op="sum", field="x")
        state.update_many(
            [
                parse_numeric(1e16),
                parse_numeric(1.0),
                parse_numeric(-1e16),
            ]
        )
        result = state.result()
        self.assertIsNotNone(result)
        assert result is not None
        self.assertEqual(result["value"], 1.0)
        self.assertLessEqual(
            abs(result["value"] - 1.0),
            result["error_estimate"],
        )
        self.assertIn("neumaier-compensated-sum", result["error_path"])

    def test_merge_accumulates_merge_bound(self) -> None:
        left = MetricState(name="total", op="sum", field="x")
        right = MetricState(name="total", op="sum", field="x")
        left.update_many([parse_numeric(1e16), parse_numeric(1.0)])
        right.update_many([parse_numeric(-1e16), parse_numeric(2.0)])
        left.merge(right)
        result = left.result()
        self.assertIsNotNone(result)
        assert result is not None
        self.assertEqual(result["merge_events"], 1)
        self.assertGreater(result["error_components"]["merge_bound"], 0)
        self.assertEqual(result["count"], 4)

    def test_stddev_requires_two_samples(self) -> None:
        one = MetricState(name="s", op="stdDev", field="x")
        one.update(parse_numeric(1.0))
        self.assertIsNone(one.result())
        two = MetricState(name="s", op="stdDev", field="x")
        two.update_many([parse_numeric(1.0), parse_numeric(3.0)])
        result = two.result()
        self.assertIsNotNone(result)
        assert result is not None
        self.assertAlmostEqual(result["value"], 2**0.5)

    def test_randomized_declared_bounds_cover_reference(self) -> None:
        rng = random.Random(20261120)
        for trial in range(80):
            values = []
            for _ in range(rng.randint(2, 20)):
                kind = rng.choice(("float", "decimal", "integer", "large_integer"))
                if kind == "float":
                    values.append(
                        rng.uniform(-9.9, 9.9) * 10.0 ** rng.randint(-20, 20)
                    )
                elif kind == "decimal":
                    units = rng.randint(-10**8, 10**8)
                    values.append(f"{Decimal(units).scaleb(-4):.4f}")
                elif kind == "integer":
                    values.append(rng.randint(-10**12, 10**12))
                else:
                    values.append(rng.randint(2**53, 2**63))
            parsed = [parse_numeric(value) for value in values]
            chunks = [parsed[index::3] for index in range(3)]
            for op in ("sum", "avg", "min", "max", "stdDev"):
                state = MetricState(name="x", op=op, field="x")
                state.update_many(chunks[0])
                for chunk in chunks[1:]:
                    partial = MetricState(name="x", op=op, field="x")
                    partial.update_many(chunk)
                    state.merge(partial)
                result = state.result()
                reference = _aggregate_reference("x", op, "x", parsed)
                self.assertIsNotNone(result, (trial, op))
                assert result is not None
                deviation = abs(
                    Decimal.from_float(result["value"]) - reference.value
                )
                self.assertLessEqual(
                    deviation,
                    Decimal(str(result["error_estimate"])),
                    (trial, op),
                )


class RoundingTests(unittest.TestCase):
    def test_rounding_modes_and_stage(self) -> None:
        value = Decimal("12.345")
        half_even = round_significant(
            value, RoundingSpec(mode="half_even", significant_digits=4)
        )
        toward_zero = round_significant(
            Decimal("-12.345"), RoundingSpec(mode="toward_zero", significant_digits=4)
        )
        away_from_zero = round_significant(
            Decimal("-12.341"), RoundingSpec(mode="up", significant_digits=4)
        )
        self.assertEqual(half_even, Decimal("12.34"))
        self.assertEqual(toward_zero, Decimal("-12.34"))
        self.assertEqual(away_from_zero, Decimal("-12.35"))


if __name__ == "__main__":
    unittest.main()
