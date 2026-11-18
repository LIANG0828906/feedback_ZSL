from orderlab_gateway.config import load_config
from orderlab_gateway.normalizer import normalize_query


CONFIG = load_config("config/gateway.json")


def test_null_ordering_is_canonical_for_both_dialects():
    current = (
        "SELECT h.OrderNo, h.CustomerName FROM dbo.OrderHeader h "
        "WHERE h.TenantId=2 AND h.Status='NEW' "
        "ORDER BY CASE WHEN h.CustomerName IS NULL THEN 0 ELSE 1 END, "
        "h.CustomerName ASC, h.OrderId ASC"
    )
    legacy = (
        "SELECT h.OrderNo, h.CustomerName FROM dbo.OrderHeader h "
        "WHERE h.TenantId=2 AND h.Status='NEW' "
        "ORDER BY h.CustomerName ASC, h.OrderId ASC"
    )
    normalized_current = normalize_query(current, "mssql-current", CONFIG).sql
    normalized_legacy = normalize_query(legacy, "mssql-legacy", CONFIG).sql
    assert "CASE WHEN" in normalized_current
    assert "CASE WHEN (h.CustomerName) IS NULL THEN 0 ELSE 1 END" in normalized_legacy
    assert "ORDER BYCASE" not in normalized_legacy
    assert "h.CustomerName ASC" in normalized_legacy


def test_date_truncation_normalizes_to_identical_sql():
    current = (
        "SELECT DATETRUNC(day, h.OrderDate), DATETRUNC(second, h.OrderDate) "
        "FROM dbo.OrderHeader h"
    )
    legacy = (
        "SELECT DATEADD(day, DATEDIFF(day, 0, h.OrderDate), 0), "
        "DATEADD(second, DATEDIFF(second, 0, h.OrderDate), 0) "
        "FROM dbo.OrderHeader h"
    )
    normalized_current = normalize_query(current, "mssql-current", CONFIG)
    normalized_legacy = normalize_query(legacy, "mssql-legacy", CONFIG)
    assert normalized_current.sql == normalized_legacy.sql
    assert "CAST(h.OrderDate AS date)" in normalized_current.sql
    assert "DATETRUNC(second, h.OrderDate)" in normalized_current.sql


def test_implicit_string_number_comparison_is_normalized():
    legacy = (
        "SELECT l.Sku FROM dbo.OrderLine l "
        "WHERE l.TenantId=3 AND l.Status='SHIPPED' AND l.Sku = 1001"
    )
    current = legacy.replace("l.Sku = 1001", "TRY_CONVERT(bigint, l.Sku) = 1001")
    left = normalize_query(legacy, "mssql-legacy", CONFIG).sql
    right = normalize_query(current, "mssql-current", CONFIG).sql
    assert "TRY_CONVERT(bigint, l.Sku) = 1001" in left
    assert left == right


def test_query_hint_removed_and_normalization_idempotent():
    sql = (
        "SELECT h.OrderNo FROM dbo.OrderHeader h "
        "WHERE h.TenantId=1 AND h.Status='PAID' "
        "ORDER BY h.OrderDate OPTION (MAXDOP 1, FORCESEEK)"
    )
    first = normalize_query(sql, "mssql-legacy", CONFIG)
    second = normalize_query(first.sql, "mssql-legacy", CONFIG)
    assert "OPTION" not in first.sql
    assert first.sql == second.sql


def test_table_hints_are_removed_before_index_selection():
    sql = (
        "SELECT h.OrderNo FROM dbo.OrderHeader AS h "
        "WITH (NOLOCK, INDEX(IX_OrderHeader_CustomerName)) "
        "WHERE h.TenantId=1 AND h.Status='PAID' ORDER BY h.OrderNo"
    )
    normalized = normalize_query(sql, "mssql-legacy", CONFIG)
    assert "NOLOCK" not in normalized.sql
    assert "INDEX(IX_OrderHeader_CustomerName)" not in normalized.sql
    assert "AS h WHERE" in normalized.sql
