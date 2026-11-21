"""Unified error codes for the loudness metering pipeline."""


class MeterError(Exception):
    """Base error; ``code`` is a stable machine-readable error code."""

    code = "E_INTERNAL"

    def __init__(self, message: str = ""):
        super().__init__(f"{self.code}: {message}" if message else self.code)
        self.message = message


class WavInvalidError(MeterError):
    """Header fields contradictory, channels missing or bit depth mismatch."""

    code = "E_WAV_INVALID"


class BusyError(MeterError):
    """The same asset was submitted for metering twice."""

    code = "E_BUSY"


class VersionStaleError(MeterError):
    """Version hash carried by a query does not match the publish manifest."""

    code = "E_VERSION_STALE"


class StaleHandleError(MeterError):
    """Query handle has been retired."""

    code = "E_STALE_HANDLE"


class ReportMismatchError(MeterError):
    """Snapshot hash chain broken or required fields missing."""

    code = "E_REPORT_MISMATCH"
