import pytest

from orderlab_gateway.errors import E_READONLY, GatewayError
from orderlab_gateway.guard import validate_read_only


@pytest.mark.parametrize(
    "sql",
    [
        "SELECT 'DROP TABLE dbo.OrderHeader' AS note -- DELETE FROM dbo.OrderHeader\n",
        "SELECT [update], [delete], [exec] FROM dbo.OrderHeader",
        "SELECT 1 /* INSERT UPDATE DELETE EXEC #temp */",
        "WITH q AS (SELECT 1 AS n) SELECT n FROM q",
    ],
)
def test_comments_strings_and_identifiers_are_not_false_positives(sql):
    validate_read_only(sql)


@pytest.mark.parametrize(
    "sql",
    [
        "INSERT INTO dbo.OrderHeader(TenantId) VALUES (1)",
        "UPDATE dbo.OrderHeader SET Amount = 0",
        "DELETE FROM dbo.OrderHeader",
        "MERGE dbo.OrderHeader AS t USING dbo.OrderHeader AS s ON 1=0 WHEN NOT MATCHED THEN INSERT DEFAULT VALUES",
        "DROP TABLE dbo.OrderHeader",
        "EXEC sp_who",
        "CREATE TABLE #scratch(id int)",
        "SELECT * INTO #scratch FROM dbo.OrderHeader",
    ],
)
def test_write_or_execution_forms_are_rejected_with_fragment(sql):
    with pytest.raises(GatewayError) as caught:
        validate_read_only(sql)
    assert caught.value.code == E_READONLY
    assert caught.value.fragment
    assert caught.value.offset is not None


def test_multiple_statements_are_rejected():
    with pytest.raises(GatewayError) as caught:
        validate_read_only("SELECT 1; DROP TABLE dbo.OrderHeader")
    assert caught.value.code == E_READONLY
    assert "DROP TABLE" in caught.value.fragment
