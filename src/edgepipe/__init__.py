"""Deterministic chunked edge detection pipeline."""

from .checkpoint import CheckpointStore, SnapshotValidation
from .config import ALGORITHM_VERSION, STAGES, PipelineConfig
from .engine import PipelineEngine, PipelineResult, SimulatedCrash
from .errors import ErrorCode, EdgePipeError

__all__ = [
    "ALGORITHM_VERSION",
    "CheckpointStore",
    "EdgePipeError",
    "ErrorCode",
    "PipelineConfig",
    "PipelineEngine",
    "PipelineResult",
    "STAGES",
    "SimulatedCrash",
    "SnapshotValidation",
]

