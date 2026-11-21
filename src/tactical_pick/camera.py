"""Validated pinhole camera and screen-to-world ray generation."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Sequence

from .errors import fail
from .math3d import (
    EPS,
    Mat4,
    Ray,
    Vec3,
    cross,
    dot,
    length,
    mat4_det,
    mat4_vec,
    normalize,
    sub,
    validate_ray,
)


def _matrix(value: Sequence[Sequence[float]]) -> Mat4:
    rows = []
    for row in value:
        rows.append((float(row[0]), float(row[1]), float(row[2]), float(row[3])))
    return (rows[0], rows[1], rows[2], rows[3])


@dataclass(frozen=True)
class Camera:
    width: float
    height: float
    fx: float
    fy: float
    cx: float
    cy: float
    camera_to_world: Mat4

    def validate(self) -> None:
        if not math.isfinite(self.width) or self.width <= 0.0:
            fail("E_RAY_INVALID", "camera width is degenerate", "camera.intrinsics.width")
        if not math.isfinite(self.height) or self.height <= 0.0:
            fail("E_RAY_INVALID", "camera height is degenerate", "camera.intrinsics.height")
        if not math.isfinite(self.fx) or self.fx <= EPS:
            fail("E_RAY_INVALID", "camera focal length fx is degenerate", "camera.intrinsics.fx")
        if not math.isfinite(self.fy) or self.fy <= EPS:
            fail("E_RAY_INVALID", "camera focal length fy is degenerate", "camera.intrinsics.fy")
        if not math.isfinite(self.cx) or not math.isfinite(self.cy):
            fail("E_RAY_INVALID", "camera principal point is not finite", "camera.intrinsics.principal_point")
        values = [value for row in self.camera_to_world for value in row]
        if any(not math.isfinite(value) for value in values):
            fail("E_RAY_INVALID", "camera extrinsic matrix is not finite", "camera.camera_to_world")
        if abs(self.camera_to_world[3][0]) > 1.0e-7 or abs(self.camera_to_world[3][1]) > 1.0e-7:
            fail("E_RAY_INVALID", "camera extrinsic matrix has an invalid bottom row", "camera.camera_to_world[3]")
        if abs(self.camera_to_world[3][2]) > 1.0e-7 or abs(self.camera_to_world[3][3] - 1.0) > 1.0e-7:
            fail("E_RAY_INVALID", "camera extrinsic matrix has an invalid bottom row", "camera.camera_to_world[3]")
        if abs(mat4_det(self.camera_to_world)) <= EPS:
            fail("E_RAY_INVALID", "camera extrinsic matrix is singular", "camera.camera_to_world")

    def _screen_bounds_check(self, screen: Sequence[float]) -> tuple[float, float]:
        if len(screen) != 2:
            fail("E_SCREEN_OUT_OF_RANGE", "screen coordinate must have two components", "camera.screen")
        x = float(screen[0])
        y = float(screen[1])
        if not math.isfinite(x) or not math.isfinite(y):
            fail(
                "E_SCREEN_OUT_OF_RANGE",
                "screen coordinate is not finite",
                "camera.screen",
                first_conflict={"screen": [x, y]},
            )
        if x < 0.0 or y < 0.0 or x >= self.width or y >= self.height:
            fail(
                "E_SCREEN_OUT_OF_RANGE",
                "screen coordinate is outside the camera viewport",
                "camera.screen",
                first_conflict={"screen": [x, y], "viewport": [self.width, self.height]},
            )
        return x, y

    def screen_to_ray(self, screen: Sequence[float]) -> Ray:
        self.validate()
        x, y = self._screen_bounds_check(screen)
        camera_direction = (
            (x - self.cx) / self.fx,
            -(y - self.cy) / self.fy,
            1.0,
        )
        origin = mat4_vec(self.camera_to_world, (0.0, 0.0, 0.0), 1.0)
        world_direction = mat4_vec(self.camera_to_world, camera_direction, 0.0)
        direction = normalize(world_direction)
        ray = Ray(origin, direction)
        validate_ray(ray)
        return ray

    def to_dict(self) -> dict[str, Any]:
        return {
            "width": self.width,
            "height": self.height,
            "fx": self.fx,
            "fy": self.fy,
            "cx": self.cx,
            "cy": self.cy,
            "camera_to_world": [list(row) for row in self.camera_to_world],
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "Camera":
        return cls(
            width=float(value["width"]),
            height=float(value["height"]),
            fx=float(value["fx"]),
            fy=float(value["fy"]),
            cx=float(value["cx"]),
            cy=float(value["cy"]),
            camera_to_world=_matrix(value["camera_to_world"]),
        )

    @classmethod
    def look_at(
        cls,
        eye: Sequence[float],
        target: Sequence[float],
        width: float = 960.0,
        height: float = 540.0,
        fx: float = 800.0,
        fy: float = 800.0,
        up: Sequence[float] = (0.0, 1.0, 0.0),
    ) -> "Camera":
        eye_v: Vec3 = (float(eye[0]), float(eye[1]), float(eye[2]))
        target_v: Vec3 = (float(target[0]), float(target[1]), float(target[2]))
        forward = normalize(sub(target_v, eye_v))
        world_up = normalize((float(up[0]), float(up[1]), float(up[2])))
        if length(forward) <= EPS:
            fail("E_RAY_INVALID", "camera eye and target coincide", "camera.look_at.forward")
        right = normalize(cross(forward, world_up))
        if length(right) <= EPS:
            fail("E_RAY_INVALID", "camera up vector is parallel to view direction", "camera.look_at.up")
        camera_up = normalize(cross(right, forward))
        matrix: Mat4 = (
            (right[0], camera_up[0], forward[0], eye_v[0]),
            (right[1], camera_up[1], forward[1], eye_v[1]),
            (right[2], camera_up[2], forward[2], eye_v[2]),
            (0.0, 0.0, 0.0, 1.0),
        )
        camera = cls(width, height, fx, fy, width * 0.5, height * 0.5, matrix)
        camera.validate()
        return camera

    def camera_position(self) -> Vec3:
        return (
            self.camera_to_world[0][3],
            self.camera_to_world[1][3],
            self.camera_to_world[2][3],
        )

