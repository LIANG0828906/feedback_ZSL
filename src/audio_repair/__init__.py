"""Deterministic, resumable speech and music restoration."""

from .config import (
    CHECKPOINT_SCHEMA_VERSION,
    PIPELINE_VERSION,
    RepairConfig,
    SessionConfig,
)
from .errors import AppError

__all__ = [
    "AppError",
    "CHECKPOINT_SCHEMA_VERSION",
    "PIPELINE_VERSION",
    "RepairConfig",
    "SessionConfig",
]

__version__ = "0.1.0"

