from __future__ import annotations

import unittest
from pathlib import Path

from iovflow.errors import EngineError
from iovflow.sql_parser import parse_sql


ROOT = Path(__file__).resolve().parents[1]


class SQLParserTests(unittest.TestCase):
    def test_template_compiles_to_dag(self) -> None:
        plan = parse_sql((ROOT / "sql/window_5m.sql").read_text(encoding="utf-8"))
        self.assertEqual(plan.window.size_seconds, 300)
        self.assertEqual(
            [a.func for a in plan.aggregates],
            ["count_star", "count", "sum", "sum"],
        )
        stages = plan.default_stages(2, 4)
        self.assertEqual(
            [s["stage_type"] for s in stages],
            ["filter", "group_by_vehicle", "window_aggregate", "merge_skew", "sink"],
        )
        self.assertEqual(stages[2]["parallelism"], 4)
        self.assertEqual(stages[2]["max_concurrent"], 2)
        self.assertTrue(all(s["input_from"] and s["output_to"] for s in stages))

    def test_alternate_window_syntax(self) -> None:
        sql = (
            "select vehicle_id, time_bucket(INTERVAL '5 minutes', event_time) as ws, "
            "count(1) c, avg(mileage_delta) a from events where speed > 0 "
            "group by vehicle_id, ws"
        )
        plan = parse_sql(sql)
        self.assertEqual(plan.window.size_seconds, 300)
        self.assertEqual(plan.aggregates[0].func, "count_star")

    def test_epoch_floor_window_syntax(self) -> None:
        sql = (
            "select vehicle_id, to_timestamp(floor(epoch(event_time)/300)*300) as ws, "
            "max(battery_delta) m from events group by vehicle_id, ws"
        )
        self.assertEqual(parse_sql(sql).window.size_seconds, 300)

    def test_rejects_join_and_reports_fragment(self) -> None:
        sql = (
            "select e.vehicle_id, window_start(e.event_time,300) w, count(*) "
            "from events e join vehicles v on e.vehicle_id=v.vehicle_id "
            "group by e.vehicle_id,w"
        )
        with self.assertRaises(EngineError) as ctx:
            parse_sql(sql)
        self.assertEqual(ctx.exception.code, "IOV_SQL_UNSUPPORTED_JOIN")
        self.assertIn("join", (ctx.exception.fragment or "").lower())

    def test_rejects_subquery_cte_union_and_over(self) -> None:
        cases = {
            "select vehicle_id, window_start(event_time,300) w, count(*) "
            "from (select * from events) x group by vehicle_id,w": "IOV_SQL_UNSUPPORTED_SUBQUERY",
            "with x as (select * from events) select vehicle_id, "
            "window_start(event_time,300) w, count(*) from x group by vehicle_id,w": "IOV_SQL_UNSUPPORTED_CTE",
            "select vehicle_id, window_start(event_time,300) w, count(*) from events "
            "group by vehicle_id,w union all select vehicle_id, "
            "window_start(event_time,300) w, count(*) from events group by vehicle_id,w": "IOV_SQL_UNSUPPORTED_SET_OPERATION",
            "select vehicle_id, window_start(event_time,300) w, "
            "row_number() over () rn, count(*) from events group by vehicle_id,w": "IOV_SQL_UNSUPPORTED_WINDOW",
        }
        for sql, code in cases.items():
            with self.subTest(code=code):
                with self.assertRaises(EngineError) as ctx:
                    parse_sql(sql)
                self.assertEqual(ctx.exception.code, code)
                self.assertTrue(ctx.exception.fragment)

    def test_rejects_non_window_group(self) -> None:
        with self.assertRaises(EngineError) as ctx:
            parse_sql("select vehicle_id, count(*) from events group by vehicle_id")
        self.assertEqual(ctx.exception.code, "IOV_SQL_UNSUPPORTED_WINDOW")

    def test_rejects_having_and_order_by(self) -> None:
        sql = (
            "select vehicle_id, window_start(event_time,300) w, count(*) c "
            "from events group by vehicle_id,w having count(*)>1"
        )
        with self.assertRaises(EngineError) as ctx:
            parse_sql(sql)
        self.assertEqual(ctx.exception.code, "IOV_SQL_UNSUPPORTED_STRUCTURE")


if __name__ == "__main__":
    unittest.main()

