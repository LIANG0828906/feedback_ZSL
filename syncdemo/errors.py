"""Error codes surfaced by the sync demo toolkit.

Every domain failure raises :class:`SyncError` (or a subclass) carrying a
stable machine-readable ``code`` plus optional context attributes
(``field`` / ``unit`` / ``tick``) so callers can localise the problem.
"""

from __future__ import annotations


class SyncError(Exception):
    """Base class for all toolkit errors."""

    code = "E_SYNC"

    def __init__(self, message, *, field=None, unit=None, tick=None):
        super().__init__(message)
        self.message = message
        self.field = field
        self.unit = unit
        self.tick = tick

    def __str__(self):  # pragma: no cover - cosmetic
        ctx = []
        if self.field is not None:
            ctx.append(f"field={self.field}")
        if self.unit is not None:
            ctx.append(f"unit={self.unit}")
        if self.tick is not None:
            ctx.append(f"tick={self.tick}")
        suffix = f" ({', '.join(ctx)})" if ctx else ""
        return f"[{self.code}] {self.message}{suffix}"


class SchemaError(SyncError):
    """Sample structure/field validation failure."""

    code = "E_SCHEMA"


class UnitRefError(SyncError):
    """Reference to a unit that does not exist."""

    code = "E_UNIT"


class ParamError(SyncError):
    """Parameter out of its allowed range."""

    code = "E_PARA"


class DivergeError(SyncError):
    """Two execution paths produced different results."""

    code = "E_DIVERGE"
