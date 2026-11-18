import pytest

from orderlab_gateway.config import load_config
from orderlab_gateway.engine import QueryEngine, QueryRequest
from orderlab_gateway.errors import E_DIALECT, E_READONLY, GatewayError


class NeverDatabase:
    def connect(self):
        raise AssertionError("database must not be touched")


def test_missing_or_invalid_dialect_is_stable_business_error():
    engine = QueryEngine(load_config("config/gateway.json"), database=NeverDatabase())
    for dialect in (None, "", "oracle", "MSSQL-CURRENT", ["mssql-current"]):
        with pytest.raises(GatewayError) as caught:
            engine.query(QueryRequest(sql="SELECT 1", dialect=dialect))
        assert caught.value.code == E_DIALECT
    status = engine.status()
    assert status["counters"]["rejected"] == 5
    assert status["counters"]["by_error"][E_DIALECT] == 5


def test_guard_runs_before_database_access():
    engine = QueryEngine(load_config("config/gateway.json"), database=NeverDatabase())
    with pytest.raises(GatewayError) as caught:
        engine.query(
            QueryRequest(
                sql="DELETE FROM dbo.OrderHeader WHERE TenantId=1",
                dialect="mssql-current",
            )
        )
    assert caught.value.code == E_READONLY


def test_windows_config_loads_without_credentials():
    config = load_config("config/gateway.windows.json")
    assert config.database.auth == "windows"
    assert config.database.trusted_connection is True
