"""Deterministic glass-brick-kiln exploration renderer.

The package implements a fully deterministic, CPU/NumPy-only renderer for a
three-layer translucent glass-brick observation window in a pottery kiln.
Every random value is derived from explicit integer seeds and brick ids, so
any brick's stress fringes can be recomputed independently and repeated runs
produce byte-identical outputs.
"""

__all__ = [
    "xrand",
    "fringes",
    "model",
    "scene",
    "validate",
    "geometry",
    "lighting",
    "render",
    "snapshot",
    "io_utils",
    "cli",
]
