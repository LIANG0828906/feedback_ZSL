"""Dependency graph, content-addressed caches, and incremental recomputation."""

from __future__ import annotations

from dataclasses import dataclass, field, replace
from typing import Any, Iterable

from .errors import ALGORITHM_VERSION, ParameterError, SampleValidationError
from .hashing import content_hash
from .models import (
    AxisFrame,
    BindingTable,
    DeviceSpec,
    SampleBundle,
)
from .replay import (
    ReplayResult,
    RuleTimeline,
    assemble_result,
    compute_axis_frames,
    replay_rule,
)


@dataclass(frozen=True)
class NodeDef:
    node_id: str
    kind: str
    deps: tuple[str, ...]
    params: dict[str, Any]
    label: str
    param_fingerprint: str = field(init=False)

    def __post_init__(self) -> None:
        object.__setattr__(self, "param_fingerprint", content_hash(self.params))

    def to_dict(self) -> dict[str, Any]:
        return {
            "node_id": self.node_id,
            "kind": self.kind,
            "deps": list(self.deps),
            "params": self.params,
            "label": self.label,
            "param_fingerprint": self.param_fingerprint,
        }


@dataclass(frozen=True)
class CacheEntry:
    cache_key: str
    node_id: str
    kind: str
    param_fingerprint: str
    upstream_keys: tuple[str, ...]
    value: Any
    window_ids: tuple[str, ...] = ()
    generation: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "cache_key": self.cache_key,
            "node_id": self.node_id,
            "kind": self.kind,
            "param_fingerprint": self.param_fingerprint,
            "upstream_keys": list(self.upstream_keys),
            "value": encode_cache_value(self.kind, self.value),
            "window_ids": list(self.window_ids),
            "generation": self.generation,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "CacheEntry":
        return cls(
            cache_key=str(data["cache_key"]),
            node_id=str(data["node_id"]),
            kind=str(data["kind"]),
            param_fingerprint=str(data["param_fingerprint"]),
            upstream_keys=tuple(str(item) for item in data.get("upstream_keys", [])),
            value=decode_cache_value(str(data["kind"]), data.get("value")),
            window_ids=tuple(str(item) for item in data.get("window_ids", [])),
            generation=int(data.get("generation", 0)),
        )


@dataclass(frozen=True)
class InvalidationEntry:
    node_id: str
    kind: str
    label: str
    invalidated: bool
    reused: bool
    recomputed: bool
    reasons: tuple[str, ...]
    window_ids: tuple[str, ...] = ()
    cache_key: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "node_id": self.node_id,
            "kind": self.kind,
            "label": self.label,
            "invalidated": self.invalidated,
            "reused": self.reused,
            "recomputed": self.recomputed,
            "reasons": list(self.reasons),
            "window_ids": list(self.window_ids),
            "cache_key": self.cache_key,
        }


@dataclass(frozen=True)
class PipelineRun:
    result: ReplayResult
    invalidations: tuple[InvalidationEntry, ...]
    generation: int
    graph_signature: str
    param_version: str
    reused_node_ids: tuple[str, ...]
    recomputed_node_ids: tuple[str, ...]

    @property
    def reused_count(self) -> int:
        return len(self.reused_node_ids)

    @property
    def recomputed_count(self) -> int:
        return len(self.recomputed_node_ids)

    def to_dict(self) -> dict[str, Any]:
        return {
            "result": self.result.to_dict(),
            "invalidations": [entry.to_dict() for entry in self.invalidations],
            "generation": self.generation,
            "graph_signature": self.graph_signature,
            "param_version": self.param_version,
            "reused_node_ids": list(self.reused_node_ids),
            "recomputed_node_ids": list(self.recomputed_node_ids),
            "reused_count": self.reused_count,
            "recomputed_count": self.recomputed_count,
        }


def encode_cache_value(kind: str, value: Any) -> Any:
    if kind == "axis":
        return [frame.to_dict() for frame in value]
    if kind in {"rule", "action"}:
        return value.to_dict()
    if kind == "output":
        return value.to_dict()
    if kind in {"device", "timing"}:
        return value
    raise TypeError(f"cannot encode cache value for node kind {kind!r}")


def decode_cache_value(kind: str, value: Any) -> Any:
    if kind == "axis":
        return tuple(AxisFrame.from_dict(item) for item in value)
    if kind in {"rule", "action"}:
        return RuleTimeline.from_dict(value)
    if kind == "output":
        return ReplayResult.from_dict(value)
    if kind in {"device", "timing"}:
        return value
    raise TypeError(f"cannot decode cache value for node kind {kind!r}")


def _diff_paths(old: Any, new: Any, prefix: str = "") -> list[str]:
    if type(old) is not type(new):
        return [prefix or "value"]
    if isinstance(old, dict):
        paths: list[str] = []
        for key in sorted(set(old) | set(new), key=str):
            child = f"{prefix}.{key}" if prefix else str(key)
            if key not in old or key not in new:
                paths.append(child)
            else:
                paths.extend(_diff_paths(old[key], new[key], child))
        return paths
    if isinstance(old, list):
        paths = []
        if len(old) != len(new):
            paths.append(f"{prefix}.length")
        for index, (left, right) in enumerate(zip(old, new)):
            paths.extend(_diff_paths(left, right, f"{prefix}[{index}]"))
        return paths
    if old != new:
        return [prefix or "value"]
    return []


def _topological_order(nodes: dict[str, NodeDef]) -> tuple[str, ...]:
    order: list[str] = []
    temporary: set[str] = set()
    permanent: set[str] = set()

    def visit(node_id: str) -> None:
        if node_id in permanent:
            return
        if node_id in temporary:
            raise ValueError(f"dependency cycle detected at {node_id!r}")
        temporary.add(node_id)
        for dependency in nodes[node_id].deps:
            if dependency not in nodes:
                raise ValueError(
                    f"node {node_id!r} references missing dependency {dependency!r}"
                )
            visit(dependency)
        temporary.remove(node_id)
        permanent.add(node_id)
        order.append(node_id)

    for node_id in nodes:
        visit(node_id)
    return tuple(order)


class Pipeline:
    """Stateful incremental mapping pipeline.

    Event windows are input-sequence partitions. Axis and rule nodes only
    process windows containing events relevant to that node; output assembly
    recombines cached timelines without replaying mapping rules.
    """

    def __init__(
        self,
        bundle: SampleBundle,
        *,
        max_event_buffer: int = 256,
        window_size: int = 32,
    ) -> None:
        if max_event_buffer <= 0:
            raise ParameterError(
                "max_event_buffer must be positive", field="max_event_buffer"
            )
        if window_size <= 0:
            raise ParameterError("window_size must be positive", field="window_size")
        self.bundle = bundle
        self.max_event_buffer = max_event_buffer
        self.window_size = window_size
        self.nodes, self.order = self._build_nodes(bundle.device, bundle.bindings)
        self.caches: dict[str, CacheEntry] = {}
        self.generation = 0
        self._previous_nodes: dict[str, NodeDef] = {}
        self.graph_signature = self._graph_signature()
        self.param_version = self._param_version()

    @property
    def device(self) -> DeviceSpec:
        return self.bundle.device

    @property
    def bindings(self) -> BindingTable:
        return self.bundle.bindings

    @property
    def input_hash(self) -> str:
        return self.bundle.input_hash

    @property
    def binding_version(self) -> str:
        return self.bundle.binding_version

    def _build_nodes(
        self, device: DeviceSpec, bindings: BindingTable
    ) -> tuple[dict[str, NodeDef], tuple[str, ...]]:
        nodes: dict[str, NodeDef] = {}
        device_node_id = f"device:{device.id}"
        nodes[device_node_id] = NodeDef(
            node_id=device_node_id,
            kind="device",
            deps=(),
            params=device.structural_dict(),
            label=f"Device layout · {device.name}",
        )
        for axis in device.axes:
            node_id = f"axis:{axis.id}"
            nodes[node_id] = NodeDef(
                node_id=node_id,
                kind="axis",
                deps=(device_node_id,),
                params=axis.to_dict(),
                label=f"Axis · {axis.id}",
            )
        for rule in bindings.rules:
            timing_id = f"timing:{rule.id}"
            nodes[timing_id] = NodeDef(
                node_id=timing_id,
                kind="timing",
                deps=(),
                params={
                    "window_ms": rule.window_ms,
                    "hold_ms": rule.hold_ms,
                    "kind": rule.kind,
                },
                label=f"Timing · {rule.id}",
            )
            deps = [device_node_id, timing_id]
            if rule.kind == "axis" and rule.source_axis:
                deps.append(f"axis:{rule.source_axis}")
            rule_node_id = f"rule:{rule.id}"
            nodes[rule_node_id] = NodeDef(
                node_id=rule_node_id,
                kind="rule",
                deps=tuple(sorted(deps)),
                params={
                    "id": rule.id,
                    "action": rule.action,
                    "kind": rule.kind,
                    "buttons": list(rule.buttons),
                    "source_axis": rule.source_axis,
                    "curve": rule.curve.to_dict() if rule.curve else None,
                    "enabled": rule.enabled,
                },
                label=f"Rule · {rule.id} → {rule.action}",
            )
            action_node_id = f"action:{rule.action}"
            nodes[action_node_id] = NodeDef(
                node_id=action_node_id,
                kind="action",
                deps=(rule_node_id,),
                params={"action": rule.action, "rule_id": rule.id},
                label=f"Action · {rule.action}",
            )
        output_deps = sorted(
            [node_id for node_id in nodes if node_id.startswith("action:")]
            + [node_id for node_id in nodes if node_id.startswith("axis:")]
        )
        nodes["output"] = NodeDef(
            node_id="output",
            kind="output",
            deps=tuple(output_deps),
            params={
                "window_size": self.window_size,
                "axis_ids": list(device.axis_ids),
                "action_ids": [rule.action for rule in bindings.rules],
            },
            label="Output · assembled action stream",
        )
        return nodes, _topological_order(nodes)

    def _graph_signature(self) -> str:
        return content_hash(
            {
                "nodes": [
                    {
                        "node_id": node.node_id,
                        "kind": node.kind,
                        "deps": list(node.deps),
                        "param_fingerprint": node.param_fingerprint,
                    }
                    for node in (self.nodes[node_id] for node_id in self.order)
                ]
            }
        )

    def _param_version(self) -> str:
        return content_hash(
            {
                node_id: {
                    "kind": self.nodes[node_id].kind,
                    "params": self.nodes[node_id].params,
                }
                for node_id in self.order
            }
        )

    def _cache_key(self, node: NodeDef, upstream_keys: Iterable[str]) -> str:
        return content_hash(
            {
                "algorithm_version": ALGORITHM_VERSION,
                "input_hash": self.bundle.input_hash,
                "node_id": node.node_id,
                "param_fingerprint": node.param_fingerprint,
                "upstream_keys": sorted(upstream_keys),
            }
        )

    def cache_key_for(
        self,
        node_id: str,
        upstream_keys: Iterable[str] | None = None,
    ) -> str:
        node = self.nodes[node_id]
        if upstream_keys is None:
            upstream_keys = tuple(
                self.caches[dependency].cache_key for dependency in node.deps
            )
        return self._cache_key(node, upstream_keys)

    def _window_ids_for_sequences(self, sequences: Iterable[int]) -> tuple[str, ...]:
        ids = sorted({seq // self.window_size for seq in sequences})
        return tuple(f"w{index}" for index in ids)

    def _axis_frame_map(
        self, cache_values: dict[str, Any]
    ) -> dict[int, AxisFrame]:
        frames: dict[int, AxisFrame] = {}
        for axis in self.device.axes:
            node_id = f"axis:{axis.id}"
            for frame in cache_values.get(node_id, ()):
                frames[frame.seq] = frame
        return frames

    def _compute_value(
        self,
        node: NodeDef,
        cache_values: dict[str, Any],
    ) -> tuple[Any, tuple[str, ...]]:
        if node.kind == "device":
            return self.device.structural_dict(), ()
        if node.kind == "axis":
            axis_id = node.node_id.split(":", 1)[1]
            axis = self.device.axis_map[axis_id]
            events = self.bundle.axis_events(axis_id)
            frames = compute_axis_frames(
                axis,
                events,
                max_event_buffer=self.max_event_buffer,
            )
            windows = self._window_ids_for_sequences(event.seq for event in events)
            return frames, windows
        if node.kind == "timing":
            return dict(node.params), ()
        if node.kind == "rule":
            rule_id = node.node_id.split(":", 1)[1]
            rule = self.bindings.rule_map()[rule_id]
            axis = self.device.axis_map.get(rule.source_axis or "")
            axis_frames = self._axis_frame_map(cache_values)
            ranks = {
                item.id: rank
                for rank, item in enumerate(
                    sorted(self.bindings.rules, key=lambda value: value.id)
                )
            }
            timeline = replay_rule(
                rule,
                axis,
                self.bundle.events,
                axis_frames,
                rule_rank=ranks[rule.id],
                max_event_buffer=self.max_event_buffer,
                window_size=self.window_size,
            )
            return timeline, timeline.window_ids
        if node.kind == "action":
            rule_node_id = node.deps[0]
            timeline = cache_values[rule_node_id]
            return timeline, timeline.window_ids
        if node.kind == "output":
            axis_frames = {
                axis.id: tuple(cache_values[f"axis:{axis.id}"])
                for axis in self.device.axes
            }
            timelines = {
                node_id.split(":", 1)[1]: cache_values[node_id]
                for node_id in self.order
                if node_id.startswith("action:")
            }
            result = assemble_result(
                self.bundle,
                axis_frames,
                timelines,
                window_size=self.window_size,
            )
            return result, ()
        raise ValueError(f"unknown node kind {node.kind!r}")

    def _initial_reasons(self, node: NodeDef) -> list[str]:
        old = self._previous_nodes.get(node.node_id)
        cache = self.caches.get(node.node_id)
        reasons: list[str] = []
        if cache is None:
            reasons.append("cache_missing")
            return reasons
        if old is None:
            reasons.append("node_added")
        else:
            if old.kind != node.kind:
                reasons.append(f"node_kind_changed:{old.kind}->{node.kind}")
            if old.deps != node.deps:
                reasons.append("dependency_graph_changed")
            if old.param_fingerprint != node.param_fingerprint:
                changed = _diff_paths(old.params, node.params)
                if changed:
                    reasons.extend(f"parameter_changed:{path}" for path in changed)
                else:
                    reasons.append("parameter_changed")
        if cache.param_fingerprint != node.param_fingerprint:
            if not any(reason.startswith("parameter_changed") for reason in reasons):
                reasons.append("parameter_fingerprint_changed")
        return reasons

    def update(
        self,
        *,
        device: DeviceSpec | None = None,
        bindings: BindingTable | None = None,
        force_node_ids: Iterable[str] = (),
    ) -> PipelineRun:
        if device is None and bindings is None and not tuple(force_node_ids):
            return self.recompute(force_node_ids=force_node_ids)
        new_device = device or self.device
        new_bindings = bindings or self.bindings
        self._validate_mapping(new_device, new_bindings)
        self._previous_nodes = self.nodes
        self.bundle = replace(
            self.bundle,
            device=new_device,
            bindings=new_bindings,
        )
        self.nodes, self.order = self._build_nodes(new_device, new_bindings)
        self.graph_signature = self._graph_signature()
        self.param_version = self._param_version()
        return self.recompute(force_node_ids=force_node_ids)

    def recompute(self, *, force_node_ids: Iterable[str] = ()) -> PipelineRun:
        forced = set(force_node_ids)
        unknown = forced - set(self.nodes)
        if unknown:
            raise ParameterError(
                f"unknown node id(s): {', '.join(sorted(unknown))}",
                field="node_id",
                context={"node_ids": sorted(unknown)},
            )
        self.generation += 1
        dirty: dict[str, list[str]] = {}
        for node_id in self.order:
            node = self.nodes[node_id]
            reasons = self._initial_reasons(node)
            if node_id in forced:
                reasons.append("manual_retry")
            dirty[node_id] = reasons
        # Propagate upstream invalidation in topological order.
        for node_id in self.order:
            node = self.nodes[node_id]
            for dependency in node.deps:
                if dirty.get(dependency):
                    reason = f"upstream_invalidated:{dependency}"
                    if reason not in dirty[node_id]:
                        dirty[node_id].append(reason)
            cache = self.caches.get(node_id)
            if cache is not None and not dirty[node_id]:
                upstream_keys = tuple(
                    self.caches[dependency].cache_key
                    for dependency in node.deps
                    if dependency in self.caches
                )
                if len(upstream_keys) != len(node.deps):
                    dirty[node_id].append("missing_upstream_cache")
                else:
                    expected_key = self._cache_key(node, upstream_keys)
                    if expected_key != cache.cache_key:
                        dirty[node_id].append("cache_key_mismatch")

        if not any(dirty.values()) and "output" in self.caches:
            result = self.caches["output"].value
            invalidations = tuple(
                InvalidationEntry(
                    node_id=node_id,
                    kind=self.nodes[node_id].kind,
                    label=self.nodes[node_id].label,
                    invalidated=False,
                    reused=True,
                    recomputed=False,
                    reasons=("upstream_unchanged",),
                    window_ids=self.caches[node_id].window_ids,
                    cache_key=self.caches[node_id].cache_key,
                )
                for node_id in self.order
            )
            self._previous_nodes = self.nodes
            return PipelineRun(
                result=result,
                invalidations=invalidations,
                generation=self.generation,
                graph_signature=self.graph_signature,
                param_version=self.param_version,
                reused_node_ids=tuple(self.order),
                recomputed_node_ids=(),
            )

        new_caches = dict(self.caches)
        cache_values: dict[str, Any] = {
            node_id: entry.value for node_id, entry in self.caches.items()
        }
        reused: list[str] = []
        recomputed: list[str] = []
        for node_id in self.order:
            node = self.nodes[node_id]
            reasons = dirty[node_id]
            if not reasons and node_id in self.caches:
                reused.append(node_id)
                continue
            value, window_ids = self._compute_value(node, cache_values)
            upstream_keys = tuple(
                new_caches[dependency].cache_key for dependency in node.deps
            )
            cache_key = self._cache_key(node, upstream_keys)
            entry = CacheEntry(
                cache_key=cache_key,
                node_id=node_id,
                kind=node.kind,
                param_fingerprint=node.param_fingerprint,
                upstream_keys=upstream_keys,
                value=value,
                window_ids=window_ids,
                generation=self.generation,
            )
            new_caches[node_id] = entry
            cache_values[node_id] = value
            recomputed.append(node_id)

        # Atomic replacement only after every dirty node has succeeded.
        for removed_node_id in set(new_caches) - set(self.nodes):
            del new_caches[removed_node_id]
        self.caches = new_caches
        result = self.caches["output"].value
        invalidations: list[InvalidationEntry] = []
        for node_id in self.order:
            node = self.nodes[node_id]
            was_dirty = bool(dirty[node_id])
            entry = self.caches[node_id]
            invalidations.append(
                InvalidationEntry(
                    node_id=node_id,
                    kind=node.kind,
                    label=node.label,
                    invalidated=was_dirty,
                    reused=not was_dirty,
                    recomputed=was_dirty,
                    reasons=tuple(dirty[node_id]) if was_dirty else ("upstream_unchanged",),
                    window_ids=entry.window_ids,
                    cache_key=entry.cache_key,
                )
            )
        for old_node_id in sorted(set(self._previous_nodes) - set(self.nodes)):
            old_node = self._previous_nodes[old_node_id]
            invalidations.append(
                InvalidationEntry(
                    node_id=old_node_id,
                    kind=old_node.kind,
                    label=old_node.label,
                    invalidated=True,
                    reused=False,
                    recomputed=False,
                    reasons=("node_removed",),
                )
            )
        self._previous_nodes = self.nodes
        return PipelineRun(
            result=result,
            invalidations=tuple(invalidations),
            generation=self.generation,
            graph_signature=self.graph_signature,
            param_version=self.param_version,
            reused_node_ids=tuple(reused),
            recomputed_node_ids=tuple(recomputed),
        )

    def _validate_mapping(self, device: DeviceSpec, bindings: BindingTable) -> None:
        from .validation import validate_mapping

        issues = validate_mapping(device, bindings)
        if issues:
            raise SampleValidationError(issues)

    def install_cache_entries(self, entries: dict[str, CacheEntry]) -> None:
        new_caches: dict[str, CacheEntry] = {}
        for node_id, entry in entries.items():
            node = self.nodes.get(node_id)
            if node is None:
                raise ParameterError(
                    f"snapshot contains node not present in current graph: {node_id}",
                    field="node_id",
                )
            if entry.kind != node.kind:
                raise ParameterError(
                    f"snapshot node kind mismatch for {node_id}",
                    field="node_kind",
                )
            if entry.param_fingerprint != node.param_fingerprint:
                raise ParameterError(
                    f"snapshot parameter fingerprint mismatch for {node_id}",
                    field="param_fingerprint",
                )
            new_caches[node_id] = entry
        self.caches = new_caches
        self._previous_nodes = self.nodes

    def validate_cache_entries(self, entries: dict[str, CacheEntry]) -> list[str]:
        errors: list[str] = []
        extra = sorted(set(entries) - set(self.nodes))
        if extra:
            errors.append(f"extra snapshot nodes: {', '.join(extra)}")
        chain_keys: dict[str, str] = {}
        for node_id in self.order:
            entry = entries.get(node_id)
            if entry is None:
                errors.append(f"missing snapshot node: {node_id}")
                continue
            node = self.nodes[node_id]
            if entry.kind != node.kind:
                errors.append(f"kind mismatch: {node_id}")
                continue
            if entry.param_fingerprint != node.param_fingerprint:
                errors.append(f"parameter fingerprint mismatch: {node_id}")
                continue
            upstream_keys = tuple(chain_keys[dependency] for dependency in node.deps)
            if entry.upstream_keys != upstream_keys:
                errors.append(f"upstream key chain mismatch: {node_id}")
                continue
            expected = self.cache_key_for(node_id, upstream_keys)
            if entry.cache_key != expected:
                errors.append(f"cache key mismatch: {node_id}")
                continue
            chain_keys[node_id] = entry.cache_key
        return errors

    def cache_manifest(self) -> dict[str, Any]:
        return {
            "entries": {
                node_id: self.caches[node_id].to_dict() for node_id in self.order
            },
            "node_count": len(self.caches),
            "graph_signature": self.graph_signature,
            "param_version": self.param_version,
            "input_hash": self.bundle.input_hash,
            "binding_version": self.bundle.binding_version,
            "algorithm_version": ALGORITHM_VERSION,
        }

    def cache_entry(self, node_id: str) -> CacheEntry | None:
        return self.caches.get(node_id)

    def clear_caches(self) -> None:
        self.caches = {}

    def describe_graph(self) -> dict[str, Any]:
        return {
            "order": list(self.order),
            "nodes": {
                node_id: self.nodes[node_id].to_dict() for node_id in self.order
            },
            "graph_signature": self.graph_signature,
            "param_version": self.param_version,
        }


__all__ = [
    "CacheEntry",
    "InvalidationEntry",
    "NodeDef",
    "Pipeline",
    "PipelineRun",
    "decode_cache_value",
    "encode_cache_value",
]
