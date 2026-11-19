from __future__ import annotations

import unittest
from pathlib import Path

import duckdb

from engine.catalog import Catalog
from engine.dialects import compile_expr, compile_order_item, get_dialect
from engine.sql import BinaryOp, ColumnRef, Literal, OrderItem, parse_sql


ROOT = Path(__file__).resolve().parents[1]


@unittest.skipUnless(
    (ROOT / "data" / "schema.json").exists(),
    "run scripts/generate_data.py before dialect integration tests",
)
class DialectTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.catalog = Catalog.load(ROOT / "data")

    def test_quote_styles_parse(self) -> None:
        mysql = parse_sql(
            "SELECT `s`.`amount` FROM `fact_sales` AS `s` "
            "ORDER BY `s`.`amount` ASC"
        )
        postgres = parse_sql(
            'SELECT "s"."amount" FROM "fact_sales" AS "s" '
            'ORDER BY "s"."amount" DESC'
        )
        self.assertTrue(mysql.sql.startswith("SELECT `"))
        self.assertTrue(postgres.sql.startswith("SELECT \""))
        self.assertIsNotNone(mysql.where is None)

    def test_mysql_negative_div_and_mod(self) -> None:
        query = parse_sql("SELECT 1 FROM fact_sales")
        dialect = get_dialect("mysql")
        catalog = self.catalog
        div_sql = compile_expr(
            BinaryOp("DIV", Literal(-7, "int"), Literal(3, "int")),
            dialect,
            catalog,
            query,
        )
        mod_sql = compile_expr(
            BinaryOp("MOD", Literal(-7, "int"), Literal(3, "int")),
            dialect,
            catalog,
            query,
        )
        con = duckdb.connect()
        self.assertEqual(con.execute(f"SELECT {div_sql}").fetchone()[0], -2)
        self.assertEqual(con.execute(f"SELECT {mod_sql}").fetchone()[0], -1)
        con.close()

    def test_null_order_defaults(self) -> None:
        query = parse_sql("SELECT event_ts FROM fact_sales ORDER BY event_ts")
        mysql = compile_order_item(
            OrderItem(ColumnRef(None, "event_ts"), "ASC", None),
            get_dialect("mysql"),
            self.catalog,
            query,
            {"event_ts"},
        )
        postgres = compile_order_item(
            OrderItem(ColumnRef(None, "event_ts"), "ASC", None),
            get_dialect("postgres"),
            self.catalog,
            query,
            {"event_ts"},
        )
        self.assertIn("NULLS FIRST", mysql)
        self.assertIn("NULLS LAST", postgres)


if __name__ == "__main__":
    unittest.main()

