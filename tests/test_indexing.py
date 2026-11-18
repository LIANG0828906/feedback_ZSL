import json

from orderlab_gateway.config import load_config
from orderlab_gateway.guard import validate_read_only
from orderlab_gateway.indexing import analyze_query, apply_index_hints
from orderlab_gateway.normalizer import normalize_query


CONFIG = load_config("config/gateway.json")
SAMPLES = json.loads(open("config/samples.json", encoding="utf-8").read())["samples"]


def _analysis(sql, dialect):
    validated = validate_read_only(sql)
    normalized = normalize_query(validated.sql, dialect, CONFIG)
    return normalized, analyze_query(normalized.sql, CONFIG)


def test_covering_index_matches_for_both_dialects():
    expected = {
        "HDR_PAGE_DETAIL": ["IX_OrderHeader_Tenant_Status_OrderDate"],
        "NULL_ORDER_DETAIL": ["IX_OrderHeader_Tenant_Status_OrderDate"],
        "DATE_TRUNC_DETAIL": ["IX_OrderHeader_Tenant_Status_OrderDate"],
        "STRING_NUM_DETAIL": ["IX_OrderLine_Tenant_Status_Sku"],
        "LINE_JOIN_DETAIL": [
            "IX_OrderHeader_Tenant_Status_OrderDate",
            "IX_OrderLine_Tenant_Status_OrderId",
        ],
    }
    for sample in SAMPLES:
        if sample["query_id"] not in expected:
            continue
        names = set()
        for dialect, sql in sample["dialects"].items():
            _, analysis = _analysis(sql, dialect)
            names.add(tuple(index.name for index in analysis.expected_indexes))
        assert names == {tuple(expected[sample["query_id"]])}


def test_uncovered_column_has_locatable_fragment():
    sample = next(item for item in SAMPLES if item["query_id"] == "HDR_UNCOVERED_REJECT")
    _, analysis = _analysis(sample["dialects"]["mssql-current"], "mssql-current")
    assert not analysis.can_execute_detail
    assert analysis.condition_fragment == "h.ShipAddress"


def test_disjunctive_tenant_or_status_does_not_count_as_index_prefix():
    sql = (
        "SELECT h.OrderNo FROM dbo.OrderHeader AS h "
        "WHERE (h.TenantId=1 AND h.Status='PAID') OR h.TenantId=2"
    )
    _, analysis = _analysis(sql, "mssql-current")
    assert not analysis.can_execute_detail
    assert analysis.tenant_id is None


def test_index_hint_is_inserted_after_alias_in_all_table_references():
    sample = next(item for item in SAMPLES if item["query_id"] == "LINE_JOIN_DETAIL")
    for dialect, sql in sample["dialects"].items():
        normalized, analysis = _analysis(sql, dialect)
        hinted = apply_index_hints(normalized.sql, analysis)
        assert hinted.count("WITH (INDEX([IX_OrderHeader_Tenant_Status_OrderDate]))") == 1
        assert hinted.count("WITH (INDEX([IX_OrderLine_Tenant_Status_OrderId]))") == 1
        assert "AS h WITH (INDEX(" in hinted
