from __future__ import annotations

from dataclasses import dataclass, field
from xml.etree import ElementTree

from .errors import (
    E_PLAN_INDEX,
    E_PLAN_SCAN,
    E_PLAN_SORT_SPILL,
    GatewayError,
)


NS = {"p": "http://schemas.microsoft.com/sqlserver/2004/07/showplan"}
SCAN_OPS = {
    "table scan",
    "clustered index scan",
    "index scan",
    "columnstore index scan",
    "remote scan",
}
SEEK_OPS = {
    "index seek",
    "clustered index seek",
}


@dataclass(slots=True)
class PlanRow:
    node_id: int | None
    physical_op: str
    logical_op: str
    table: str | None
    index: str | None
    actual_rows: float | None = None
    estimated_rows: float | None = None

    def to_dict(self) -> dict[str, object]:
        return {
            "node_id": self.node_id,
            "physical_op": self.physical_op,
            "logical_op": self.logical_op,
            "table": self.table,
            "index": self.index,
            "actual_rows": self.actual_rows,
            "estimated_rows": self.estimated_rows,
        }


@dataclass(slots=True)
class PlanInfo:
    rows: list[PlanRow] = field(default_factory=list)
    scanned: bool = False
    sort_spill: bool = False
    lookup: bool = False
    seek_indexes: set[str] = field(default_factory=set)
    access: str = "no_data_access"

    def to_dict(self) -> dict[str, object]:
        return {
            "access": self.access,
            "index_seeks": sorted(self.seek_indexes),
            "rows": [row.to_dict() for row in self.rows],
        }


def _number(value: str | None) -> float | None:
    if value is None:
        return None
    try:
        return float(value)
    except ValueError:
        return None


def _node_id(relop: ElementTree.Element) -> int | None:
    value = relop.get("NodeId")
    try:
        return int(value) if value is not None else None
    except ValueError:
        return None


def _clean_object_name(value: str | None) -> str | None:
    if value is None:
        return None
    if value.startswith("[") and value.endswith("]"):
        return value[1:-1].replace("]]", "]")
    return value


def _actual_rows(relop: ElementTree.Element) -> float | None:
    value = _number(relop.get("ActualRows"))
    if value is not None:
        return value
    total = 0.0
    found = False
    for counter in relop.findall(".//p:RunTimeCountersPerThread", NS):
        current = _number(counter.get("ActualRows"))
        if current is not None:
            total += current
            found = True
    return total if found else None


def parse_plan(xml_text: str) -> PlanInfo:
    root = ElementTree.fromstring(xml_text)
    info = PlanInfo()
    object_count = 0
    for relop in root.findall(".//p:RelOp", NS):
        physical = relop.get("PhysicalOp", "")
        logical = relop.get("LogicalOp", "")
        physical_lower = physical.lower()
        logical_lower = logical.lower()
        actual_rows = _actual_rows(relop)
        estimated_rows = _number(relop.get("EstimateRows"))

        objects = relop.findall("./p:IndexScan/p:Object", NS)
        objects += relop.findall("./p:TableScan/p:Object", NS)
        table = None
        index = None
        for obj in objects:
            table = table or _clean_object_name(obj.get("Table"))
            index = index or _clean_object_name(obj.get("Index"))

        if objects:
            object_count += 1
            info.rows.append(
                PlanRow(
                    node_id=_node_id(relop),
                    physical_op=physical,
                    logical_op=logical,
                    table=table,
                    index=index,
                    actual_rows=actual_rows,
                    estimated_rows=estimated_rows,
                )
            )
            if physical_lower in SCAN_OPS or logical_lower in SCAN_OPS:
                info.scanned = True
            if physical_lower in SEEK_OPS or logical_lower in SEEK_OPS:
                if index:
                    info.seek_indexes.add(index)
            if "lookup" in physical_lower or "lookup" in logical_lower:
                info.lookup = True
        elif "sort" in physical_lower or "sort" in logical_lower:
            info.rows.append(
                PlanRow(
                    node_id=_node_id(relop),
                    physical_op=physical,
                    logical_op=logical,
                    table=None,
                    index=None,
                    actual_rows=actual_rows,
                    estimated_rows=estimated_rows,
                )
            )

        spill = relop.get("SpillToTempDb")
        if spill and spill not in {"0", "false", "False"}:
            info.sort_spill = True
        for child in relop.iter():
            child_spill = child.get("SpillToTempDb")
            if child_spill and child_spill not in {"0", "false", "False"}:
                info.sort_spill = True

    if info.seek_indexes and not info.scanned and not info.lookup:
        info.access = "index_seek"
    elif info.scanned:
        info.access = "scan"
    elif info.lookup:
        info.access = "key_lookup"
    elif object_count:
        info.access = "other_access"
    return info


def validate_plan(
    info: PlanInfo,
    expected_indexes: list[str],
    *,
    require_seek: bool = True,
    reject_sort_spill: bool = True,
    fragment: str | None = None,
) -> None:
    details = {"plan_rows": [row.to_dict() for row in info.rows], "access": info.access}
    if reject_sort_spill and info.sort_spill:
        raise GatewayError(
            E_PLAN_SORT_SPILL,
            "actual execution plan reports a sort spill to tempdb",
            fragment=fragment,
            details=details,
        )
    if info.scanned:
        raise GatewayError(
            E_PLAN_SCAN,
            "execution plan contains a table or index scan",
            fragment=fragment,
            details=details,
        )
    if info.lookup:
        raise GatewayError(
            E_PLAN_INDEX,
            "execution plan contains a key lookup, so the selected index is not covering",
            fragment=fragment,
            details=details,
        )
    if require_seek:
        missing = sorted(
            index_name
            for index_name in expected_indexes
            if index_name.lower() not in {name.lower() for name in info.seek_indexes}
        )
        if missing:
            details["missing_index_seeks"] = missing
            raise GatewayError(
                E_PLAN_INDEX,
                "execution plan did not seek the selected covering index",
                fragment=fragment,
                details=details,
            )
