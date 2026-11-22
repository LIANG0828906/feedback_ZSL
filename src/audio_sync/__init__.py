"""Multichannel acquisition alignment and clock synchronization."""

from .config import AlignmentConfig, AlgorithmInfo
from .engine import AlignmentEngine, AlignmentResult, compare_results, run_three_paths
from .errors import SyncError
from .generator import generate_session
from .manifest import load_session, load_manifest, validate_session

__all__ = [
    "AlgorithmInfo",
    "AlignmentConfig",
    "AlignmentEngine",
    "AlignmentResult",
    "SyncError",
    "compare_results",
    "generate_session",
    "load_manifest",
    "load_session",
    "run_three_paths",
    "validate_session",
]

__version__ = "1.0.0"

