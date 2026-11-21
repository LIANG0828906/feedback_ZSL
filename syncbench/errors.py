"""Error codes raised across the synchronization pipeline."""


class SyncError(Exception):
    """Pipeline error carrying a stable machine-readable code."""

    def __init__(self, code, message, details=None):
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message
        self.details = details or {}


E_BUSY = "E_BUSY"
E_DRIFT_UNKNOWN = "E_DRIFT_UNKNOWN"
E_REPORT_MISMATCH = "E_REPORT_MISMATCH"
E_STITCH_MISMATCH = "E_STITCH_MISMATCH"
E_MARKER_LOW_SNR = "E_MARKER_LOW_SNR"
E_CONFIG = "E_CONFIG"
E_CANCELLED = "E_CANCELLED"
