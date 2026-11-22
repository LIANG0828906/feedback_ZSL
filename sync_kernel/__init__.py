"""Deterministic multiplayer state-sync kernel.

The package intentionally keeps the simulation and protocol layers free of
third-party dependencies.  NumPy and PySide6 are used only by optional
analysis and presentation code.
"""

from .errors import KernelError, KernelIssue
from .models import AuthoritativeState, MatchContext, UnitState
from .protocol import (
    SUPPORTED_VERSIONS,
    DecodedFrame,
    EncodeResult,
    decode_frame,
    encode_frame,
)

__all__ = [
    "AuthoritativeState",
    "DecodedFrame",
    "EncodeResult",
    "KernelError",
    "KernelIssue",
    "MatchContext",
    "SUPPORTED_VERSIONS",
    "UnitState",
    "decode_frame",
    "encode_frame",
]

