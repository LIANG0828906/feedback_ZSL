"""Deterministic local match replay engine.

The package intentionally uses only the Python standard library.  All replay
arithmetic is based on ``decimal.Decimal`` or IEEE-754 binary64; no wall clock,
filesystem size, or process-global random state participates in state hashes.
"""

from .errors import ReplayError
from .model import (
    BASELINE_VERSION,
    SNAPSHOT_VERSION,
    STATE_MODEL_VERSION,
    ZERO_HASH,
)
from .validation import load_recording, parse_recording
from .engine import ReplayEngine, run_recording

__all__ = [
    "BASELINE_VERSION",
    "SNAPSHOT_VERSION",
    "STATE_MODEL_VERSION",
    "ZERO_HASH",
    "ReplayError",
    "ReplayEngine",
    "load_recording",
    "parse_recording",
    "run_recording",
]
