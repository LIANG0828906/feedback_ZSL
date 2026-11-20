"""Deterministic differentiable inverse rendering with dependency-aware reuse."""

from .errors import ErrorCode, ToolError
from .scene import Scene, load_scene
from .params import GroupSpec, ParameterStore, load_group_specs
from .graph import DependencyGraph, GraphDiff, build_dependency_graph
from .engine import EvaluationResult, GradientResult, IncrementalEngine
from .checkpoint import CheckpointManager, ResumeState
from .fit import FitConfig, FitResult, FitService, run_fit
from .snapshot import Snapshot, SnapshotPublisher

__all__ = [
    "CheckpointManager",
    "DependencyGraph",
    "ErrorCode",
    "EvaluationResult",
    "FitConfig",
    "FitResult",
    "FitService",
    "GradientResult",
    "GraphDiff",
    "GroupSpec",
    "IncrementalEngine",
    "ParameterStore",
    "ResumeState",
    "Scene",
    "Snapshot",
    "SnapshotPublisher",
    "ToolError",
    "build_dependency_graph",
    "load_group_specs",
    "load_scene",
    "run_fit",
]

