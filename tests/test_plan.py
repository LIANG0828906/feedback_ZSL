import pytest

from orderlab_gateway.errors import E_PLAN_INDEX, E_PLAN_SCAN, E_PLAN_SORT_SPILL, GatewayError
from orderlab_gateway.plan import parse_plan, validate_plan


NS = "http://schemas.microsoft.com/sqlserver/2004/07/showplan"


def _xml(access: str = "seek", spill: bool = False) -> str:
    if access == "seek":
        relop = (
            '<RelOp NodeId="1" PhysicalOp="Index Seek" LogicalOp="Index Seek" '
            'EstimateRows="5" ActualRows="5">'
            '<IndexScan><Object Table="[OrderHeader]" '
            'Index="[IX_OrderHeader_Tenant_Status_OrderDate]" /></IndexScan>'
            "</RelOp>"
        )
    elif access == "scan":
        relop = (
            '<RelOp NodeId="1" PhysicalOp="Clustered Index Scan" '
            'LogicalOp="Clustered Index Scan" EstimateRows="20000" ActualRows="20000">'
            '<IndexScan><Object Table="[OrderHeader]" Index="[PK_OrderHeader]" />'
            "</IndexScan></RelOp>"
        )
    else:
        relop = (
            '<RelOp NodeId="1" PhysicalOp="Key Lookup" LogicalOp="Key Lookup" '
            'EstimateRows="5" ActualRows="5">'
            '<IndexScan><Object Table="[OrderHeader]" Index="[PK_OrderHeader]" />'
            "</IndexScan></RelOp>"
        )
    sort = ""
    if spill:
        sort = '<RelOp NodeId="2" PhysicalOp="Sort" LogicalOp="Sort" SpillToTempDb="1" />'
    return (
        f'<ShowPlanXML xmlns="{NS}"><BatchSequence><Batch><Statements><StmtSimple>'
        f"<QueryPlan>{sort}{relop}</QueryPlan>"
        "</StmtSimple></Statements></Batch></BatchSequence></ShowPlanXML>"
    )


def test_plan_parser_unquotes_names_and_reads_actual_rows():
    info = parse_plan(_xml())
    assert info.access == "index_seek"
    assert info.seek_indexes == {"IX_OrderHeader_Tenant_Status_OrderDate"}
    assert info.rows[0].actual_rows == 5
    assert info.rows[0].table == "OrderHeader"


def test_scan_is_rejected_with_plan_rows():
    with pytest.raises(GatewayError) as caught:
        validate_plan(parse_plan(_xml("scan")), ["IX_OrderHeader_Tenant_Status_OrderDate"])
    assert caught.value.code == E_PLAN_SCAN
    assert caught.value.details["plan_rows"][0]["physical_op"] == "Clustered Index Scan"


def test_sort_spill_is_rejected():
    with pytest.raises(GatewayError) as caught:
        validate_plan(
            parse_plan(_xml(spill=True)),
            ["IX_OrderHeader_Tenant_Status_OrderDate"],
        )
    assert caught.value.code == E_PLAN_SORT_SPILL


def test_key_lookup_means_index_is_not_covering():
    with pytest.raises(GatewayError) as caught:
        validate_plan(parse_plan(_xml("lookup")), ["IX_OrderHeader_Tenant_Status_OrderDate"])
    assert caught.value.code == E_PLAN_INDEX
