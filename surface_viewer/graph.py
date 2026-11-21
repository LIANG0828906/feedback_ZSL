from __future__ import annotations

from collections import deque
from dataclasses import dataclass
from typing import Iterable, Mapping

from .errors import ErrorCode, SurfaceViewerError
from .hashing import sha256_json
from .model import (
    PARAMETER_NAMES,
    SurfaceAssembly,
    SurfacePatch,
    changed_parameter_names,
)

ARTIFACT_KINDS = ("mesh", "normals", "density", "preview")

REQUIRED_PARAMETER_DEPENDENCIES: dict[str, tuple[str, ...]] = {
    "mesh": ("control_points", "surface_type", "precision_level"),
    "normals": (
        "control_points",
        "surface_type",
        "precision_level",
        "flip_normal",
    ),
    "density": ("surface_type", "precision_level"),
    "preview": (
        "control_points",
        "surface_type",
        "precision_level",
        "flip_normal",
    ),
}

REQUIRED_ARTIFACT_DEPENDENCIES: dict[str, tuple[str, ...]] = {
    "mesh": (),
    "normals": (),
    "density": (),
    "preview": ("mesh", "normals", "density"),
}


def parameter_node_id(patch_id: str, name: str) -> str:
    return f"param:{patch_id}:{name}"


def artifact_node_id(patch_id: str, kind: str) -> str:
    return f"artifact:{patch_id}:{kind}"


@dataclass(frozen=True)
class Node:
    node_id: str
    patch_id: str
    node_type: str
    name: str
    version: str

    def to_dict(self) -> dict[str, str]:
        return {
            "node_id": self.node_id,
            "patch_id": self.patch_id,
            "type": self.node_type,
            "name": self.name,
            "version": self.version,
        }


@dataclass(frozen=True)
class Edge:
    source: str
    target: str
    param_version: str

    @property
    def identity(self) -> tuple[str, str]:
        return (self.source, self.target)

    def to_dict(self) -> dict[str, str]:
        return {
            "source": self.source,
            "target": self.target,
            "param_version": self.param_version,
        }


@dataclass(frozen=True)
class GraphDiff:
    added_nodes: tuple[str, ...]
    removed_nodes: tuple[str, ...]
    added_edges: tuple[tuple[str, str], ...]
    removed_edges: tuple[tuple[str, str], ...]
    changed_nodes: tuple[tuple[str, str, str], ...] = ()
    changed_edges: tuple[tuple[str, str, str, str], ...] = ()

    @property
    def changed(self) -> bool:
        return bool(
            self.added_nodes
            or self.removed_nodes
            or self.added_edges
            or self.removed_edges
            or self.changed_nodes
            or self.changed_edges
        )

    def to_dict(self) -> dict[str, object]:
        return {
            "changed": self.changed,
            "added_nodes": list(self.added_nodes),
            "removed_nodes": list(self.removed_nodes),
            "added_edges": [list(edge) for edge in self.added_edges],
            "removed_edges": [list(edge) for edge in self.removed_edges],
            "changed_nodes": [list(node) for node in self.changed_nodes],
            "changed_edges": [list(edge) for edge in self.changed_edges],
        }


@dataclass(frozen=True)
class InvalidatedArtifact:
    artifact_id: str
    patch_id: str
    kind: str
    reasons: tuple[str, ...]

    def to_dict(self) -> dict[str, object]:
        return {
            "artifact_id": self.artifact_id,
            "patch_id": self.patch_id,
            "kind": self.kind,
            "reasons": list(self.reasons),
        }


@dataclass(frozen=True)
class InvalidationReport:
    changed_parameters: tuple[str, ...]
    dependency_closure: tuple[str, ...]
    affected: tuple[InvalidatedArtifact, ...]
    reused: tuple[str, ...]
    conservative: tuple[InvalidatedArtifact, ...]

    @property
    def affected_ids(self) -> tuple[str, ...]:
        return tuple(item.artifact_id for item in self.affected)

    @property
    def affected_patches(self) -> tuple[str, ...]:
        return tuple(sorted({item.patch_id for item in self.affected}))

    def to_dict(self) -> dict[str, object]:
        return {
            "changed_parameters": list(self.changed_parameters),
            "dependency_closure": list(self.dependency_closure),
            "affected": [item.to_dict() for item in self.affected],
            "reused": list(self.reused),
            "conservative": [item.to_dict() for item in self.conservative],
            "affected_patches": list(self.affected_patches),
            "reused_count": len(self.reused),
            "recomputed_count": len(self.affected),
        }


class DependencyGraph:
    def __init__(self, nodes: Iterable[Node], edges: Iterable[Edge]):
        self.nodes: dict[str, Node] = {node.node_id: node for node in nodes}
        unique_edges: dict[tuple[str, str], Edge] = {}
        for edge in edges:
            unique_edges[edge.identity] = edge
        self.edges: tuple[Edge, ...] = tuple(
            sorted(unique_edges.values(), key=lambda edge: (edge.source, edge.target))
        )
        self._outgoing: dict[str, list[Edge]] = {}
        self._incoming: dict[str, list[Edge]] = {}
        for edge in self.edges:
            self._outgoing.setdefault(edge.source, []).append(edge)
            self._incoming.setdefault(edge.target, []).append(edge)

    @classmethod
    def build(cls, assembly: SurfaceAssembly) -> "DependencyGraph":
        nodes: list[Node] = []
        edges: list[Edge] = []
        for patch in assembly.patches:
            versions = patch.parameter_versions
            for name in PARAMETER_NAMES:
                nodes.append(
                    Node(
                        node_id=parameter_node_id(patch.patch_id, name),
                        patch_id=patch.patch_id,
                        node_type="parameter",
                        name=name,
                        version=versions[name],
                    )
                )

            for kind in ARTIFACT_KINDS:
                signature = parameter_signature_for_patch(patch, kind)
                nodes.append(
                    Node(
                        node_id=artifact_node_id(patch.patch_id, kind),
                        patch_id=patch.patch_id,
                        node_type="artifact",
                        name=kind,
                        version=signature,
                    )
                )
                for name in REQUIRED_PARAMETER_DEPENDENCIES[kind]:
                    edges.append(
                        Edge(
                            source=parameter_node_id(patch.patch_id, name),
                            target=artifact_node_id(patch.patch_id, kind),
                            param_version=versions[name],
                        )
                    )
                for dependency_kind in REQUIRED_ARTIFACT_DEPENDENCIES[kind]:
                    edges.append(
                        Edge(
                            source=artifact_node_id(patch.patch_id, dependency_kind),
                            target=artifact_node_id(patch.patch_id, kind),
                            param_version=parameter_signature_for_patch(
                                patch, dependency_kind
                            ),
                        )
                    )
        return cls(nodes, edges)

    @property
    def node_count(self) -> int:
        return len(self.nodes)

    @property
    def edge_count(self) -> int:
        return len(self.edges)

    def artifact_nodes(self) -> tuple[Node, ...]:
        return tuple(
            sorted(
                (node for node in self.nodes.values() if node.node_type == "artifact"),
                key=lambda node: node.node_id,
            )
        )

    def parameter_node(self, patch_id: str, name: str) -> Node:
        return self.nodes[parameter_node_id(patch_id, name)]

    def artifact_signature(self, artifact_id: str) -> str:
        node = self.nodes.get(artifact_id)
        if node is None or node.node_type != "artifact":
            raise KeyError(artifact_id)
        return node.version

    def dependency_versions(self, artifact_id: str) -> dict[str, str]:
        return {
            edge.source: edge.param_version
            for edge in self._incoming.get(artifact_id, ())
        }

    def missing_edges(self) -> list[tuple[str, str]]:
        expected: set[tuple[str, str]] = set()
        for node in self.artifact_nodes():
            for name in REQUIRED_PARAMETER_DEPENDENCIES[node.name]:
                expected.add((parameter_node_id(node.patch_id, name), node.node_id))
            for kind in REQUIRED_ARTIFACT_DEPENDENCIES[node.name]:
                expected.add((artifact_node_id(node.patch_id, kind), node.node_id))
        actual = {edge.identity for edge in self.edges}
        return sorted(expected - actual)

    def validate_complete(self) -> None:
        dangling = sorted(
            edge
            for edge in self.edges
            if edge.source not in self.nodes or edge.target not in self.nodes
        )
        if dangling:
            raise SurfaceViewerError(
                ErrorCode.DEP_GRAPH_INCOMPLETE,
                "dependency graph contains edges with missing nodes",
                details={
                    "dangling_edges": [
                        [edge.source, edge.target] for edge in dangling
                    ]
                },
            )

        missing = self.missing_edges()
        if missing:
            first_target = missing[0][1]
            raise SurfaceViewerError(
                ErrorCode.DEP_GRAPH_INCOMPLETE,
                "an artifact is not covered by all required dependency edges",
                field=first_target,
                details={
                    "missing_edges": [list(edge) for edge in missing],
                    "node_count": self.node_count,
                    "edge_count": self.edge_count,
                },
            )

        for edge in self.edges:
            source = self.nodes[edge.source]
            if source.node_type == "parameter":
                if edge.param_version != source.version:
                    raise SurfaceViewerError(
                        ErrorCode.DEP_GRAPH_INCOMPLETE,
                        "dependency edge carries a stale parameter version",
                        field=edge.target,
                        details={
                            "source": edge.source,
                            "expected_version": source.version,
                            "actual_version": edge.param_version,
                        },
                    )

        for node in self.artifact_nodes():
            incoming = self._incoming.get(node.node_id, ())
            if not incoming:
                raise SurfaceViewerError(
                    ErrorCode.DEP_GRAPH_INCOMPLETE,
                    "artifact has no incoming dependency edges",
                    field=node.node_id,
                    details={
                        "artifact_id": node.node_id,
                        "node_count": self.node_count,
                        "edge_count": self.edge_count,
                    },
                )

    def assert_publishable(self) -> None:
        self.validate_complete()

    def remove_edge(self, source: str, target: str) -> "DependencyGraph":
        return DependencyGraph(
            self.nodes.values(),
            (edge for edge in self.edges if edge.identity != (source, target)),
        )

    def diff(self, previous: "DependencyGraph | None") -> GraphDiff:
        if previous is None:
            return GraphDiff(
                added_nodes=tuple(sorted(self.nodes)),
                removed_nodes=(),
                added_edges=tuple(sorted(edge.identity for edge in self.edges)),
                removed_edges=(),
            )
        new_nodes = set(self.nodes)
        old_nodes = set(previous.nodes)
        new_edges = {edge.identity for edge in self.edges}
        old_edges = {edge.identity for edge in previous.edges}
        changed_nodes = tuple(
            (node_id, previous.nodes[node_id].version, self.nodes[node_id].version)
            for node_id in sorted(new_nodes & old_nodes)
            if previous.nodes[node_id].version != self.nodes[node_id].version
        )
        new_edge_map = {edge.identity: edge for edge in self.edges}
        old_edge_map = {edge.identity: edge for edge in previous.edges}
        changed_edges = tuple(
            (
                identity[0],
                identity[1],
                old_edge_map[identity].param_version,
                new_edge_map[identity].param_version,
            )
            for identity in sorted(new_edges & old_edges)
            if old_edge_map[identity].param_version
            != new_edge_map[identity].param_version
        )
        return GraphDiff(
            added_nodes=tuple(sorted(new_nodes - old_nodes)),
            removed_nodes=tuple(sorted(old_nodes - new_nodes)),
            added_edges=tuple(sorted(new_edges - old_edges)),
            removed_edges=tuple(sorted(old_edges - new_edges)),
            changed_nodes=changed_nodes,
            changed_edges=changed_edges,
        )

    def summary(self, previous: "DependencyGraph | None" = None) -> dict[str, object]:
        return {
            "node_count": self.node_count,
            "edge_count": self.edge_count,
            "diff": self.diff(previous).to_dict(),
        }

    def invalidate(
        self,
        changed_parameter_ids: Iterable[str],
        *,
        cached_signatures: Mapping[str, str] | None = None,
        structural_changed_patches: Iterable[str] = (),
    ) -> InvalidationReport:
        changed = tuple(sorted(set(changed_parameter_ids)))
        structural = set(structural_changed_patches)

        reasons: dict[str, list[str]] = {}
        closure: set[str] = set()
        for node_id in changed:
            closure.add(node_id)
            node = self.nodes.get(node_id)
            if node is None:
                continue
            self._propagate(
                node_id,
                (f"{node.patch_id}.{node.name} version changed",),
                reasons,
                closure,
            )

        artifact_nodes = self.artifact_nodes()
        for node in artifact_nodes:
            if node.patch_id in structural:
                reason = (
                    "patch structure changed; cannot prove artifact unaffected"
                )
                reasons.setdefault(node.node_id, []).append(reason)
                self._propagate(node.node_id, (reason,), reasons, closure)

        if cached_signatures is not None:
            for node in artifact_nodes:
                cached = cached_signatures.get(node.node_id)
                if cached is None:
                    reason = (
                        "artifact is absent from the cache; conservative recompute"
                    )
                    reasons.setdefault(node.node_id, []).append(reason)
                    self._propagate(
                        node.node_id,
                        (reason,),
                        reasons,
                        closure,
                    )
                elif cached != node.version:
                    reason = (
                        "cached parameter signature differs without a proven "
                        "dependency path; conservative recompute"
                    )
                    reasons.setdefault(node.node_id, []).append(reason)
                    self._propagate(
                        node.node_id,
                        (reason,),
                        reasons,
                        closure,
                    )

        affected: list[InvalidatedArtifact] = []
        reused: list[str] = []
        conservative: list[InvalidatedArtifact] = []
        artifact_by_id = {node.node_id: node for node in artifact_nodes}
        for artifact_id, artifact_reasons in sorted(reasons.items()):
            node = artifact_by_id[artifact_id]
            deduplicated = tuple(dict.fromkeys(artifact_reasons))
            item = InvalidatedArtifact(
                artifact_id=artifact_id,
                patch_id=node.patch_id,
                kind=node.name,
                reasons=deduplicated,
            )
            affected.append(item)
            if any("conservative" in reason for reason in deduplicated):
                conservative.append(item)

        affected_ids = {item.artifact_id for item in affected}
        reused = sorted(
            node.node_id
            for node in artifact_nodes
            if node.node_id not in affected_ids
        )
        return InvalidationReport(
            changed_parameters=changed,
            dependency_closure=tuple(sorted(closure)),
            affected=tuple(affected),
            reused=tuple(reused),
            conservative=tuple(conservative),
        )

    def _propagate(
        self,
        source_id: str,
        seed_reasons: tuple[str, ...],
        reasons: dict[str, list[str]],
        closure: set[str],
    ) -> None:
        queue: deque[tuple[str, tuple[str, ...]]] = deque([(source_id, seed_reasons)])
        visited: set[str] = set()
        while queue:
            source, path_reasons = queue.popleft()
            if source in visited:
                continue
            visited.add(source)
            closure.add(source)
            for edge in self._outgoing.get(source, ()):
                target = self.nodes.get(edge.target)
                if target is None:
                    continue
                if target.node_type == "artifact":
                    reason = _artifact_reason(source, path_reasons)
                    reasons.setdefault(edge.target, []).append(reason)
                if edge.target not in visited:
                    queue.append((edge.target, path_reasons))


def _artifact_reason(source: str, prior: tuple[str, ...]) -> str:
    if source.startswith("param:"):
        return prior[0]
    suffix = ""
    if any("conservative" in reason for reason in prior):
        suffix = "; conservative cascade"
    return "dependency closure through " + source + suffix


def parameter_signature_for_patch(patch: SurfacePatch, kind: str) -> str:
    versions = patch.parameter_versions
    return sha256_json(
        {
            "artifact_kind": kind,
            "patch_id": patch.patch_id,
            "dependencies": {
                name: versions[name]
                for name in REQUIRED_PARAMETER_DEPENDENCIES[kind]
            },
        }
    )


def changed_parameter_node_ids(
    old_assembly: SurfaceAssembly, new_assembly: SurfaceAssembly
) -> tuple[str, ...]:
    old_by_id = old_assembly.by_id
    new_by_id = new_assembly.by_id
    changed: list[str] = []
    for patch_id in sorted(set(old_by_id) & set(new_by_id)):
        old_patch = old_by_id[patch_id]
        new_patch = new_by_id[patch_id]
        for name in changed_parameter_names(old_patch, new_patch):
            changed.append(parameter_node_id(patch_id, name))
    return tuple(sorted(changed))


def structurally_changed_patches(
    old_assembly: SurfaceAssembly, new_assembly: SurfaceAssembly
) -> tuple[str, ...]:
    old_by_id = old_assembly.by_id
    new_by_id = new_assembly.by_id
    changed = set(old_by_id) ^ set(new_by_id)
    for patch_id in set(old_by_id) & set(new_by_id):
        if old_by_id[patch_id].block_id != new_by_id[patch_id].block_id:
            changed.add(patch_id)
    return tuple(sorted(changed))
