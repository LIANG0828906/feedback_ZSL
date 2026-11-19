"""Generate independent dialect baselines for the two sample queries."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path

import duckdb


def _sha256(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def _copy_csv(con: duckdb.DuckDBPyConnection, query: str, target: Path) -> int:
    target.parent.mkdir(parents=True, exist_ok=True)
    tmp = target.with_name(target.name + ".tmp")
    path = str(tmp.resolve()).replace("\\", "/")
    con.execute(
        f"COPY ({query}) TO '{path}' (FORMAT CSV, HEADER TRUE, DELIMITER ',', "
        "QUOTE '\"', ESCAPE '\"', NULL '')"
    )
    tmp.replace(target)
    return int(con.execute(f"SELECT count(*) FROM ({query})").fetchone()[0])


def mysql_baseline(data_dir: Path, target: Path) -> int:
    fact = str((data_dir / "fact_sales.parquet").resolve()).replace("\\", "/")
    store = str((data_dir / "dim_store.csv").resolve()).replace("\\", "/")
    product = str((data_dir / "dim_product.parquet").resolve()).replace("\\", "/")
    con = duckdb.connect()
    con.execute("SET threads=2")
    store_rel = (
        "read_csv('"
        + store
        + "', header=true, nullstr='', columns={"
        "'store_id':'BIGINT','region':'VARCHAR','active':'BIGINT','store_name':'VARCHAR'})"
    )
    query = f"""
        SELECT
          CAST(s.event_ts AS DATE) AS sale_day,
          st.region AS region,
          p.category AS category,
          CAST(SUM(CAST(trunc(CAST(s.amount AS DOUBLE) / 7.0) AS BIGINT)) AS BIGINT)
            AS amount_div_7,
          CAST(SUM(CAST(s.qty - trunc(CAST(s.qty AS DOUBLE) / 3.0) * 3 AS BIGINT))
            AS BIGINT) AS qty_mod_3,
          CAST(COUNT(*) AS BIGINT) AS sale_count,
          AVG(s.amount) AS avg_amount
        FROM read_parquet('{fact}') AS s
        JOIN {store_rel} AS st ON s.store_id = st.store_id
        JOIN read_parquet('{product}') AS p ON s.product_id = p.product_id
        WHERE s.amount >= -500 AND st.active = 1
        GROUP BY CAST(s.event_ts AS DATE), st.region, p.category
        ORDER BY sale_day ASC NULLS FIRST, region ASC NULLS FIRST,
                 category ASC NULLS FIRST
    """
    rows = _copy_csv(con, query, target)
    con.close()
    return rows


def postgres_baseline(data_dir: Path, target: Path) -> int:
    stream = str((data_dir / "stream_events.csv").resolve()).replace("\\", "/")
    con = duckdb.connect()
    con.execute("SET threads=2")
    stream_rel = (
        "read_csv('"
        + stream
        + "', header=true, nullstr='', columns={"
        "'event_key':'VARCHAR','event_ts':'TIMESTAMP','user_id':'BIGINT',"
        "'payload':'VARCHAR','received_seq':'BIGINT'})"
    )
    delta = "(received_seq - 100)"
    query = f"""
        WITH ranked AS (
          SELECT *,
            row_number() OVER (
              PARTITION BY event_key
              ORDER BY event_ts ASC NULLS LAST, received_seq ASC NULLS LAST
            ) AS rn
          FROM {stream_rel}
          WHERE event_ts IS NOT NULL
        )
        SELECT
          event_key,
          event_ts,
          date_trunc('day', event_ts) AS event_day,
          CAST(trunc(CAST({delta} AS DOUBLE) / 7.0) AS BIGINT) AS seq_bucket,
          CAST(({delta}) - trunc(CAST({delta} AS DOUBLE) / 7.0) * 7 AS BIGINT)
            AS seq_mod,
          user_id,
          payload
        FROM ranked
        WHERE rn = 1
        ORDER BY event_key ASC NULLS LAST, event_ts ASC NULLS LAST,
                 event_day ASC NULLS LAST, seq_bucket ASC NULLS LAST,
                 seq_mod ASC NULLS LAST, user_id ASC NULLS LAST,
                 payload ASC NULLS LAST
    """
    rows = _copy_csv(con, query, target)
    con.close()
    return rows


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--data-dir", default="data")
    parser.add_argument("--output-dir", default="baseline")
    args = parser.parse_args()
    data_dir = Path(args.data_dir)
    out = Path(args.output_dir)
    mysql = out / "mysql_daily.csv"
    postgres = out / "postgres_streaming.csv"
    mysql_rows = mysql_baseline(data_dir, mysql)
    pg_rows = postgres_baseline(data_dir, postgres)
    manifest = {
        "mysql": {"rows": mysql_rows, "sha256": _sha256(mysql)},
        "postgres": {"rows": pg_rows, "sha256": _sha256(postgres)},
        "dialect_semantics": {
            "mysql": {
                "date_trunc_function": "DATE",
                "identifier_quote": "backtick",
                "integer_division": "truncate_toward_zero",
                "modulo": "sign_of_dividend",
                "null_order_asc": "FIRST",
                "null_order_desc": "LAST",
            },
            "postgres": {
                "date_trunc_function": "date_trunc",
                "identifier_quote": "double_quote",
                "integer_division": "truncate_toward_zero",
                "modulo": "sign_of_dividend",
                "null_order_asc": "LAST",
                "null_order_desc": "FIRST",
            },
        },
    }
    (out / "manifest.json").write_text(
        json.dumps(manifest, sort_keys=True, indent=2) + "\n", encoding="utf-8"
    )
    print(json.dumps(manifest, sort_keys=True))


if __name__ == "__main__":
    main()

