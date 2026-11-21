"""Validated pinhole camera and screen-to-world ray construction."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Mapping, Sequence

from .canonical import sha256_json
from .errors import E_RAY_INVALID, fail
from .geometry import EPS, Vec3, add, cross, dot, length, mul, normalize, sub


@dataclass(frozen=True)
class Ray:
    origin: Vec3
    direction: Vec3

    def point_at(self, t: float) -> Vec3:
        return add(self.origin, mul(self.direction, float(t)))

    def to_dict(self) -> dict[str, list[float]]:
        return {
            "origin": list(self.origin),
            "direction": list(self.direction),
        }


@dataclass(frozen=True)
class Camera:
    camera_id: str
    position: Vec3
    target: Vec3
    up: Vec3
    fov_y_deg: float
    near: float
    far: float
    viewport_width: int
    viewport_height: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "camera_id": self.camera_id,
            "position": list(self.position),
            "target": list(self.target),
            "up": list(self.up),
            "fov_y_deg": self.fov_y_deg,
            "near": self.near,
            "far": self.far,
            "viewport_width": self.viewport_width,
            "viewport_height": self.viewport_height,
        }

    @staticmethod
    def from_dict(value: Mapping[str, Any]) -> "Camera":
        try:
            position = tuple(float(x) for x in value["position"])  # type: ignore[arg-type]
            target = tuple(float(x) for x in value["target"])  # type: ignore[arg-type]
            up = tuple(float(x) for x in value.get("up", (0.0, 0.0, 1.0)))  # type: ignore[arg-type]
            if len(position) != 3 or len(target) != 3 or len(up) != 3:
                raise ValueError("camera vectors must have three coordinates")
            return Camera(
                camera_id=str(value.get("camera_id", "camera")),
                position=position,
                target=target,
                up=up,
                fov_y_deg=float(value["fov_y_deg"]),
                near=float(value["near"]),
                far=float(value["far"]),
                viewport_width=int(value["viewport_width"]),
                viewport_height=int(value["viewport_height"]),
            )
        except (KeyError, TypeError, ValueError) as exc:
            fail(
                E_RAY_INVALID,
                "camera parameters are missing or malformed",
                field="camera",
                error=str(exc),
            )

    def content_hash(self) -> str:
        return sha256_json(self.to_dict())

    def validate(self) -> "Camera":
        if (
            len(self.position) != 3
            or len(self.target) != 3
            or len(self.up) != 3
        ):
            fail(
                E_RAY_INVALID,
                "camera vectors must contain exactly three coordinates",
                field="camera",
                camera_id=self.camera_id,
            )
        values = (
            *self.position,
            *self.target,
            *self.up,
            self.fov_y_deg,
            self.near,
            self.far,
        )
        if not all(math.isfinite(float(value)) for value in values):
            fail(
                E_RAY_INVALID,
                "camera contains a non-finite parameter",
                field="camera",
                camera_id=self.camera_id,
            )
        if not self.camera_id:
            fail(E_RAY_INVALID, "camera id must not be empty", field="camera.camera_id")
        if self.viewport_width <= 0 or self.viewport_height <= 0:
            fail(
                E_RAY_INVALID,
                "camera viewport dimensions must be positive",
                field="camera.viewport",
                width=self.viewport_width,
                height=self.viewport_height,
            )
        if length(sub(self.target, self.position)) <= EPS:
            fail(
                E_RAY_INVALID,
                "camera position and target coincide",
                field="camera.target",
                camera_id=self.camera_id,
            )
        if length(self.up) <= EPS:
            fail(
                E_RAY_INVALID,
                "camera up vector is zero",
                field="camera.up",
                camera_id=self.camera_id,
            )
        forward = normalize(sub(self.target, self.position))
        if abs(dot(forward, normalize(self.up))) >= 1.0 - 1e-10:
            fail(
                E_RAY_INVALID,
                "camera up vector is parallel to its view direction",
                field="camera.up",
                camera_id=self.camera_id,
            )
        if not (0.0 < self.fov_y_deg < 180.0):
            fail(
                E_RAY_INVALID,
                "camera vertical field of view must be in (0, 180) degrees",
                field="camera.fov_y_deg",
                camera_id=self.camera_id,
                value=self.fov_y_deg,
            )
        if self.near <= 0.0 or self.far <= self.near:
            fail(
                E_RAY_INVALID,
                "camera near/far planes are invalid",
                field="camera.near",
                camera_id=self.camera_id,
                near=self.near,
                far=self.far,
            )
        return self

    def basis(self) -> tuple[Vec3, Vec3, Vec3]:
        self.validate()
        forward = normalize(sub(self.target, self.position))
        right = normalize(cross(forward, normalize(self.up)))
        camera_up = normalize(cross(right, forward))
        return forward, right, camera_up

    def screen_to_ray(self, screen: Sequence[float], *, injection: bool = False) -> Ray:
        self.validate()
        if len(screen) != 2:
            fail(
                E_RAY_INVALID,
                "screen coordinate must contain exactly two values",
                field="screen",
                actual=len(screen),
            )
        try:
            x = float(screen[0])
            y = float(screen[1])
        except (TypeError, ValueError):
            fail(E_RAY_INVALID, "screen coordinate is not numeric", field="screen")
        if not math.isfinite(x) or not math.isfinite(y):
            fail(
                E_RAY_INVALID,
                "screen coordinate is not finite",
                field="screen",
                screen=[str(x), str(y)],
            )
        if (
            x < 0.0
            or y < 0.0
            or x > float(self.viewport_width)
            or y > float(self.viewport_height)
        ):
            fail(
                E_RAY_INVALID,
                "screen coordinate is outside the camera viewport",
                field="screen",
                screen=[x, y],
                viewport=[self.viewport_width, self.viewport_height],
            )
        forward, right, camera_up = self.basis()
        ndc_x = (x / float(self.viewport_width)) * 2.0 - 1.0
        ndc_y = 1.0 - (y / float(self.viewport_height)) * 2.0
        aspect = float(self.viewport_width) / float(self.viewport_height)
        tan_half = math.tan(math.radians(self.fov_y_deg) * 0.5)
        direction_raw = add(
            add(forward, mul(right, ndc_x * tan_half * aspect)),
            mul(camera_up, ndc_y * tan_half),
        )
        if not all(math.isfinite(value) for value in direction_raw):
            fail(
                E_RAY_INVALID,
                "camera produced a non-finite ray direction",
                field="ray.direction",
                camera_id=self.camera_id,
            )
        if length(direction_raw) <= EPS:
            fail(
                E_RAY_INVALID,
                "camera produced a zero-length ray direction",
                field="ray.direction",
                camera_id=self.camera_id,
            )
        return Ray(self.position, normalize(direction_raw))

    def project(self, point: Vec3) -> tuple[float, float]:
        """Project a world point to pixel coordinates (used by samples)."""

        forward, right, camera_up = self.basis()
        relative = sub(point, self.position)
        depth = dot(relative, forward)
        if depth <= self.near:
            fail(
                E_RAY_INVALID,
                "world point is behind the camera near plane",
                field="camera.projection",
                depth=depth,
            )
        tan_half = math.tan(math.radians(self.fov_y_deg) * 0.5)
        aspect = float(self.viewport_width) / float(self.viewport_height)
        ndc_x = dot(relative, right) / (depth * tan_half * aspect)
        ndc_y = dot(relative, camera_up) / (depth * tan_half)
        sx = (ndc_x + 1.0) * 0.5 * self.viewport_width
        sy = (1.0 - ndc_y) * 0.5 * self.viewport_height
        return (sx, sy)
