"""pcwb — versioned, thread-safe local point-cloud workbench.

The package is deliberately split into small, single-responsibility modules:

errors       stable error codes returned across the public API
format       custom binary point-cloud format (read / write / validate)
cloud        immutable PointCloud snapshot value object
selection    rectangle / sphere selection bound to a specific version
operators    pure, byte-deterministic edit operators
versions     atomic, versioned on-disk store with a hash manifest
engine       background edit thread with cooperative cancel and snapshot swap
render       OpenGL (EGL headless / GLFW window) plus a CPU fallback renderer
telemetry    bounded latency sampling and deterministic trajectory logs
samples      deterministic sample generator
"""

from .errors import (
    E_CANCELLED,
    E_COMMIT_FAILED,
    E_POINTCLOUD_INVALID,
    E_SELECTION_STALE,
    E_UNKNOWN_VERSION,
)

__all__ = [
    "E_CANCELLED",
    "E_COMMIT_FAILED",
    "E_POINTCLOUD_INVALID",
    "E_SELECTION_STALE",
    "E_UNKNOWN_VERSION",
]
