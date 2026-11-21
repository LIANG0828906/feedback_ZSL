"""Stable machine-readable error codes used by the CLI and GUI."""

from __future__ import annotations

from typing import Any, Mapping

E_GEOMETRY_INVALID = "E_GEOMETRY_INVALID"
E_POLARITY_UNKNOWN = "E_POLARITY_UNKNOWN"
E_CALIB_INFEASIBLE = "E_CALIB_INFEASIBLE"
E_BUSY = "E_BUSY"
E_MEASUREMENT_INVALID = "E_MEASUREMENT_INVALID"
E_CONFIG_INVALID = "E_CONFIG_INVALID"
E_CANCELLED = "E_CANCELLED"
E_CACHE_INVALID = "E_CACHE_INVALID"


class VenueError(Exception):
    """An expected failure with a stable code and structured location data."""

    def __init__(
        self,
        code: str,
        message: str,
        details: Mapping[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = dict(details or {})

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "details": self.details,
        }

    def __str__(self) -> str:
        if not self.details:
            return f"{self.code}: {self.message}"
        location = ", ".join(f"{key}={value!r}" for key, value in self.details.items())
        return f"{self.code}: {self.message} ({location})"
