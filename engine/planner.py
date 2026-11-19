"""Logical planning, statistics-based join ordering, and pushdown."""

from __future__ import annotations

from dataclasses import dataclass, replace
from itertools import combinations
from typing import Any, Iterable

from .catalog import Catalog, TableInfo
from .dialects import DialectSpec
from .errors import ERROR_SQL, ExecutorError
from .sql import (
    AGGREGATE_NAMES,
    Between,
    BinaryOp,
    ColumnRef,
    Expr,
    FuncCall,
    IsNull,
    Literal,
    Query,
    UnaryOp,
    contains_aggregate,
    equality_sides,
    expr_aliases,
    split_conjuncts,
)


@dataclass(frozen=True)
class PartialAggregate:
    expr: FuncCall
    state_kinds: tuple[str, ...]


@dataclass
class PartialAggPlan:
    fact_alias: str
    group_exprs: tuple[Expr, ...]
    fact_group_exprs: frozenset[Expr]
    aggregates: tuple[PartialAggregate, ...]

    @property
    def group_index(self) -> dict[Expr, int]:
        return {expr: idx for idx, expr in enumerate(self.group_exprs)}

    @property
    def aggregate_index(self) -> dict[FuncCall, int]:
        return {spec.expr: idx for idx, spec in enumerate(self.aggregates)}


@dataclass
class BasePlan:
    alias: str
    table: TableInfo
    filters: tuple[Expr, ...] = ()
    partial: PartialAggPlan | None = None
    row_estimate: float = 1.0
    node_id: str = ""

    @property
    def aliases(self) -> frozenset[str]:
        return frozenset({self.alias})


@dataclass
class JoinPlan:
    left: "PlanNode"
    right: "PlanNode"
    conditions: tuple[Expr, ...]
    kind: str = "INNER"
    row_estimate: float = 1.0
    node_id: str = ""

    @property
    def aliases(self) -> frozenset[str]:
        return self.left.aliases | self.right.aliases


PlanNode = BasePlan | JoinPlan


@dataclass
class JoinPredicate:
    expr: Expr
    left: tuple[str, str]
    right: tuple[str, str]

    @property
    def alias_pair(self) -> frozenset[str]:
        return frozenset({self.left[0], self.right[0]})


@dataclass
class PlanningResult:
    query: Query
    dialect: DialectSpec
    catalog: Catalog
    bases: dict[str, BasePlan]
    root: PlanNode
    predicates: tuple[JoinPredicate, ...]
    residual_filters: tuple[Expr, ...]
    partial: PartialAggPlan | None
    baseline_max_intermediate: float
    planned_max_intermediate: float
    filtered_scan_estimate: float


def _expr_tables(
    expr: Expr, catalog: Catalog | None = None, query: Query | None = None
) -> set[str]:
    aliases = set(expr_aliases(expr))
    if catalog is not None and query is not None:
        for node in _walk(expr):
            if isinstance(node, ColumnRef) and node.table is None:
                alias, _ = catalog.resolve_column(query, node)
                aliases.add(alias)
    return aliases


def _selectivity(expr: Expr, catalog: Catalog, query: Query, alias: str) -> float:
    if isinstance(expr, Literal) and expr.kind == "bool":
        return 1.0 if expr.value else 0.0
    if isinstance(expr, UnaryOp) and expr.op.upper() == "NOT":
        return max(0.0, 1.0 - _selectivity(expr.operand, catalog, query, alias))
    if isinstance(expr, BinaryOp):
        op = expr.op.upper()
        if op == "AND":
            return _selectivity(expr.left, catalog, query, alias) * _selectivity(
                expr.right, catalog, query, alias
            )
        if op == "OR":
            return min(
                1.0,
                _selectivity(expr.left, catalog, query, alias)
                + _selectivity(expr.right, catalog, query, alias),
            )
        if op in {"=", "!=", "<>"}:
            if isinstance(expr.left, ColumnRef):
                try:
                    _, info = catalog.resolve_column(query, expr.left, {alias})
                    table = catalog.table(
                        next(t.name for t in query.tables if t.alias == alias)
                    )
                    if op == "=":
                        return min(1.0, 1.0 / table.distinct_count(info.name))
                    return 1.0 - min(1.0, 1.0 / table.distinct_count(info.name))
                except ExecutorError:
                    return 0.25 if op == "=" else 0.75
        if op in {"<", "<=", ">", ">="}:
            return 1.0 / 3.0
    if isinstance(expr, Between):
        return 1.0 / 4.0
    if isinstance(expr, IsNull):
        base = 0.1
        return 1.0 - base if expr.negated else base
    return 0.5


def _base_estimate(
    table: TableInfo, filters: Iterable[Expr], catalog: Catalog, query: Query, alias: str
) -> float:
    rows = float(table.row_count or catalog.count_rows(table))
    for item in filters:
        rows *= _selectivity(item, catalog, query, alias)
    return max(1.0, rows)


def _choose_fact_alias(
    query: Query, catalog: Catalog, bases: dict[str, BasePlan]
) -> str | None:
    aggregate_exprs = [
        node
        for expr in _query_expressions(query)
        for node in _walk(expr)
        if isinstance(node, FuncCall) and node.name.upper() in AGGREGATE_NAMES
    ]
    if not aggregate_exprs:
        return None
    candidates: list[str] = []
    for alias in bases:
        okay = True
        for agg in aggregate_exprs:
            aliases = _expr_tables(agg, catalog, query)
            if aliases and aliases != {alias}:
                okay = False
                break
            if agg.distinct:
                okay = False
                break
        for group_expr in query.group_by:
            aliases = _expr_tables(group_expr, catalog, query)
            if aliases and len(aliases) > 1:
                okay = False
                break
            if alias not in aliases and aliases:
                # A dimension-only group key is fine.
                continue
        if not okay:
            continue
        candidates.append(alias)
    if not candidates:
        return None
    return max(candidates, key=lambda a: bases[a].table.row_count or 0)


def _walk(expr: Expr | None) -> Iterable[Expr]:
    if expr is None:
        return
    yield expr
    if isinstance(expr, BinaryOp):
        yield from _walk(expr.left)
        yield from _walk(expr.right)
    elif isinstance(expr, UnaryOp):
        yield from _walk(expr.operand)
    elif isinstance(expr, FuncCall):
        for arg in expr.args:
            yield from _walk(arg)
    elif isinstance(expr, IsNull):
        yield from _walk(expr.expr)
    elif isinstance(expr, Between):
        yield from _walk(expr.expr)
        yield from _walk(expr.low)
        yield from _walk(expr.high)


def _query_expressions(query: Query) -> list[Expr]:
    out = [item.expr for item in query.select]
    out.extend(item.expr for item in query.order_by)
    if query.having is not None:
        out.append(query.having)
    return out


def _build_partial_plan(
    query: Query,
    catalog: Catalog,
    fact_alias: str,
    predicates: tuple[JoinPredicate, ...],
) -> PartialAggPlan | None:
    aggregates: list[PartialAggregate] = []
    for expr_root in _query_expressions(query):
        for node in _walk(expr_root):
            if not (isinstance(node, FuncCall) and node.name.upper() in AGGREGATE_NAMES):
                continue
            if node.distinct:
                return None
            name = node.name.upper()
            if name == "SUM":
                kinds = ("sum",)
            elif name == "AVG":
                kinds = ("sum", "count")
            elif name == "COUNT":
                kinds = ("count",)
            elif name in {"MIN", "MAX"}:
                kinds = (name.lower(),)
            else:
                return None
            spec = PartialAggregate(node, kinds)
            if spec not in aggregates:
                aggregates.append(spec)
    if not aggregates:
        return None

    fact_group_exprs: list[Expr] = []
    for group_expr in query.group_by:
        aliases = _expr_tables(group_expr, catalog, query)
        if aliases and aliases != {fact_alias}:
            continue
        if contains_aggregate(group_expr):
            return None
        fact_group_exprs.append(group_expr)

    # Join keys must survive the partial aggregate.
    for pred in predicates:
        if pred.left[0] == fact_alias:
            key = ColumnRef(fact_alias, pred.left[1])
            if key not in fact_group_exprs:
                fact_group_exprs.append(key)
        if pred.right[0] == fact_alias:
            key = ColumnRef(fact_alias, pred.right[1])
            if key not in fact_group_exprs:
                fact_group_exprs.append(key)

    if not fact_group_exprs:
        return None
    return PartialAggPlan(
        fact_alias=fact_alias,
        group_exprs=tuple(fact_group_exprs),
        fact_group_exprs=frozenset(fact_group_exprs),
        aggregates=tuple(aggregates),
    )


def _predicate_distinct(
    alias: str, column: str, bases: dict[str, BasePlan]
) -> float:
    base = bases.get(alias)
    if base is None:
        return 1.0
    table = base.table
    return max(1.0, min(table.distinct_count(column), base.row_estimate))


def _estimate_join_rows(
    left: PlanNode,
    right: PlanNode,
    conditions: tuple[Expr, ...],
    bases: dict[str, BasePlan],
) -> float:
    rows = left.row_estimate * right.row_estimate
    used = False
    for expr in conditions:
        sides = equality_sides(expr)
        if sides is None:
            rows *= 0.5
            continue
        lref, rref = sides
        if not lref.table or not rref.table:
            rows *= 0.5
            continue
        if lref.table in left.aliases and rref.table in right.aliases:
            ldist = _predicate_distinct(lref.table, lref.name, bases)
            rdist = _predicate_distinct(rref.table, rref.name, bases)
        elif rref.table in left.aliases and lref.table in right.aliases:
            ldist = _predicate_distinct(rref.table, rref.name, bases)
            rdist = _predicate_distinct(lref.table, lref.name, bases)
        else:
            continue
        rows = rows / max(ldist, rdist)
        used = True
    return max(1.0, rows if used else min(rows, left.row_estimate * right.row_estimate))


def _conditions_between(
    left: PlanNode, right: PlanNode, predicates: tuple[JoinPredicate, ...]
) -> tuple[Expr, ...]:
    out: list[Expr] = []
    for pred in predicates:
        if pred.left[0] in left.aliases and pred.right[0] in right.aliases:
            out.append(pred.expr)
        elif pred.right[0] in left.aliases and pred.left[0] in right.aliases:
            out.append(pred.expr)
    return tuple(out)


def _has_edge(
    left: PlanNode, right: PlanNode, predicates: tuple[JoinPredicate, ...]
) -> bool:
    return bool(_conditions_between(left, right, predicates))


def choose_join_order(
    bases: dict[str, BasePlan], predicates: tuple[JoinPredicate, ...]
) -> PlanNode:
    aliases = sorted(bases)
    if len(aliases) == 1:
        return bases[aliases[0]]
    best: dict[frozenset[str], PlanNode] = {
        frozenset({alias}): bases[alias] for alias in aliases
    }
    cost: dict[frozenset[str], tuple[float, float, str]] = {
        frozenset({alias}): (
            0.0,
            bases[alias].row_estimate,
            bases[alias].node_id,
        )
        for alias in aliases
    }
    for size in range(2, len(aliases) + 1):
        for subset_tuple in combinations(aliases, size):
            subset = frozenset(subset_tuple)
            first = subset_tuple[0]
            options: list[tuple[tuple[float, float, str], JoinPlan]] = []
            # Fix the first alias on the left to avoid duplicate orderings.
            for left_size in range(1, size):
                for left_tuple in combinations(subset_tuple, left_size):
                    if first not in left_tuple:
                        continue
                    left_set = frozenset(left_tuple)
                    right_set = subset - left_set
                    if left_set not in best or right_set not in best:
                        continue
                    left = best[left_set]
                    right = best[right_set]
                    conditions = _conditions_between(left, right, predicates)
                    if not conditions:
                        continue
                    est = _estimate_join_rows(left, right, conditions, bases)
                    node = JoinPlan(
                        left=left,
                        right=right,
                        conditions=conditions,
                        row_estimate=est,
                    )
                    left_cost = cost[left_set]
                    right_cost = cost[right_set]
                    max_rows = max(est, left_cost[1], right_cost[1])
                    total = left_cost[0] + right_cost[0] + est
                    signature = f"{left.node_id}|{right.node_id}"
                    options.append(((max_rows, total, signature), node))
            if not options:
                continue
            options.sort(key=lambda pair: pair[0])
            _, chosen = options[0]
            best[subset] = chosen
            cost[subset] = (
                cost[chosen.left.aliases][0]
                + cost[chosen.right.aliases][0]
                + chosen.row_estimate,
                max(
                    chosen.row_estimate,
                    cost[chosen.left.aliases][1],
                    cost[chosen.right.aliases][1],
                ),
                f"{chosen.left.node_id}|{chosen.right.node_id}",
            )
    full = frozenset(aliases)
    if full not in best:
        raise ExecutorError(
            ERROR_SQL,
            "join graph is disconnected; only connected equi-join plans are supported",
        )
    return best[full]


def _render_plan_signature(node: PlanNode) -> str:
    if isinstance(node, BasePlan):
        suffix = "+partial" if node.partial else ""
        return f"{node.alias}{suffix}"
    return f"({_render_plan_signature(node.left)} J {_render_plan_signature(node.right)})"


def _assign_node_ids(node: PlanNode) -> None:
    if isinstance(node, BasePlan):
        node.node_id = f"scan_{node.alias}"
        return
    _assign_node_ids(node.left)
    _assign_node_ids(node.right)
    node.node_id = f"join_{_render_plan_signature(node).replace(' ', '').replace('(', 'L').replace(')', 'R')}"


def _baseline_intermediate(
    bases: dict[str, BasePlan], predicates: tuple[JoinPredicate, ...]
) -> float:
    if len(bases) <= 1:
        return max((b.row_estimate for b in bases.values()), default=1.0)
    # The baseline keeps raw scans (no filters or partial aggregation) but uses
    # the same deterministic DP to avoid comparing against a strawman order.
    raw_bases = {
        alias: BasePlan(
            alias=base.alias,
            table=base.table,
            filters=(),
            row_estimate=float(base.table.row_count or 1),
            node_id=f"base_{alias}",
        )
        for alias, base in bases.items()
    }
    root = choose_join_order(raw_bases, predicates)

    def max_rows(node: PlanNode) -> float:
        if isinstance(node, BasePlan):
            return node.row_estimate
        return max(
            node.row_estimate, max_rows(node.left), max_rows(node.right)
        )

    return max_rows(root)


def _physical_scan_estimate(
    query: Query, catalog: Catalog, base: BasePlan
) -> float:
    return _base_estimate(
        base.table, base.filters, catalog, query, base.alias
    )


def plan_query(
    query: Query, catalog: Catalog, dialect: DialectSpec
) -> PlanningResult:
    if len(query.tables) > 12:
        raise ExecutorError(ERROR_SQL, "at most 12 input tables are supported")
    query = _normalize_group_aliases(query)
    catalog.ensure_statistics()
    bases: dict[str, BasePlan] = {}
    for ref in query.tables:
        bases[ref.alias] = BasePlan(
            alias=ref.alias,
            table=catalog.table(ref.name),
            filters=(),
            node_id=f"scan_{ref.alias}",
        )

    predicates: list[JoinPredicate] = []
    filters_by_alias: dict[str, list[Expr]] = {alias: [] for alias in bases}
    residual: list[Expr] = []

    all_conditions: list[Expr] = []
    for edge in query.joins:
        if edge.kind not in {"INNER", "CROSS"}:
            raise ExecutorError(
                ERROR_SQL,
                f"{edge.kind} JOIN is not supported by this bounded executor",
            )
        all_conditions.extend(split_conjuncts(edge.condition))
    all_conditions.extend(split_conjuncts(query.where))

    for condition in all_conditions:
        aliases = _expr_tables(condition, catalog, query)
        if len(aliases) == 1:
            alias = next(iter(aliases))
            if alias not in filters_by_alias:
                residual.append(condition)
            else:
                filters_by_alias[alias].append(condition)
            continue
        if len(aliases) == 0:
            if isinstance(condition, Literal) and condition.kind == "bool":
                if not condition.value:
                    filters_by_alias[next(iter(filters_by_alias))].append(condition)
                continue
            residual.append(condition)
            continue
        sides = equality_sides(condition)
        if sides is None:
            residual.append(condition)
            continue
        left, right = sides
        if not left.table:
            left_alias, left_info = catalog.resolve_column(
                query, left, set(bases)
            )
            left = ColumnRef(left_alias, left_info.name)
        if not right.table:
            right_alias, right_info = catalog.resolve_column(
                query, right, set(bases)
            )
            right = ColumnRef(right_alias, right_info.name)
        if not left.table or not right.table:
            residual.append(condition)
            continue
        condition = BinaryOp("=", left, right)
        predicates.append(
            JoinPredicate(
                expr=condition,
                left=(left.table, left.name),
                right=(right.table, right.name),
            )
        )

    for alias, filters in filters_by_alias.items():
        bases[alias].filters = tuple(filters)
        bases[alias].row_estimate = _base_estimate(
            bases[alias].table,
            bases[alias].filters,
            catalog,
            query,
            alias,
        )

    fact_alias = _choose_fact_alias(query, catalog, bases)
    partial = None
    if fact_alias:
        partial = _build_partial_plan(
            query, catalog, fact_alias, tuple(predicates)
        )
        if partial is not None:
            base = bases[fact_alias]
            base.partial = partial
            # Grouping by one or more fact keys normally reduces rows. Use a
            # conservative estimate; execution metrics replace this in reports.
            base.row_estimate = max(
                1.0, min(base.row_estimate, base.row_estimate * 0.45)
            )

    for alias, base in bases.items():
        base.node_id = f"scan_{alias}"
    root = choose_join_order(bases, tuple(predicates))
    _assign_node_ids(root)

    planned_max = root.row_estimate
    if isinstance(root, JoinPlan):
        def recurse(node: PlanNode) -> float:
            if isinstance(node, BasePlan):
                return node.row_estimate
            return max(
                node.row_estimate, recurse(node.left), recurse(node.right)
            )

        planned_max = recurse(root)
    baseline_max = _baseline_intermediate(bases, tuple(predicates))
    return PlanningResult(
        query=query,
        dialect=dialect,
        catalog=catalog,
        bases=bases,
        root=root,
        predicates=tuple(predicates),
        residual_filters=tuple(residual),
        partial=partial,
        baseline_max_intermediate=baseline_max,
        planned_max_intermediate=planned_max,
        filtered_scan_estimate=sum(b.row_estimate for b in bases.values()),
    )


def _normalize_group_aliases(query: Query) -> Query:
    resolved: list[Expr] = []
    changed = False
    for expr in query.group_by:
        replacement = expr
        if isinstance(expr, Literal) and expr.kind == "int":
            ordinal = int(expr.value)
            if 1 <= ordinal <= len(query.select):
                replacement = query.select[ordinal - 1].expr
                changed = True
        if isinstance(expr, ColumnRef) and expr.table is None:
            for item in query.select:
                if item.alias and item.alias.lower() == expr.name.lower():
                    replacement = item.expr
                    changed = True
                    break
        resolved.append(replacement)
    if not changed:
        return query
    return replace(query, group_by=tuple(resolved))


def plan_to_tree(
    node: PlanNode,
    actual_rows: dict[str, int] | None = None,
    partition_count: int | None = None,
) -> dict[str, Any]:
    actual_rows = actual_rows or {}
    if isinstance(node, BasePlan):
        children: list[dict[str, Any]] = []
        out = {
            "node_id": node.node_id,
            "operator": "scan",
            "table": node.table.name,
            "alias": node.alias,
            "estimated_rows": round(node.row_estimate, 3),
            "actual_rows": actual_rows.get(node.node_id),
            "filters": [repr(item) for item in node.filters],
        }
        if node.partial:
            children.append(
                {
                    "node_id": node.node_id + "_partial_agg",
                    "operator": "partial_aggregate",
                    "group_keys": [repr(item) for item in node.partial.group_exprs],
                    "aggregates": [
                        item.expr.name.upper() for item in node.partial.aggregates
                    ],
                    "estimated_rows": round(node.row_estimate, 3),
                    "actual_rows": actual_rows.get(node.node_id + "_partial_agg"),
                    "children": [],
                }
            )
        out["children"] = children
        return out
    left_tree = plan_to_tree(node.left, actual_rows, partition_count)
    right_tree = plan_to_tree(node.right, actual_rows, partition_count)
    children = [left_tree, right_tree]
    if partition_count is not None:
        children = [
            {
                "node_id": node.node_id + "_left_repartition",
                "operator": "hash_repartition",
                "side": "left",
                "partitions": partition_count,
                "actual_rows": left_tree.get("actual_rows"),
                "children": [left_tree],
            },
            {
                "node_id": node.node_id + "_right_repartition",
                "operator": "hash_repartition",
                "side": "right",
                "partitions": partition_count,
                "actual_rows": right_tree.get("actual_rows"),
                "children": [right_tree],
            },
        ]
    return {
        "node_id": node.node_id,
        "operator": "hash_join",
        "join_kind": node.kind,
        "conditions": [repr(item) for item in node.conditions],
        "estimated_rows": round(node.row_estimate, 3),
        "actual_rows": actual_rows.get(node.node_id),
        "children": children,
    }

