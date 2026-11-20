"""Dependency graph construction, validation, impact analysis, and edge diffs."""

from __future__ import annotations

from collections import OrderedDict, defaultdict, deque
from dataclasses import dataclass
from typing import Any, Iterable, Mapping, Sequence

from .errors import invalid_graph
from .hashing import sha256_json
from .params import GroupSpec
from .scene import Scene


COMPUTE_NODES: tuple[str, ...] = (
    "intersections",
    "visibility",
    "shadow_factor",
    "lighting",
    "loss",
)

GROUP_PREFIX = "group:"


@dataclass(frozen=True, order=True)
class Edge:
    source: str
    target: str
    kind: str = "data"
    reason: str = ""

    def to_ordered(self) -> OrderedDict[str, Any]:
        return OrderedDict(
            (
                ("source", self.source),
                ("target", self.target),
                ("kind", self.kind),
                ("reason", self.reason),
            )
        )


@dataclass(frozen=True)
class GraphDiff:
    added: tuple[Edge, ...]
    deleted: tuple[Edge, ...]
    changed: tuple[tuple[Edge, Edge], ...]
    graph_version_before: str
    graph_version_after: str

    def to_ordered(self) -> OrderedDict[str, Any]:
        return OrderedDict(
            (
                ("graph_version_before", self.graph_version_before),
                ("graph_version_after", self.graph_version_after),
                ("added", [edge.to_ordered() for edge in self.added]),
                ("deleted", [edge.to_ordered() for edge in self.deleted]),
                (
                    "changed",
                    [
                        OrderedDict(
                            (
                                ("before", before.to_ordered()),
                                ("after", after.to_ordered()),
                            )
                        )
                        for before, after in self.changed
                    ],
                ),
            )
        )


def _group_node(group_id: str) -> str:
    return GROUP_PREFIX + group_id


class DependencyGraph:
    """Directed acyclic graph between parameter groups and render nodes."""

    def __init__(
        self,
        *,
        nodes: Sequence[str],
        edges: Iterable[Edge],
        group_specs: Sequence[GroupSpec],
        structure_hash: str,
        validate: bool = True,
    ) -> None:
        self.nodes: tuple[str, ...] = tuple(nodes)
        self.edges: tuple[Edge, ...] = tuple(edges)
        self.group_specs: tuple[GroupSpec, ...] = tuple(group_specs)
        self.structure_hash = structure_hash
        self._group_kinds = OrderedDict((group.id, group.kind) for group in group_specs)
        self._outgoing: dict[str, list[str]] = defaultdict(list)
        self._incoming: dict[str, list[str]] = defaultdict(list)
        self._build_indexes()
        if validate:
            self.validate()
        self.version = self._compute_version()

    @property
    def node_set(self) -> frozenset[str]:
        return frozenset(self.nodes)

    def _build_indexes(self) -> None:
        self._outgoing = defaultdict(list)
        self._incoming = defaultdict(list)
        for edge in self.edges:
            self._outgoing[edge.source].append(edge.target)
            self._incoming[edge.target].append(edge.source)
        for node in self.nodes:
            self._outgoing.setdefault(node, [])
            self._incoming.setdefault(node, [])

    def validate(self) -> None:
        if not self.nodes:
            raise invalid_graph("dependency graph has no nodes")
        if len(self.nodes) != len(set(self.nodes)):
            raise invalid_graph("dependency graph contains duplicate nodes")
        node_set = set(self.nodes)
        expected_group_nodes = {_group_node(group.id) for group in self.group_specs}
        actual_group_nodes = {
            node for node in self.nodes if node.startswith(GROUP_PREFIX)
        }
        if expected_group_nodes != actual_group_nodes:
            raise invalid_graph(
                "group nodes do not match parameter groups",
                missing=sorted(expected_group_nodes - actual_group_nodes),
                extra=sorted(actual_group_nodes - expected_group_nodes),
            )
        if not set(COMPUTE_NODES).issubset(node_set):
            raise invalid_graph(
                "dependency graph is missing compute nodes",
                missing=sorted(set(COMPUTE_NODES) - node_set),
            )
        pairs: set[tuple[str, str]] = set()
        for edge in self.edges:
            if edge.source not in node_set or edge.target not in node_set:
                raise invalid_graph(
                    "edge endpoint is not a graph node",
                    source=edge.source,
                    target=edge.target,
                )
            pair = (edge.source, edge.target)
            if pair in pairs:
                raise invalid_graph(
                    "dependency graph contains duplicate edge",
                    source=edge.source,
                    target=edge.target,
                )
            pairs.add(pair)

        indegree = {node: 0 for node in self.nodes}
        for edge in self.edges:
            indegree[edge.target] += 1
        ready = deque(node for node in self.nodes if indegree[node] == 0)
        visited = 0
        while ready:
            node = ready.popleft()
            visited += 1
            for target in self._outgoing[node]:
                indegree[target] -= 1
                if indegree[target] == 0:
                    ready.append(target)
        if visited != len(self.nodes):
            cyclic = sorted(node for node, degree in indegree.items() if degree > 0)
            raise invalid_graph("dependency graph contains a cycle", nodes=cyclic)

        for group_node in expected_group_nodes:
            if not self._outgoing.get(group_node):
                raise invalid_graph("parameter group is disconnected", node=group_node)
        if not self._incoming.get("loss"):
            raise invalid_graph("loss node has no dependencies")

    def _compute_version(self) -> str:
        return sha256_json(self.to_ordered(include_version=False))

    def to_ordered(self, *, include_version: bool = True) -> OrderedDict[str, Any]:
        result: OrderedDict[str, Any] = OrderedDict()
        if include_version:
            result["graph_version"] = self.version
        result["structure_hash"] = self.structure_hash
        result["nodes"] = list(self.nodes)
        result["groups"] = [
            OrderedDict(
                (
                    ("id", group.id),
                    ("kind", group.kind),
                    ("parameters", list(group.parameters)),
                )
            )
            for group in self.group_specs
        ]
        result["edges"] = [edge.to_ordered() for edge in self.edges]
        return result

    def group_kind(self, node: str) -> str | None:
        if not node.startswith(GROUP_PREFIX):
            return None
        group_id = node[len(GROUP_PREFIX) :]
        return self._group_kinds.get(group_id)

    def _group_ids_for_node(self, node: str) -> list[str]:
        if node.startswith(GROUP_PREFIX):
            return [node[len(GROUP_PREFIX) :]]
        visited = {node}
        queue = deque([node])
        groups: set[str] = set()
        while queue:
            current = queue.popleft()
            for parent in self._incoming.get(current, []):
                if parent in visited:
                    continue
                visited.add(parent)
                if parent.startswith(GROUP_PREFIX):
                    groups.add(parent[len(GROUP_PREFIX) :])
                else:
                    queue.append(parent)
        return [group_id for group_id in self._group_kinds if group_id in groups]

    def upstream_groups(self, node: str) -> tuple[str, ...]:
        if node not in self.node_set:
            raise invalid_graph("unknown graph node", node=node)
        return tuple(self._group_ids_for_node(node))

    def descendants(self, node: str, *, include_self: bool = False) -> tuple[str, ...]:
        if node not in self.node_set:
            raise invalid_graph("unknown graph node", node=node)
        visited: set[str] = {node} if include_self else set()
        queue = deque([node])
        while queue:
            current = queue.popleft()
            for target in self._outgoing.get(current, []):
                if target not in visited:
                    visited.add(target)
                    queue.append(target)
        return tuple(target for target in COMPUTE_NODES if target in visited)

    def impacted_nodes(self, changed_groups: Iterable[str]) -> tuple[str, ...]:
        impacted: set[str] = set()
        for group_id in changed_groups:
            if group_id not in self._group_kinds:
                raise invalid_graph("unknown parameter group", group_id=group_id)
            impacted.update(self.descendants(_group_node(group_id)))
        return tuple(node for node in COMPUTE_NODES if node in impacted)

    def diff(self, previous: "DependencyGraph") -> GraphDiff:
        old_by_pair = {(edge.source, edge.target): edge for edge in previous.edges}
        new_by_pair = {(edge.source, edge.target): edge for edge in self.edges}
        added = tuple(
            sorted(
                (new_by_pair[pair] for pair in new_by_pair.keys() - old_by_pair.keys()),
                key=lambda edge: (edge.source, edge.target),
            )
        )
        deleted = tuple(
            sorted(
                (old_by_pair[pair] for pair in old_by_pair.keys() - new_by_pair.keys()),
                key=lambda edge: (edge.source, edge.target),
            )
        )
        changed: list[tuple[Edge, Edge]] = []
        for pair in sorted(old_by_pair.keys() & new_by_pair.keys()):
            before = old_by_pair[pair]
            after = new_by_pair[pair]
            if before != after:
                changed.append((before, after))
        return GraphDiff(
            added=added,
            deleted=deleted,
            changed=tuple(changed),
            graph_version_before=previous.version,
            graph_version_after=self.version,
        )


def _group_compute_targets(kind: str) -> tuple[tuple[str, str], ...]:
    if kind == "camera":
        return (("intersections", "camera rays determine analytic intersections"),)
    if kind == "geometry":
        return (
            ("intersections", "object geometry changes ray hits"),
            ("shadow_factor", "object geometry changes shadow ray blockers"),
        )
    if kind == "material":
        return (("lighting", "diffuse albedo contributes to Lambert lighting"),)
    if kind == "light":
        return (
            ("shadow_factor", "light direction controls soft shadow rays"),
            ("lighting", "light direction and intensity contribute to Lambert lighting"),
        )
    if kind == "mixed":
        merged: list[tuple[str, str]] = []
        for member_kind in ("camera", "geometry", "material", "light"):
            for target, reason in _group_compute_targets(member_kind):
                if (target, reason) not in merged:
                    merged.append((target, reason))
        return tuple(merged)
    raise invalid_graph("unknown group kind", kind=kind)


def build_dependency_graph(
    scene: Scene,
    group_specs: Sequence[GroupSpec],
) -> DependencyGraph:
    nodes = [_group_node(group.id) for group in group_specs] + list(COMPUTE_NODES)
    edges: list[Edge] = []

    for group in group_specs:
        for target, reason in _group_compute_targets(group.kind):
            edges.append(
                Edge(
                    source=_group_node(group.id),
                    target=target,
                    kind="parameter",
                    reason=reason,
                )
            )

    edges.extend(
        (
            Edge(
                "intersections",
                "visibility",
                "data",
                "visibility comes from the nearest analytic hit",
            ),
            Edge(
                "intersections",
                "shadow_factor",
                "data",
                "shadow rays start at analytic hit points",
            ),
            Edge(
                "intersections",
                "lighting",
                "data",
                "surface normals drive Lambert terms",
            ),
            Edge(
                "visibility",
                "shadow_factor",
                "data",
                "only visible primary hits need shadowing",
            ),
            Edge(
                "visibility",
                "lighting",
                "data",
                "visibility masks the shaded image",
            ),
            Edge(
                "shadow_factor",
                "lighting",
                "data",
                "soft shadow factor attenuates Lambert lighting",
            ),
            Edge("lighting", "loss", "loss", "pixel L2 is computed from lit colors"),
        )
    )

    graph = DependencyGraph(
        nodes=nodes,
        edges=edges,
        group_specs=group_specs,
        structure_hash=scene.structure_hash(),
    )
    return graph

