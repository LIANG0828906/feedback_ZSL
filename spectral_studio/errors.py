"""Stable error codes surfaced by every module."""


class ErrorCode:
    E_SPECTRUM_INVALID = "E_SPECTRUM_INVALID"
    E_COLOR_INVALID = "E_COLOR_INVALID"
    E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
    E_BUSY = "E_BUSY"
    E_STALE_SNAPSHOT = "E_STALE_SNAPSHOT"


class SpectralStudioError(Exception):
    """Exception carrying a stable machine-readable error code."""

    def __init__(self, code, message, detail=None):
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message
        self.detail = detail or {}
