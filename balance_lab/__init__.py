"""Battle Balance Lab.

Deterministic, fixed-point combat balance validation for local multiplayer
game projects.  The package deliberately keeps simulation, budgeting,
persistence, reporting, and presentation in separate modules.
"""

from .errors import BalanceError, ErrorCode

__all__ = ["BalanceError", "ErrorCode"]
__version__ = "1.0.0"

