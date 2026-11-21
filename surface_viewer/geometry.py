from __future__ import annotations

import math
from dataclasses import dataclass
from functools import lru_cache
import numpy as np

from .model import SurfaceAssembly, SurfacePatch


def _bezier_basis(count: int, parameter: float) -> np.ndarray:
    degree = count - 1
    result = np.zeros(count, dtype=np.float64)
    for index in range(count):
        result[index] = (
            math.comb(degree, index)
            * (parameter**index)
            * ((1.0 - parameter) ** (degree - index))
        )
    return result


@lru_cache(maxsize=512)
def _knot_vector(control_count: int, degree: int) -> tuple[float, ...]:
    interior = control_count - degree - 1
    return (
        (0.0,) * (degree + 1)
        + tuple(
            index / (interior + 1)
            for index in range(1, interior + 1)
        )
        + (1.0,) * (degree + 1)
    )


def _bspline_basis(control_count: int, parameter: float, degree: int) -> np.ndarray:
    degree = min(degree, control_count - 1)
    knots = _knot_vector(control_count, degree)
    values = np.zeros(control_count, dtype=np.float64)
    for index in range(control_count):
        values[index] = _basis_recursive(
            index, degree, parameter, knots, control_count
        )
    total = float(values.sum())
    if total > 0.0:
        values /= total
    return values


def _basis_recursive(
    index: int,
    degree: int,
    parameter: float,
    knots: tuple[float, ...],
    control_count: int,
) -> float:
    if degree == 0:
        if index == control_count - 1 and parameter == 1.0:
            return 1.0
        return 1.0 if knots[index] <= parameter < knots[index + 1] else 0.0

    left = 0.0
    left_denominator = knots[index + degree] - knots[index]
    if left_denominator > 0.0:
        left = (
            (parameter - knots[index])
            / left_denominator
            * _basis_recursive(
                index, degree - 1, parameter, knots, control_count
            )
        )

    right = 0.0
    right_denominator = knots[index + degree + 1] - knots[index + 1]
    if right_denominator > 0.0:
        right = (
            (knots[index + degree + 1] - parameter)
            / right_denominator
            * _basis_recursive(
                index + 1, degree - 1, parameter, knots, control_count
            )
        )
    return left + right


def _basis_vector(patch: SurfacePatch, count: int, parameter: float) -> np.ndarray:
    if patch.surface_type == "bezier":
        return _bezier_basis(count, parameter)
    if patch.surface_type in ("bspline", "nurbs"):
        return _bspline_basis(count, parameter, min(3, count - 1))
    raise ValueError(f"unsupported surface type: {patch.surface_type}")


@dataclass(frozen=True)
class BoundaryRef:
    patch_id: str
    side: str
    reversed: bool = False


class AssemblyTopology:
    """Detects exact shared parametric boundaries.

    The sample generator builds adjacent patches from one control lattice, so
    their boundary control points are byte-for-byte equal.  When such a
    boundary is found, both patches sample the same canonical control sequence.
    This is what makes block seams bitwise continuous instead of merely close.
    """

    def __init__(self, assembly: SurfaceAssembly):
        self.assembly = assembly
        self._matches: dict[tuple[str, str], tuple[BoundaryRef, BoundaryRef]] = {}
        self._discover()

    @staticmethod
    def _boundary_points(patch: SurfacePatch, side: str) -> tuple:
        if side == "u0":
            return patch.control_points[0]
        if side == "u1":
            return patch.control_points[-1]
        if side == "v0":
            return tuple(row[0] for row in patch.control_points)
        if side == "v1":
            return tuple(row[-1] for row in patch.control_points)
        raise ValueError(side)

    def _discover(self) -> None:
        sides = ("u0", "u1", "v0", "v1")
        opposite = {"u0": "u1", "u1": "u0", "v0": "v1", "v1": "v0"}
        patches = self.assembly.patches
        for left_index, left in enumerate(patches):
            for right in patches[left_index + 1 :]:
                if left.surface_type != right.surface_type:
                    continue
                for left_side in sides:
                    left_points = self._boundary_points(left, left_side)
                    if len(left_points) < 2:
                        continue
                    for right_side in sides:
                        right_points = self._boundary_points(right, right_side)
                        if len(right_points) != len(left_points):
                            continue
                        if left_points == right_points:
                            match = (
                                BoundaryRef(left.patch_id, left_side, False),
                                BoundaryRef(right.patch_id, right_side, False),
                            )
                        elif left_points == tuple(reversed(right_points)):
                            match = (
                                BoundaryRef(left.patch_id, left_side, False),
                                BoundaryRef(right.patch_id, right_side, True),
                            )
                        else:
                            continue
                        self._matches[(left.patch_id, left_side)] = match
                        self._matches[(right.patch_id, right_side)] = (
                            match[1],
                            match[0],
                        )
                        break

    def match(self, patch_id: str, side: str) -> tuple[BoundaryRef, BoundaryRef] | None:
        return self._matches.get((patch_id, side))

    def shared_boundary_count(self) -> int:
        return len(self._matches) // 2


class SurfaceGeometry:
    def __init__(self, topology: AssemblyTopology | None = None):
        self.topology = topology

    def evaluate_grid(
        self, patch: SurfacePatch, u_values: np.ndarray, v_values: np.ndarray
    ) -> np.ndarray:
        control = np.asarray(patch.control_points, dtype=np.float64)
        control_count_u, control_count_v, point_size = control.shape
        u_basis = np.stack(
            [_basis_vector(patch, control_count_u, float(value)) for value in u_values]
        )
        v_basis = np.stack(
            [_basis_vector(patch, control_count_v, float(value)) for value in v_values]
        )

        if point_size == 4:
            weights = control[:, :, 3]
            homogeneous = control[:, :, :3] * weights[:, :, None]
            numerator = np.einsum(
                "ui,ijc,vj->uvc", u_basis, homogeneous, v_basis, optimize=True
            )
            denominator = np.einsum(
                "ui,ij,vj->uv", u_basis, weights, v_basis, optimize=True
            )
            safe = np.where(np.abs(denominator) < 1e-15, 1.0, denominator)
            return numerator / safe[:, :, None]

        return np.einsum(
            "ui,ijc,vj->uvc", u_basis, control, v_basis, optimize=True
        )

    @staticmethod
    def _boundary_values(patch: SurfacePatch, side: str, count: int) -> np.ndarray:
        if patch.surface_type == "bezier":
            control_count = (
                len(patch.control_points)
                if side in ("v0", "v1")
                else len(patch.control_points[0])
            )
            points = AssemblyTopology._boundary_points(patch, side)
            basis = np.stack(
                [
                    _bezier_basis(control_count, float(value))
                    for value in np.linspace(0.0, 1.0, count)
                ]
            )
            return np.einsum("ti,ic->tc", basis, np.asarray(points), optimize=True)

        if side == "u0":
            sub_patch_points = patch.control_points[0]
        elif side == "u1":
            sub_patch_points = patch.control_points[-1]
        elif side == "v0":
            sub_patch_points = tuple(row[0] for row in patch.control_points)
        else:
            sub_patch_points = tuple(row[-1] for row in patch.control_points)

        control_count = len(sub_patch_points)
        basis = np.stack(
            [
                _bspline_basis(control_count, float(value), min(3, control_count - 1))
                for value in np.linspace(0.0, 1.0, count)
            ]
        )
        points = np.asarray(sub_patch_points, dtype=np.float64)
        if points.shape[1] == 4:
            weights = points[:, 3]
            homogeneous = points[:, :3] * weights[:, None]
            numerator = np.einsum(
                "ti,ic->tc", basis, homogeneous, optimize=True
            )
            denominator = np.einsum(
                "ti,i->t", basis, weights, optimize=True
            )
            safe = np.where(np.abs(denominator) < 1e-15, 1.0, denominator)
            return numerator / safe[:, None]
        return np.einsum("ti,ic->tc", basis, points, optimize=True)

    def _canonical_boundary(
        self, patch: SurfacePatch, side: str, count: int
    ) -> np.ndarray | None:
        if self.topology is None:
            return None
        match = self.topology.match(patch.patch_id, side)
        if match is None:
            return None
        left, right = match
        other = right
        other_patch = self.topology.assembly.patch(other.patch_id)
        if other_patch.segments != patch.segments:
            return None
        values = self._boundary_values(other_patch, other.side, count)
        if other.reversed:
            values = values[::-1]
        return values

    def _apply_shared_boundary_vertices(
        self, patch: SurfacePatch, vertices: np.ndarray
    ) -> None:
        if self.topology is None:
            return
        count_u, count_v, _ = vertices.shape
        boundaries = {
            "u0": (vertices[0, :, :], count_v),
            "u1": (vertices[-1, :, :], count_v),
            "v0": (vertices[:, 0, :], count_u),
            "v1": (vertices[:, -1, :], count_u),
        }
        for side, (view, count) in boundaries.items():
            canonical = self._canonical_boundary(patch, side, count)
            if canonical is not None:
                view[...] = canonical

    def compute_mesh(self, patch: SurfacePatch) -> np.ndarray:
        segments = patch.segments
        parameters = np.linspace(0.0, 1.0, segments + 1)
        vertices = self.evaluate_grid(patch, parameters, parameters)
        vertices = np.ascontiguousarray(vertices, dtype="<f4")
        self._apply_shared_boundary_vertices(patch, vertices)
        return vertices

    def compute_normals(self, patch: SurfacePatch) -> np.ndarray:
        segments = patch.segments
        parameters = np.linspace(0.0, 1.0, segments + 1)
        epsilon = 1e-4
        plus = parameters + epsilon
        minus = parameters - epsilon

        position_u_plus = self.evaluate_grid(patch, plus, parameters)
        position_u_minus = self.evaluate_grid(patch, minus, parameters)
        position_v_plus = self.evaluate_grid(patch, parameters, plus)
        position_v_minus = self.evaluate_grid(patch, parameters, minus)

        tangent_u = (position_u_plus - position_u_minus) / (2.0 * epsilon)
        tangent_v = (position_v_plus - position_v_minus) / (2.0 * epsilon)
        normals = np.cross(tangent_u, tangent_v)
        lengths = np.linalg.norm(normals, axis=2, keepdims=True)
        normals = np.divide(
            normals,
            lengths,
            out=np.tile(np.array([0.0, 0.0, 1.0]), normals.shape[:2] + (1,)),
            where=lengths > 1e-20,
        )
        if patch.flip_normal:
            normals = -normals
        return np.ascontiguousarray(normals, dtype="<f4")


def seam_position_error(
    assembly: SurfaceAssembly,
    meshes: dict[str, np.ndarray],
) -> float:
    topology = AssemblyTopology(assembly)
    worst = 0.0
    seen: set[tuple[tuple[str, str], tuple[str, str]]] = set()
    for patch in assembly.patches:
        for side in ("u0", "u1", "v0", "v1"):
            match = topology.match(patch.patch_id, side)
            if match is None:
                continue
            left, right = match
            key = ((left.patch_id, left.side), (right.patch_id, right.side))
            if key in seen:
                continue
            seen.add(key)
            left_mesh = meshes[left.patch_id]
            right_mesh = meshes[right.patch_id]
            left_edge = _mesh_edge(left_mesh, left.side)
            right_edge = _mesh_edge(right_mesh, right.side)
            if right.reversed:
                right_edge = right_edge[::-1]
            if left_edge.shape != right_edge.shape:
                continue
            error = float(np.max(np.abs(left_edge - right_edge)))
            worst = max(worst, error)
    return worst


def _mesh_edge(mesh: np.ndarray, side: str) -> np.ndarray:
    if side == "u0":
        return mesh[0, :, :]
    if side == "u1":
        return mesh[-1, :, :]
    if side == "v0":
        return mesh[:, 0, :]
    return mesh[:, -1, :]
