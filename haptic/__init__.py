"""Deterministic haptic waveform editor and incremental renderer."""

from .engine import BuildState, CompileResult, compile_signals
from .errors import HapticError
from .model import (
    HapticProject,
    ProjectConfig,
    WaveSegment,
    load_project,
    parse_project,
)
from .snapshot import SnapshotManager
from .tasks import RecomputeTaskManager
from .versioning import Publisher, VersionedSignal

__all__ = [
    "BuildState",
    "CompileResult",
    "HapticError",
    "HapticProject",
    "ProjectConfig",
    "Publisher",
    "RecomputeTaskManager",
    "SnapshotManager",
    "VersionedSignal",
    "WaveSegment",
    "compile_signals",
    "load_project",
    "parse_project",
]
