"""Brocade embroidery stage: deterministic off-screen renderer.

The package implements a self-contained, NumPy/Pillow based renderer for the
Republican-era embroidered-stage transition shot.  Every random value is drawn
from an explicitly seeded integer hash so output never depends on NumPy's
global RNG state or triangle traversal order.
"""

__all__ = [
    "rng",
    "geometry",
    "scene_gen",
    "validation",
    "sampling",
    "shading",
    "shadow",
    "rasterizer",
    "snapshot",
    "render",
    "cli",
]
