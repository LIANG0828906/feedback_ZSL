"""Deterministic median-split BVH for unit and cover acceleration."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Sequence

from .math3d import EPS, Ray, Vec3, ray_aabb, vmax, vmin


@dataclass(frozen=True)
class BVHNode:
    lower: Vec3
    upper: Vec3
    left: int
    right: int
    start: int
    end: int

    @property
    def is_leaf(self) -> bool:
        return self.left < 0


@dataclass(frozen=True)
class BVH:
    nodes: tuple[BVHNode, ...]
    primitive_indices: tuple[int, ...]
    root: int

    @classmethod
    def build(cls, bounds: Sequence[tuple[Vec3, Vec3]], leaf_size: int = 4) -> "BVH":
        count = len(bounds)
        if count == 0:
            return cls((), (), -1)
        leaf_size = max(1, int(leaf_size))
        nodes: list[BVHNode] = []
        ordered: list[int] = []

        centroids = [
            ((lower[0] + upper[0]) * 0.5, (lower[1] + upper[1]) * 0.5, (lower[2] + upper[2]) * 0.5)
            for lower, upper in bounds
        ]

        def aggregate(indices: Sequence[int]) -> tuple[Vec3, Vec3]:
            lower = bounds[indices[0]][0]
            upper = bounds[indices[0]][1]
            for index in indices[1:]:
                lower = vmin(lower, bounds[index][0])
                upper = vmax(upper, bounds[index][1])
            return lower, upper

        def build_node(indices: list[int]) -> int:
            lower, upper = aggregate(indices)
            node_index = len(nodes)
            nodes.append(BVHNode(lower, upper, -1, -1, 0, 0))
            if len(indices) <= leaf_size:
                start = len(ordered)
                ordered.extend(sorted(indices))
                nodes[node_index] = BVHNode(lower, upper, -1, -1, start, len(ordered))
                return node_index

            extents = (upper[0] - lower[0], upper[1] - lower[1], upper[2] - lower[2])
            axis = max(range(3), key=lambda item: (extents[item], -item))
            indices.sort(key=lambda item: (centroids[item][axis], item))
            middle = len(indices) // 2
            left_indices = indices[:middle]
            right_indices = indices[middle:]
            left = build_node(left_indices)
            right = build_node(right_indices)
            nodes[node_index] = BVHNode(lower, upper, left, right, 0, 0)
            return node_index

        root = build_node(list(range(count)))
        return cls(tuple(nodes), tuple(ordered), root)

    def candidate_indices(self, ray: Ray) -> list[int]:
        if self.root < 0:
            return []
        candidates: list[int] = []
        stack = [self.root]
        while stack:
            node_index = stack.pop()
            node = self.nodes[node_index]
            if ray_aabb(ray.origin, ray.direction, node.lower, node.upper) is None:
                continue
            if node.is_leaf:
                candidates.extend(self.primitive_indices[node.start:node.end])
            else:
                # Push far child first so the near child is visited first on a LIFO stack.
                left = self.nodes[node.left]
                right = self.nodes[node.right]
                left_hit = ray_aabb(ray.origin, ray.direction, left.lower, left.upper)
                right_hit = ray_aabb(ray.origin, ray.direction, right.lower, right.upper)
                left_distance = math.inf if left_hit is None else left_hit[0]
                right_distance = math.inf if right_hit is None else right_hit[0]
                if left_distance <= right_distance:
                    if right_hit is not None:
                        stack.append(node.right)
                    if left_hit is not None:
                        stack.append(node.left)
                else:
                    if left_hit is not None:
                        stack.append(node.left)
                    if right_hit is not None:
                        stack.append(node.right)
        return candidates

    def validate(self) -> bool:
        if not self.nodes:
            return self.root == -1
        if self.root < 0 or self.root >= len(self.nodes):
            return False
        for index, node in enumerate(self.nodes):
            if node.left >= len(self.nodes) or node.right >= len(self.nodes):
                return False
            if node.is_leaf:
                if node.start < 0 or node.end < node.start or node.end > len(self.primitive_indices):
                    return False
            elif node.left == index or node.right == index:
                return False
            if any(not math.isfinite(value) for value in (*node.lower, *node.upper)):
                return False
            if any(node.lower[axis] > node.upper[axis] + EPS for axis in range(3)):
                return False
        return True

