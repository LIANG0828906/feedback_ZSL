"""Structured business errors with stable error codes."""


class BusinessError(Exception):
    """Raised for every expected, reportable business validation failure."""

    def __init__(self, code, message, details=None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details or {}

    def to_dict(self):
        return {"code": self.code, "message": self.message, "details": self.details}


FILE_MISSING = "FILE_MISSING"
SAMPLE_RATE_MISMATCH = "SAMPLE_RATE_MISMATCH"
CHANNEL_COUNT_MISMATCH = "CHANNEL_COUNT_MISMATCH"
DUPLICATE_MIC_NAME = "DUPLICATE_MIC_NAME"
INVALID_FREQ_BAND = "INVALID_FREQ_BAND"
INVALID_PROJECT = "INVALID_PROJECT"
UNSUPPORTED_SAMPLE_RATE = "UNSUPPORTED_SAMPLE_RATE"
E_RESUME_MISMATCH = "E_RESUME_MISMATCH"
