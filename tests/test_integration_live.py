import json
import os

import pytest

from orderlab_gateway.config import load_config
from orderlab_gateway.engine import QueryEngine, QueryRequest
from orderlab_gateway.errors import E_READONLY, GatewayError


pytestmark = pytest.mark.integration


def test_live_dialects_write_guard_and_summary():
    if os.environ.get("ORDERLAB_INTEGRATION") != "1":
        pytest.skip("set ORDERLAB_INTEGRATION=1 for live SQL Server tests")
    config = load_config(os.environ.get("ORDERLAB_CONFIG", "config/gateway.json"))
    samples = json.loads(config.samples_path.read_text(encoding="utf-8"))["samples"]
    engine = QueryEngine(config)
    for query_id in ("HDR_PAGE_DETAIL", "DATE_TRUNC_DETAIL", "STRING_NUM_DETAIL", "LINE_JOIN_DETAIL"):
        sample = next(item for item in samples if item["query_id"] == query_id)
        results = [
            engine.query(
                QueryRequest(
                    sql=sample["dialects"][dialect],
                    dialect=dialect,
                    query_id=query_id,
                    fallback=sample["fallback"],
                )
            )
            for dialect in ("mssql-current", "mssql-legacy")
        ]
        assert results[0]["columns"] == results[1]["columns"]
        assert results[0]["rows"] == results[1]["rows"]
        assert results[0]["index_names"] == results[1]["index_names"]
        assert all(row["physical_op"] != "Table Scan" for row in results[0]["plan_rows"])

    illegal = next(item for item in samples if item["query_id"] == "ILLEGAL_DELETE")
    with pytest.raises(GatewayError) as caught:
        engine.query(
            QueryRequest(
                sql=illegal["dialects"]["mssql-current"],
                dialect="mssql-current",
                query_id="ILLEGAL_DELETE",
            )
        )
    assert caught.value.code == E_READONLY

    summary_sample = next(item for item in samples if item["query_id"] == "SUMMARY_FALLBACK")
    summary = engine.query(
        QueryRequest(
            sql=summary_sample["dialects"]["mssql-current"],
            dialect="mssql-current",
            query_id="SUMMARY_FALLBACK",
            fallback="summary",
        )
    )
    assert summary["mode"] == "summary"
    assert summary["aggregate"] is True
    assert summary["summary_row_keys"]
