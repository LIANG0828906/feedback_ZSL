"""Forward dependency graph and incremental cache with stale-cache denial."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from typing import Any, Iterable, Mapping

import numpy as np

from .params import GROUPS, ParameterSet
from .reduction import (
    GradientResult,
    ReductionTolerance,
    compute_gradients,
    reduce_chunk_results,
    results_equivalent,
)
from .renderer import (
    ChunkGradient,
    ForwardChunk,
    PixelChunk,
    chunk_gradient_hash,
    forward_chunk,
    generate_rays,
    intersect_scene,
    make_chunks,
    shadow_factors,
    shade_image,
)
from .scene import Scene


NODE_ORDER: tuple[str, ...] = (
    "camera_rays",
    "intersections",
    "visibility",
    "shadow_factor",
    "shading",
    "loss",
)

NODE_PARAM_GROUPS: dict[str, tuple[str, ...]] = {
    "camera_rays": ("camera",),
    "intersections": ("camera", "geometry"),
    "visibility": ("camera", "geometry"),
    "shadow_factor": ("camera", "geometry", "light"),
    "shading": ("camera", "geometry", "material", "light"),
    "loss": GROUPS,
}

NODE_PREDECESSORS: dict[str, tuple[str, ...]] = {
    "camera_rays": (),
    "intersections": ("camera_rays",),
    "visibility": ("intersections",),
    "shadow_factor": ("intersections", "visibility"),
    "shading": ("visibility", "shadow_factor"),
    "loss": ("shading",),
}


@dataclass
class StageCache:
    signature: tuple[tuple[str, int, str], ...]
    value: Any
    value_hash: str
    chunk_key: tuple[int, int, int, int]


class DependencyGraph:
    def __init__(self) -> None:
        self._children: dict[str, set[str]] = {
            **{f"parameter:{group}": set() for group in GROUPS},
            **{node: set() for node in NODE_ORDER},
        }
        explicit = {
            "parameter:camera": {"camera_rays"},
            "parameter:geometry": {"intersections"},
            "parameter:material": {"shading"},
            "parameter:light": {"shadow_factor", "shading"},
        }
        for parent, children in explicit.items():
            self._children[parent].update(children)
        for node, predecessors in NODE_PREDECESSORS.items():
            for predecessor in predecessors:
                self._children[predecessor].add(node)

    def affected_nodes(self, modified_groups: Iterable[str]) -> tuple[str, ...]:
        frontier = [f"parameter:{group}" for group in modified_groups]
        seen = set(frontier)
        while frontier:
            current = frontier.pop()
            for child in self._children.get(current, ()):
                if child not in seen:
                    seen.add(child)
                    frontier.append(child)
        return tuple(node for node in NODE_ORDER if node in seen)

    def parameter_edges(self) -> dict[str, tuple[str, ...]]:
        return {
            group: tuple(
                node
                for node in NODE_ORDER
                if group in NODE_PARAM_GROUPS[node]
            )
            for group in GROUPS
        }

    def to_dict(self) -> dict[str, object]:
        return {
            "nodes": list(NODE_ORDER),
            "parameter_edges": {
                group: list(nodes) for group, nodes in self.parameter_edges().items()
            },
            "predecessors": {
                node: list(predecessors)
                for node, predecessors in NODE_PREDECESSORS.items()
            },
        }


class IncrementalEvaluator:
    def __init__(self, scene: Scene) -> None:
        self.scene = scene
        self.graph = DependencyGraph()
        self._cache: dict[tuple[int, str], StageCache] = {}
        self._gradient_cache: dict[
            int, tuple[tuple[tuple[str, int, str], ...], float, dict[str, np.ndarray], str]
        ] = {}

    def clear(self) -> None:
        self._cache.clear()
        self._gradient_cache.clear()

    def warm_cache(
        self,
        params: ParameterSet,
        *,
        chunk_size: int | tuple[int, int] = 16,
    ) -> None:
        """Rebuild all forward nodes at the current parameter versions.

        This is called after an optimisation update and after recovery. It
        makes the next step's reuse decision depend on the committed parameter
        state rather than on whether the process happened to survive.
        """
        self.clear()
        chunks = make_chunks(self.scene.width, self.scene.height, chunk_size)
        for chunk in chunks:
            self._eval_chunk(
                params,
                chunk,
                set(NODE_ORDER),
                reused=[],
                recomputed=[],
            )

    def poisoned_cache_entries(self) -> list[dict[str, object]]:
        bad: list[dict[str, object]] = []
        for (chunk_index, node), entry in sorted(self._cache.items()):
            current = self._hash_value(node, entry.value)
            if current != entry.value_hash:
                bad.append(
                    {
                        "chunk_index": chunk_index,
                        "node": node,
                        "reason": "content_hash_mismatch",
                    }
                )
        return bad

    def evaluate(
        self,
        params: ParameterSet,
        *,
        modified_groups: Iterable[str] = (),
        chunk_size: int | tuple[int, int] = 16,
        parallel: int = 1,
        verify_incremental: bool = True,
        force_full: bool = False,
    ) -> GradientResult:
        modified = tuple(group for group in GROUPS if group in set(modified_groups))
        affected = self.graph.affected_nodes(modified)
        affected_set = set(affected)
        chunks = make_chunks(self.scene.width, self.scene.height, chunk_size)
        all_signature = params.signature(GROUPS)

        forced_reason = "force_full" if force_full else None
        if not force_full:
            forced_reason = self._detect_stale_or_corrupt(params, chunks, affected_set)
        recompute_all = forced_reason is not None
        reused: list[str] = []
        recomputed: list[str] = []

        forward_by_chunk: dict[int, ForwardChunk] = {}
        for chunk in chunks:
            forward = self._eval_chunk(
                params,
                chunk,
                affected_set if not recompute_all else set(NODE_ORDER),
                reused,
                recomputed,
            )
            forward_by_chunk[chunk.index] = forward

        gradient_result = None
        if not modified and not recompute_all:
            gradient_result = self._reuse_gradient_chunks(
                params, chunks, chunk_size, parallel, all_signature
            )
        if gradient_result is None:
            gradient_result = compute_gradients(
                params,
                self.scene,
                chunk_size=chunk_size,
                parallel=parallel,
            )
        gradient_result.affected_nodes = affected
        gradient_result.reused_nodes = tuple(
            node for node in NODE_ORDER if node in set(reused)
        )
        gradient_result.recomputed_nodes = tuple(
            node for node in NODE_ORDER if node in set(recomputed)
        )

        # The per-chunk gradient cache is keyed by every parameter version and
        # content hash. A hit is allowed only when the whole signature matches.
        for chunk in chunks:
            if chunk.index in self._gradient_cache:
                signature, cached_loss, cached_gradients, cached_hash = self._gradient_cache[
                    chunk.index
                ]
                if signature != all_signature:
                    self._gradient_cache.pop(chunk.index, None)
        for result in gradient_result.chunk_results:
            self._gradient_cache[result.chunk.index] = (
                all_signature,
                result.loss,
                {group: result.gradients[group].copy() for group in GROUPS},
                result.gradients_sha256,
            )

        if forced_reason is not None:
            gradient_result.degraded = True
            gradient_result.downgrade_reason = forced_reason

        if verify_incremental and not recompute_all:
            reference = compute_gradients(
                params,
                self.scene,
                chunk_size=chunk_size,
                parallel=parallel,
            )
            equivalent, difference = results_equivalent(gradient_result, reference)
            gradient_result.verification = {
                "checked": True,
                "equivalent": equivalent,
                "difference": difference,
                "reference_sha256": reference.gradients_sha256,
            }
            if not equivalent:
                reference.degraded = True
                reference.downgrade_reason = f"incremental_mismatch:{difference}"
                reference.verification = {
                    "checked": True,
                    "equivalent": False,
                    "difference": difference,
                }
                return reference
        else:
            gradient_result.verification = {
                "checked": False,
                "equivalent": None,
                "difference": None,
            }
        return gradient_result

    def full_reference(
        self,
        params: ParameterSet,
        *,
        chunk_size: int | tuple[int, int] = 16,
        parallel: int = 1,
    ) -> GradientResult:
        return compute_gradients(
            params,
            self.scene,
            chunk_size=chunk_size,
            parallel=parallel,
        )

    def _detect_stale_or_corrupt(
        self,
        params: ParameterSet,
        chunks: list[PixelChunk],
        affected: set[str],
    ) -> str | None:
        for chunk in chunks:
            for node in NODE_ORDER:
                if node in affected:
                    continue
                entry = self._cache.get((chunk.index, node))
                if entry is None:
                    continue
                expected = params.signature(NODE_PARAM_GROUPS[node])
                if entry.signature != expected:
                    return (
                        f"stale_dependency:{node}:chunk={chunk.index}:"
                        "upstream_version_or_content_changed"
                    )
                if self._hash_value(node, entry.value) != entry.value_hash:
                    return f"cache_hash_mismatch:{node}:chunk={chunk.index}"
        return None

    def _reuse_gradient_chunks(
        self,
        params: ParameterSet,
        chunks: list[PixelChunk],
        chunk_size: int | tuple[int, int],
        parallel: int,
        signature: tuple[tuple[str, int, str], ...],
    ) -> GradientResult | None:
        results: list[ChunkGradient] = []
        for chunk in chunks:
            cached = self._gradient_cache.get(chunk.index)
            if cached is None:
                return None
            cached_signature, cached_loss, cached_gradients, cached_hash = cached
            if cached_signature != signature:
                return None
            if chunk_gradient_hash(cached_gradients) != cached_hash:
                return None
            results.append(
                ChunkGradient(
                    chunk=chunk,
                    loss=cached_loss,
                    gradients={
                        group: cached_gradients[group].copy() for group in GROUPS
                    },
                    gradients_sha256=cached_hash,
                )
            )
        reduced = reduce_chunk_results(
            results,
            chunk_size=chunk_size,
            parallel=parallel,
        )
        return reduced

    def _eval_chunk(
        self,
        params: ParameterSet,
        chunk: PixelChunk,
        affected: set[str],
        reused: list[str],
        recomputed: list[str],
    ) -> ForwardChunk:
        def cached(node: str) -> StageCache | None:
            entry = self._cache.get((chunk.index, node))
            if (
                entry is not None
                and node not in affected
                and entry.signature == params.signature(NODE_PARAM_GROUPS[node])
                and self._hash_value(node, entry.value) == entry.value_hash
            ):
                if node not in reused:
                    reused.append(node)
                return entry
            return None

        def store(node: str, value: Any) -> Any:
            entry = StageCache(
                signature=params.signature(NODE_PARAM_GROUPS[node]),
                value=value,
                value_hash=self._hash_value(node, value),
                chunk_key=chunk.key,
            )
            self._cache[(chunk.index, node)] = entry
            if node not in recomputed:
                recomputed.append(node)
            return value

        rays_entry = cached("camera_rays")
        if rays_entry is not None:
            rays = rays_entry.value
        else:
            rays = store("camera_rays", generate_rays(params, self.scene.width, self.scene.height, chunk))

        intersections_entry = cached("intersections")
        if intersections_entry is not None:
            intersections = intersections_entry.value
        else:
            intersections = store("intersections", intersect_scene(*rays, params))
        points, normals, object_ids, depths, hit = intersections

        visibility_entry = cached("visibility")
        if visibility_entry is not None:
            hit = visibility_entry.value
        else:
            hit = store("visibility", hit)

        shadow_entry = cached("shadow_factor")
        if shadow_entry is not None:
            shadow = shadow_entry.value
        else:
            shadow = store(
                "shadow_factor",
                shadow_factors(
                    points,
                    normals,
                    hit,
                    params,
                    self.scene.settings.softness,
                    self.scene.settings.shadow_samples,
                ),
            )

        shading_entry = cached("shading")
        if shading_entry is not None:
            shading = shading_entry.value
        else:
            shading = store(
                "shading",
                shade_image(points, normals, object_ids, hit, shadow, params),
            )

        loss_entry = cached("loss")
        if loss_entry is not None:
            loss = float(loss_entry.value)
        else:
            target = self.scene.target[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1]
            difference = shading - target
            loss = float(
                np.sum(difference * difference)
                / (self.scene.width * self.scene.height * 3)
            )
            store("loss", loss)

        stage_hashes = {
            "camera_rays": self._hash_value("camera_rays", rays),
            "intersections": self._hash_value("intersections", intersections),
            "visibility": self._hash_value("visibility", hit),
            "shadow_factor": self._hash_value("shadow_factor", shadow),
            "shading": self._hash_value("shading", shading),
            "loss": self._hash_value("loss", loss),
        }
        return ForwardChunk(
            rays=rays,
            intersections=intersections,
            visibility=hit,
            shadow_factor=shadow,
            shading=shading,
            loss=loss,
            stage_hashes=stage_hashes,
        )

    @staticmethod
    def _hash_value(node: str, value: Any) -> str:
        digest = hashlib.sha256()
        digest.update(node.encode("ascii"))

        def update(item: Any) -> None:
            if isinstance(item, np.ndarray):
                contiguous = np.ascontiguousarray(item)
                digest.update(str(contiguous.shape).encode("ascii"))
                digest.update(str(contiguous.dtype).encode("ascii"))
                digest.update(contiguous.tobytes())
            elif isinstance(item, (tuple, list)):
                digest.update(b"[")
                for child in item:
                    update(child)
                digest.update(b"]")
            elif isinstance(item, (int, np.integer)):
                digest.update(str(int(item)).encode("ascii"))
            elif isinstance(item, (float, np.floating)):
                digest.update(repr(float(item)).encode("ascii"))
            else:
                digest.update(repr(item).encode("ascii"))
            digest.update(b";")

        update(value)
        return digest.hexdigest()

