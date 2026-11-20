"""Deterministic, versioned surface-mesh editor."""

from .errors import (
    E_COMMIT_FAILED,
    E_PARSE_INVALID,
    E_RECOVERY_INVALID,
    E_VERSION_LOSS,
    E_VERSION_STALE,
    EditCancelled,
    MeshEditorError,
)

__all__ = [
    "E_COMMIT_FAILED",
    "E_PARSE_INVALID",
    "E_RECOVERY_INVALID",
    "E_VERSION_LOSS",
    "E_VERSION_STALE",
    "EditCancelled",
    "MeshEditorError",
]

__version__ = "1.0.0"
