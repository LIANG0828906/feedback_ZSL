"""Deterministic binary BVH used for unit and cover broad-phase queries."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Sequence

from .geometry import Vec3, ray_aabb_intersection


@dataclass(frozen=True)
class Bounds:
    minimum: Vec3
    maximum: Vec3

    @property
    def center(self) -> Vec3:
        return (
            (self.minimum[0] + self.maximum[0]) * 0.5,
            (self.minimum[1] + self.maximum[1]) * 0.5,
            (self.minimum[2] + self.maximum[2]) * 0.5,
        )

    @property
    def extent(self) -> Vec3:
        return (
            self.maximum[0] - self.minimum[0],
            self.maximum[1] - self.minimum[1],
            self.maximum[2] - self.minimum[2],
        )

    def union(self, other: "Bounds") -> "Bounds":
        return Bounds(
            (
                min(self.minimum[0], other.minimum[0]),
                min(self.minimum[1], other.minimum[1]),
                min(self.minimum[2], other.minimum[2]),
            ),
            (
                max(self.maximum[0], other.maximum[0]),
                max(self.maximum[1], other.maximum[1]),
                max(self.maximum[2], other.maximum[2]),
            ),
        )

    def ray_intersects(self, origin: Vec3, direction: Vec3) -> bool:
        return ray_aabb_intersection(origin, direction, self.minimum, self.maximum) is not None


@dataclass(frozen=True)
class BVHNode:
    bounds: Bounds
    left: int = -1
    right: int = -1
    start: int = 0
    count: int = 0

    @property
    def is_leaf(self) -> bool:
        return self.left < 0


@dataclass(frozen=True)
class BVH:
    nodes: tuple[BVHNode, ...]
    indices: tuple[int, ...]
    leaf_size: int

    @staticmethod
    def build(bounds: Sequence[Bounds], leaf_size: int = 4) -> "BVH":
        if leaf_size <= 0:
            raise ValueError("leaf_size must be positive")
        if not bounds:
            return BVH(nodes=(), indices=(), leaf_size=leaf_size)
        nodes: list[BVHNode] = []
        leaf_indices: list[int] = []

        def node_bounds(items: tuple[int, ...]) -> Bounds:
            result = bounds[items[0]]
            for item in items[1:]:
                result = result.union(bounds[item])
            return result

        def split_axis(bounds_all: Bounds) -> int:
            extent = bounds_all.extent
            if extent[0] >= extent[1] and extent[0] >= extent[2]:
                return 0
            if extent[1] >= extent[2]:
                return 1
            return 2

        def build_node(items: tuple[int, ...]) -> int:
            node_index = len(nodes)
            bounds_all = node_bounds(items)
            nodes.append(BVHNode(bounds_all))
            if len(items) <= leaf_size:
                start = len(leaf_indices)
                leaf_indices.extend(sorted(items))
                nodes[node_index] = BVHNode(
                    bounds=bounds_all,
                    start=start,
                    count=len(items),
                )
                return node_index

            axis = split_axis(bounds_all)
            ordered = tuple(
                sorted(
                    items,
                    key=lambda item: (
                        bounds[item].center[axis],
                        bounds[item].minimum[(axis + 1) % 3],
                        bounds[item].minimum[(axis + 2) % 3],
                        item,
                    ),
                )
            )
            middle = len(ordered) // 2
            left = build_node(ordered[:middle])
            right = build_node(ordered[middle:])
            nodes[node_index] = BVHNode(
                bounds=bounds_all,
                left=left,
                right=right,
            )
            return node_index

        root = build_node(tuple(range(len(bounds))))
        if root != 0:
            raise AssertionError("root must be the first node")
        return BVH(
            nodes=tuple(nodes),
            indices=tuple(leaf_indices),
            leaf_size=leaf_size,
        )

    def candidates(self, origin: Vec3, direction: Vec3) -> tuple[int, ...]:
        if not self.nodes:
            return ()
        found: list[int] = []
        stack = [0]
        while stack:
            node_index = stack.pop()
            node = self.nodes[node_index]
            if not node.bounds.ray_intersects(origin, direction):
                continue
            if node.is_leaf:
                found.extend(self.indices[node.start : node.start + node.count])
            else:
                # Push right first so traversal order is deterministic
                # left-to-right even though this is a stack.
                stack.append(node.right)
                stack.append(node.left)
        return tuple(found)

    def depth(self) -> int:
        if not self.nodes:
            return 0

        result = 0
        stack = [(0, 1)]
        while stack:
            node_index, depth = stack.pop()
            result = max(result, depth)
            node = self.nodes[node_index]
            if not node.is_leaf:
                stack.append((node.left, depth + 1))
                stack.append((node.right, depth + 1))
        return result
