from __future__ import annotations

from tactical_pick.camera import Camera
from tactical_pick.model import Cover, Grid, Scene, Unit


def full_mask(cols: int, rows: int) -> bytes:
    mask = bytearray((cols * rows + 7) // 8)
    for bit in range(cols * rows):
        mask[bit >> 3] |= 1 << (bit & 7)
    return bytes(mask)


def make_grid(cols: int = 5, rows: int = 5, blocked=()) -> Grid:
    mask = bytearray(full_mask(cols, rows))
    for row, col in blocked:
        bit = row * cols + col
        mask[bit >> 3] &= ~(1 << (bit & 7))
    return Grid(cols, rows, 1.0, 0.0, 0.0, bytes(mask))


def aabb_unit(
    unit_id: int,
    center,
    half=(0.35, 0.35),
    height=1.5,
    z0=0.0,
    name: str | None = None,
) -> Unit:
    cx, cy = center
    hx, hy = half
    return Unit(
        unit_id,
        name or f"unit-{unit_id}",
        "aabb",
        (cx - hx, cy - hy, z0),
        (cx + hx, cy + hy, z0 + height),
    )


def capsule_unit(
    unit_id: int,
    a,
    b,
    radius: float,
    name: str | None = None,
) -> Unit:
    return Unit(
        unit_id,
        name or f"capsule-{unit_id}",
        "capsule",
        (
            min(a[0], b[0]) - radius,
            min(a[1], b[1]) - radius,
            min(a[2], b[2]) - radius,
        ),
        (
            max(a[0], b[0]) + radius,
            max(a[1], b[1]) + radius,
            max(a[2], b[2]) + radius,
        ),
        capsule_a=a,
        capsule_b=b,
        radius=radius,
    )


def simple_scene() -> Scene:
    vertices = (
        (0.0, 4.0, 0.0),
        (1.0, 4.0, 0.0),
        (0.5, 4.8, 1.2),
    )
    return Scene(
        structure_version=9,
        ground_height=0.0,
        grid=make_grid(5, 5, [(0, 0), (4, 4)]),
        vertices=vertices,
        units=(
            aabb_unit(10, (1.0, 1.0)),
            capsule_unit(11, (3.2, 0.8, 0.5), (3.8, 0.8, 0.5), 0.3),
        ),
        covers=(Cover(100, 0, 1, 2, "wall"),),
    )


def center_camera(
    *,
    position=(0.0, 0.0, 5.0),
    target=(0.0, 0.0, 0.0),
    up=(0.0, 1.0, 0.0),
    fov=60.0,
    width=800,
    height=600,
) -> Camera:
    return Camera(
        camera_id="test-camera",
        position=position,
        target=target,
        up=up,
        fov_y_deg=fov,
        near=0.1,
        far=100.0,
        viewport_width=width,
        viewport_height=height,
    )
