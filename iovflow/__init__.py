"""iovflow - deterministic event-time aggregation for small hosts.

The package intentionally uses only the Python standard library at runtime,
apart from DuckDB which is used as a *parser* (not as the execution engine).
"""

from .errors import EngineError
from .version import __version__

__all__ = ["EngineError", "__version__"]

