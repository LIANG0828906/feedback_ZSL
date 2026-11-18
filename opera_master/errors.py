"""Structured business errors and process exit codes."""

# Business codes mandated by the task brief.
FILE_MISSING = "FILE_MISSING"
SAMPLE_RATE_MISMATCH = "SAMPLE_RATE_MISMATCH"
BIT_DEPTH_MISMATCH = "BIT_DEPTH_MISMATCH"
CHANNEL_COUNT_MISMATCH = "CHANNEL_COUNT_MISMATCH"
DUPLICATE_PART_NAME = "DUPLICATE_PART_NAME"
BED_CHANNEL_MISSING = "BED_CHANNEL_MISSING"
PART_MISSING = "PART_MISSING"
REF_TIMES_NOT_ASCENDING = "REF_TIMES_NOT_ASCENDING"
INVALID_CUE = "INVALID_CUE"
DETECTION_FAILED = "DETECTION_FAILED"
E_RESUME_MISMATCH = "E_RESUME_MISMATCH"

# Non-zero integer exit codes.
EXIT_BUSINESS_ERROR = 2
EXIT_RESUME_MISMATCH = 3


class OperaError(Exception):
    """Base error carrying a structured, JSON-serialisable payload."""

    code = INVALID_CUE
    exit_code = EXIT_BUSINESS_ERROR

    def __init__(self, message, code=None, channel=None, details=None):
        super().__init__(message)
        if code is not None:
            self.code = code
        self.channel = channel
        self.details = details or {}

    def to_payload(self):
        payload = {"code": self.code, "message": str(self)}
        if self.channel is not None:
            payload["channel"] = self.channel
        if self.details:
            payload["details"] = self.details
        return payload


class CueValidationError(OperaError):
    """One or more cue-sheet / WAV validation failures."""

    code = INVALID_CUE

    def __init__(self, errors):
        self.errors = list(errors)
        message = "; ".join(
            f"[{e.get('code')}] {e.get('channel', '-')}: {e.get('message')}"
            for e in self.errors
        )
        super().__init__(message or "cue validation failed")

    def to_payload(self):
        return {"code": INVALID_CUE, "message": str(self), "errors": self.errors}


class ResumeMismatchError(OperaError):
    """Committed snapshot no longer matches the current inputs."""

    code = E_RESUME_MISMATCH
    exit_code = EXIT_RESUME_MISMATCH


def make_error(code, message, channel=None, details=None):
    payload = {"code": code, "message": message}
    if channel is not None:
        payload["channel"] = channel
    if details:
        payload["details"] = details
    return payload
