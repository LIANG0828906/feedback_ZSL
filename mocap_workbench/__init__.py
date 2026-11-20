"""Deterministic BVH constraint-solving workbench.

The public API is intentionally small.  Core users normally work with
``BvhDocument``, ``ConstraintSet``, ``SolvePipeline`` and ``SnapshotStore``.
"""

from .bvh import BvhDocument, parse_bvh, parse_bvh_text
from .constraints import ConstraintSet, load_constraints
from .errors import (
    E_BUDGET_EXCEEDED,
    E_BUSY,
    E_BVH_INVALID,
    E_CANCELLED,
    E_CONFIG_INVALID,
    E_CONSTRAINT_INVALID,
    E_IO,
    E_RECOVERY_INVALID,
    BusinessError,
)
from .pipeline import RunReport, SolvePipeline
from .snapshot import RecoveryState, SnapshotStore
from .solver import FrameResult, SolveCache, SolverConfig, solve_frames

__all__ = [
    "E_BUDGET_EXCEEDED",
    "E_BUSY",
    "E_BVH_INVALID",
    "E_CANCELLED",
    "E_CONFIG_INVALID",
    "E_CONSTRAINT_INVALID",
    "E_IO",
    "E_RECOVERY_INVALID",
    "BusinessError",
    "BvhDocument",
    "ConstraintSet",
    "FrameResult",
    "RecoveryState",
    "RunReport",
    "SnapshotStore",
    "SolveCache",
    "SolvePipeline",
    "SolverConfig",
    "load_constraints",
    "parse_bvh",
    "parse_bvh_text",
    "solve_frames",
]

__version__ = "1.0.0"

