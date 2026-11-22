"""Error codes and the single exception type used across the component.

Every abnormal path raises :class:`VolViewError` carrying a stable machine
readable ``code`` and the offending ``field`` (parameter name, manifest field
or annotation id) so callers can route errors instead of silently producing
plausible-but-wrong measurements.
"""

from __future__ import annotations


class ErrorCode:
    E_AXIS = "E_AXIS"        # axis-order declaration conflicts with data dims
    E_CAL = "E_CAL"          # calibration / spacing declaration invalid
    E_WIN = "E_WIN"          # window width/level out of valid range
    E_THICK = "E_THICK"      # slab thickness out of available range
    E_RANGE = "E_RANGE"      # annotation endpoint / plane position out of volume
    E_DECODE = "E_DECODE"    # volume payload cannot be decoded (truncated etc.)
    E_ACC = "E_ACC"          # measured deviation exceeds derived bound
    E_STATE = "E_STATE"      # snapshot / session state inconsistency


class VolViewError(Exception):
    """Component error with a stable code and the offending field."""

    def __init__(self, code: str, field: str, message: str, **details):
        self.code = code
        self.field = field
        self.details = details
        super().__init__(f"[{code}] {field}: {message}")

    def to_dict(self) -> dict:
        return {
            "code": self.code,
            "field": self.field,
            "message": str(self),
            "details": self.details,
        }
