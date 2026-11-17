"""Business error codes, structured error reporting and process exit codes.

All validation failures are reported as a single structured JSON object on
stderr and the process terminates with a stable, non-zero integer exit code.
"""

from __future__ import annotations

import json
import sys


class BusinessCode:
    FILE_MISSING = "FILE_MISSING"
    FILE_UNREADABLE = "FILE_UNREADABLE"
    SAMPLE_RATE_MISMATCH = "SAMPLE_RATE_MISMATCH"
    BIT_DEPTH_MISMATCH = "BIT_DEPTH_MISMATCH"
    CHANNEL_COUNT_MISMATCH = "CHANNEL_COUNT_MISMATCH"
    DUPLICATE_MIC_NAME = "DUPLICATE_MIC_NAME"
    INVALID_ACTION_SEGMENT = "INVALID_ACTION_SEGMENT"
    INVALID_FREQUENCY_BAND = "INVALID_FREQUENCY_BAND"
    INVALID_MANIFEST = "INVALID_MANIFEST"
    E_RESUME_MISMATCH = "E_RESUME_MISMATCH"
    SNAPSHOT_CORRUPT = "SNAPSHOT_CORRUPT"


EXIT_CODES = {
    BusinessCode.FILE_MISSING: 20,
    BusinessCode.FILE_UNREADABLE: 21,
    BusinessCode.SAMPLE_RATE_MISMATCH: 22,
    BusinessCode.BIT_DEPTH_MISMATCH: 23,
    BusinessCode.CHANNEL_COUNT_MISMATCH: 24,
    BusinessCode.DUPLICATE_MIC_NAME: 25,
    BusinessCode.INVALID_ACTION_SEGMENT: 26,
    BusinessCode.INVALID_FREQUENCY_BAND: 27,
    BusinessCode.INVALID_MANIFEST: 28,
    BusinessCode.E_RESUME_MISMATCH: 30,
    BusinessCode.SNAPSHOT_CORRUPT: 31,
}


class StageNoiseError(Exception):
    """Raised for any deterministic, business-level failure."""

    def __init__(self, code: str, message: str, mic: str | None = None,
                 details: dict | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.mic = mic
        self.details = details or {}

    def to_payload(self) -> dict:
        payload = {
            "status": "error",
            "business_code": self.code,
            "exit_code": EXIT_CODES.get(self.code, 1),
            "message": self.message,
        }
        if self.mic is not None:
            payload["mic_name"] = self.mic
        if self.details:
            payload["details"] = self.details
        return payload


def fail(error: StageNoiseError) -> None:
    """Print the structured error to stderr and exit with the mapped code."""
    json.dump(error.to_payload(), sys.stderr, sort_keys=False,
              ensure_ascii=False, indent=2)
    sys.stderr.write("\n")
    raise SystemExit(EXIT_CODES.get(error.code, 1))
