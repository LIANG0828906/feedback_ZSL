"""Deterministic local multi-track mixing and slice recording.

The public API intentionally keeps the long-running engine separate from the
optional PySide6 workbench.  Importing :mod:`mixrec` therefore works on a
headless machine that only has NumPy installed.
"""

from .errors import MixError
from .models import MixConfig, TrackSpec
from .engine import MixEngine, RunResult
from .index import verify_index_file

__all__ = [
    "MixConfig",
    "MixEngine",
    "MixError",
    "RunResult",
    "TrackSpec",
    "verify_index_file",
]

__version__ = "1.0.0"
