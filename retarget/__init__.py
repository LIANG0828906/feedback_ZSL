"""Skeleton animation retargeting pipeline with explicit error propagation.

Supports v1/v2/v3 JSON animation formats, float64 and fixed-point dual
compute paths, per-node error propagation, checkpoint/recovery and
deterministic sample generation.
"""

__version__ = "1.0.0"

SNAPSHOT_VERSION = 1
