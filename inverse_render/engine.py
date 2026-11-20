"""Dependency-aware incremental forward/backward execution."""

from __future__ import annotations

from collections import OrderedDict
from dataclasses import dataclass
from typing import Any, Mapping

import numpy as np
import torch

from .errors import ErrorCode, ToolError
from .graph import COMPUTE_NODES, DependencyGraph, GraphDiff, build_dependency_graph
from .hashing import combine_hashes, sha256_json, tensor_map_hash
from .params import GroupSpec, ParameterStore
from .rendering import (
    TensorMap,
    compute_intersections,
    compute_lighting,
    compute_loss,
    compute_shadow_factor,
    compute_visibility,
    full_render,
)
from .scene import Scene


@dataclass
class CacheEntry:
    node: str
    values: TensorMap
    value_hash: str
    upstream_versions: OrderedDict[str, int]
    parameter_hashes: OrderedDict[str, OrderedDict[str, str]]
    graph_version: str

    @property
    def signature(self) -> OrderedDict[str, Any]:
        return OrderedDict(
            (
                ("versions", OrderedDict(self.upstream_versions)),
                ("parameter_hashes", OrderedDict(
                    (group, OrderedDict(paths))
                    for group, paths in self.parameter_hashes.items()
                )),
            )
        )


@dataclass(frozen=True)
class EvaluationResult:
    nodes: OrderedDict[str, TensorMap]
    loss: torch.Tensor
    changed_groups: tuple[str, ...]
    affected_nodes: tuple[str, ...]
    reused_nodes: tuple[str, ...]
    recomputed_nodes: tuple[str, ...]
    cache_hashes: OrderedDict[str, str]
    content_hash: str
    graph_version: str
    degraded: bool = False
    degraded_reason: str | None = None
    stale_nodes: tuple[str, ...] = ()


@dataclass(frozen=True)
class GradientResult:
    loss: float
    gradients: OrderedDict[str, OrderedDict[str, torch.Tensor]]
    gradient_norm: float
    evaluation: EvaluationResult


@dataclass(frozen=True)
class ReuseComparison:
    loss_incremental: float
    loss_full: float
    loss_abs_error: float
    max_group_grad_error: float
    group_grad_errors: OrderedDict[str, float]
    matched: bool
    first_mismatch: Mapping[str, Any] | None

    def to_ordered(self) -> OrderedDict[str, Any]:
        return OrderedDict(
            (
                ("matched", self.matched),
                ("loss_incremental", self.loss_incremental),
                ("loss_full", self.loss_full),
                ("loss_abs_error", self.loss_abs_error),
                ("max_group_grad_error", self.max_group_grad_error),
                (
                    "group_grad_errors",
                    OrderedDict(self.group_grad_errors),
                ),
                (
                    "first_mismatch",
                    None
                    if self.first_mismatch is None
                    else OrderedDict(self.first_mismatch),
                ),
            )
        )


class IncrementalEngine:
    """Stateful renderer whose cache entries are guarded by two signatures."""

    def __init__(
        self,
        scene: Scene,
        store: ParameterStore,
        graph: DependencyGraph | None = None,
    ) -> None:
        self.scene = scene
        self.store = store
        self.graph = graph or build_dependency_graph(scene, list(store.groups.values()))
        if self.graph.structure_hash != scene.structure_hash():
            raise ToolError(
                ErrorCode.E_GRAPH_INVALID,
                "graph structure hash does not match scene",
                details={"scene_hash": scene.structure_hash()},
            )
        self.cache: OrderedDict[str, CacheEntry] = OrderedDict()
        self._last_versions = store.versions()
        self._graph_version = self.graph.version
        self._last_evaluation: EvaluationResult | None = None

    def _signature_parts(
        self, node: str, signatures: Mapping[str, Mapping[str, Any]]
    ) -> tuple[OrderedDict[str, int], OrderedDict[str, OrderedDict[str, str]]]:
        group_ids = self.graph.upstream_groups(node)
        versions: OrderedDict[str, int] = OrderedDict()
        hashes: OrderedDict[str, OrderedDict[str, str]] = OrderedDict()
        for group_id in group_ids:
            versions[group_id] = int(signatures[group_id]["version"])
            hashes[group_id] = OrderedDict(signatures[group_id]["parameter_hashes"])
        return versions, hashes

    def _entry_for(
        self,
        node: str,
        values: TensorMap,
        signatures: Mapping[str, Mapping[str, Any]],
    ) -> CacheEntry:
        versions, hashes = self._signature_parts(node, signatures)
        return CacheEntry(
            node=node,
            values=values,
            value_hash=tensor_map_hash(values),
            upstream_versions=versions,
            parameter_hashes=hashes,
            graph_version=self._graph_version,
        )

    def _cache_still_valid(
        self,
        entry: CacheEntry,
        signatures: Mapping[str, Mapping[str, Any]],
    ) -> tuple[bool, str]:
        if entry.graph_version != self._graph_version:
            return False, "graph_version_changed"
        current_versions, current_hashes = self._signature_parts(entry.node, signatures)
        if current_versions != entry.upstream_versions:
            return False, "upstream_version_changed_without_invalidation"
        if current_hashes != entry.parameter_hashes:
            return False, "upstream_content_changed_without_invalidation"
        if tensor_map_hash(entry.values) != entry.value_hash:
            return False, "cached_content_hash_mismatch"
        return True, ""

    def evaluate(self, *, force_full: bool = False) -> EvaluationResult:
        current_versions = self.store.versions()
        signatures = self.store.signatures()
        changed_groups = tuple(
            group_id
            for group_id in self.store.group_order
            if self._last_versions.get(group_id) != current_versions[group_id]
        )
        impacted = self.graph.impacted_nodes(changed_groups) if changed_groups else ()
        full_recompute = force_full or not self.cache
        stale_nodes: list[str] = []
        degraded = False
        degraded_reason: str | None = None

        if self._graph_version != self.graph.version:
            self.cache.clear()
            full_recompute = True
            degraded = True
            degraded_reason = "graph_version_changed"

        if not full_recompute:
            affected = set(impacted)
            for node in COMPUTE_NODES:
                if node in affected:
                    continue
                entry = self.cache.get(node)
                if entry is None:
                    stale_nodes.append(node)
                    degraded = True
                    degraded_reason = degraded_reason or "cache_missing"
                    full_recompute = True
                    continue
                valid, reason = self._cache_still_valid(entry, signatures)
                if not valid:
                    stale_nodes.append(node)
                    degraded = True
                    degraded_reason = degraded_reason or reason
                    full_recompute = True

        recomputed: list[str] = []
        reused: list[str] = []
        if full_recompute:
            rendered_nodes, loss = full_render(self.store, self.scene)
            self.cache.clear()
            for node in COMPUTE_NODES:
                self.cache[node] = self._entry_for(
                    node, rendered_nodes[node], signatures
                )
            recomputed = list(COMPUTE_NODES)
            affected_nodes = tuple(COMPUTE_NODES)
        else:
            rendered_nodes = OrderedDict()
            affected = set(impacted)
            for node in COMPUTE_NODES:
                if node in affected:
                    if node == "intersections":
                        values = compute_intersections(self.store, self.scene)
                    elif node == "visibility":
                        values = compute_visibility(
                            self.scene, rendered_nodes["intersections"]
                        )
                    elif node == "shadow_factor":
                        values = compute_shadow_factor(
                            self.store,
                            self.scene,
                            rendered_nodes["intersections"],
                            rendered_nodes["visibility"],
                        )
                    elif node == "lighting":
                        values = compute_lighting(
                            self.store,
                            self.scene,
                            rendered_nodes["intersections"],
                            rendered_nodes["visibility"],
                            rendered_nodes["shadow_factor"],
                        )
                    elif node == "loss":
                        values = compute_loss(self.scene, rendered_nodes["lighting"])
                    else:
                        raise RuntimeError(f"unknown compute node {node}")
                    rendered_nodes[node] = values
                    self.cache[node] = self._entry_for(node, values, signatures)
                    recomputed.append(node)
                else:
                    entry = self.cache[node]
                    rendered_nodes[node] = entry.values
                    reused.append(node)
            loss = rendered_nodes["loss"]["loss"]
            affected_nodes = tuple(impacted)

        cache_hashes = OrderedDict(
            (node, self.cache[node].value_hash) for node in COMPUTE_NODES
        )
        content_hash = combine_hashes(cache_hashes.values())
        result = EvaluationResult(
            nodes=rendered_nodes,
            loss=loss,
            changed_groups=changed_groups,
            affected_nodes=affected_nodes,
            reused_nodes=tuple(reused),
            recomputed_nodes=tuple(recomputed),
            cache_hashes=cache_hashes,
            content_hash=content_hash,
            graph_version=self._graph_version,
            degraded=degraded,
            degraded_reason=degraded_reason,
            stale_nodes=tuple(stale_nodes),
        )
        self._last_versions = current_versions
        self._last_evaluation = result
        return result

    def backward(
        self,
        evaluation: EvaluationResult | None = None,
    ) -> GradientResult:
        evaluation = evaluation or self._last_evaluation
        if evaluation is None:
            raise ToolError(
                ErrorCode.E_CONFIG_INVALID,
                "evaluate must be called before backward",
            )
        self.store.zero_grad()
        evaluation.loss.backward(retain_graph=True)
        return GradientResult(
            loss=float(evaluation.loss.detach().cpu().item()),
            gradients=self.store.group_gradients(),
            gradient_norm=self.store.gradient_norm(),
            evaluation=evaluation,
        )

    def invalidate(self) -> None:
        self.cache.clear()
        self._last_evaluation = None
        self._last_versions = self.store.versions()

    def reconfigure(
        self,
        scene: Scene,
        group_specs: list[GroupSpec],
        store: ParameterStore,
    ) -> GraphDiff:
        new_graph = build_dependency_graph(scene, group_specs)
        diff = new_graph.diff(self.graph)
        self.scene = scene
        self.graph = new_graph
        self.store = store
        self._graph_version = new_graph.version
        self._last_versions = store.versions()
        self.invalidate()
        return diff

    def compare_with_full(
        self,
        gradient_result: GradientResult,
        *,
        loss_atol: float = 1e-8,
        grad_atol: float = 1e-7,
    ) -> ReuseComparison:
        fresh_store = ParameterStore.clone_fresh(self.store)
        _, full_loss_tensor = full_render(fresh_store, self.scene)
        fresh_store.zero_grad()
        full_loss_tensor.backward()
        full_gradients = fresh_store.group_gradients()

        loss_incremental = float(gradient_result.loss)
        loss_full = float(full_loss_tensor.detach().cpu().item())
        loss_error = abs(loss_incremental - loss_full)
        group_errors: OrderedDict[str, float] = OrderedDict()
        first_mismatch: dict[str, Any] | None = None
        if loss_error > loss_atol:
            first_mismatch = {
                "node": "loss",
                "group": "loss",
                "max_abs_error": loss_error,
                "tolerance": loss_atol,
            }
        for group_id in self.store.group_order:
            max_error = 0.0
            for path in self.store.parameter_order[group_id]:
                incremental = gradient_result.gradients[group_id][path].cpu().numpy()
                full = full_gradients[group_id][path].cpu().numpy()
                error = float(np.max(np.abs(incremental - full))) if incremental.size else 0.0
                max_error = max(max_error, error)
                if error > grad_atol and first_mismatch is None:
                    first_mismatch = {
                        "node": self._node_for_group_path(group_id, path),
                        "group": group_id,
                        "parameter": path,
                        "max_abs_error": error,
                        "tolerance": grad_atol,
                    }
            group_errors[group_id] = max_error
        max_group_error = max(group_errors.values(), default=0.0)
        matched = (
            loss_error <= loss_atol
            and max_group_error <= grad_atol
            and first_mismatch is None
        )
        return ReuseComparison(
            loss_incremental=loss_incremental,
            loss_full=loss_full,
            loss_abs_error=loss_error,
            max_group_grad_error=max_group_error,
            group_grad_errors=group_errors,
            matched=matched,
            first_mismatch=first_mismatch,
        )

    def _node_for_group_path(self, group_id: str, path: str) -> str:
        kind = self.store.groups[group_id].kind
        if kind == "material":
            return "lighting"
        if kind == "light":
            return "shadow_factor"
        if kind in ("camera", "geometry"):
            return "intersections"
        return "loss"

    def assert_full_equivalence(
        self,
        gradient_result: GradientResult,
        *,
        loss_atol: float = 1e-8,
        grad_atol: float = 1e-7,
    ) -> ReuseComparison:
        comparison = self.compare_with_full(
            gradient_result,
            loss_atol=loss_atol,
            grad_atol=grad_atol,
        )
        if not comparison.matched:
            first = comparison.first_mismatch or {}
            raise ToolError(
                ErrorCode.E_REUSE_MISMATCH,
                "incremental reuse differs from full recomputation",
                details={
                    "node": first.get("node", "loss"),
                    "group": first.get("group", "loss"),
                    "max_abs_error": first.get("max_abs_error"),
                    "tolerance": first.get("tolerance"),
                    "comparison": comparison.to_ordered(),
                },
            )
        return comparison

