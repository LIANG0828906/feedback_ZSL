"""Business error codes shared by every pipeline stage."""

from __future__ import annotations


class ErrorCode:
    E_MANIFEST_INVALID = "E_MANIFEST_INVALID"
    E_TRANSIENT_NOT_FOUND = "E_TRANSIENT_NOT_FOUND"
    E_RESUME_MISMATCH = "E_RESUME_MISMATCH"


class BusinessError(Exception):
    """Raised for fatal business-rule violations.

    ``warnings`` always carries structured records; every record includes the
    offending file name (or ``"<manifest>"`` when no file is involved).
    """

    def __init__(self, code: str, warnings: list[dict], message: str = ""):
        self.code = code
        self.warnings = warnings
        super().__init__(message or code)
