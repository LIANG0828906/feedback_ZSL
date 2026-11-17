"""Structured business errors.

Every failure that must abort the pipeline without emitting a partial bus is
reported as a PipelineError carrying a stable business code.  The CLI
serialises it to stderr as JSON, e.g.::

    {"error": {"code": "SAMPLE_RATE_MISMATCH", "channel": "bass", "message": "..."}}
"""

from __future__ import annotations

import json
import sys


class PipelineError(Exception):
    """Business-level failure with a stable machine-readable code."""

    def __init__(self, code: str, message: str, channel: str | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.channel = channel

    def to_dict(self) -> dict:
        payload = {"code": self.code, "message": self.message}
        if self.channel is not None:
            payload["channel"] = self.channel
        return {"error": payload}


# Validation codes
PROJECT_FILE_MISSING = "PROJECT_FILE_MISSING"
PROJECT_PARSE_ERROR = "PROJECT_PARSE_ERROR"
PROJECT_SCHEMA_ERROR = "PROJECT_SCHEMA_ERROR"
CHANNEL_FILE_MISSING = "CHANNEL_FILE_MISSING"
SAMPLE_RATE_MISMATCH = "SAMPLE_RATE_MISMATCH"
CHANNEL_COUNT_MISMATCH = "CHANNEL_COUNT_MISMATCH"
DUPLICATE_CHANNEL_NAME = "DUPLICATE_CHANNEL_NAME"
CHANNEL_ROLE_MISSING = "CHANNEL_ROLE_MISSING"
CHANNEL_ROLE_INVALID = "CHANNEL_ROLE_INVALID"
REFERENCE_MISSING = "REFERENCE_MISSING"

# Bus rendering codes
BUS_LOUDNESS_OUT_OF_TOLERANCE = "BUS_LOUDNESS_OUT_OF_TOLERANCE"
BUS_TRUE_PEAK_EXCEEDED = "BUS_TRUE_PEAK_EXCEEDED"

# Resume codes
E_RESUME_MISMATCH = "E_RESUME_MISMATCH"


def emit_error(err: PipelineError) -> None:
    """Write the structured error to stderr (single line JSON)."""
    sys.stderr.write(json.dumps(err.to_dict(), ensure_ascii=False, sort_keys=True) + "\n")
