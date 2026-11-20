"""Business error codes surfaced by the replay service."""


class BizError(Exception):
    """Base class for diagnosable business errors."""

    code = "E_UNKNOWN"

    def __init__(self, message="", **context):
        super().__init__(message or self.code)
        self.message = message or self.code
        self.context = context

    def to_dict(self):
        return {"code": self.code, "message": self.message,
                "context": self.context}


class OpLateError(BizError):
    code = "E_OP_LATE"


class OpDuplicateError(BizError):
    code = "E_OP_DUPLICATE"


class OpOutOfBoundsError(BizError):
    code = "E_OP_OUT_OF_BOUNDS"


class OpUnknownPlayerError(BizError):
    code = "E_OP_UNKNOWN_PLAYER"


class SnapshotWriteError(BizError):
    code = "E_SNAPSHOT_WRITE"


class RecoveryInvalidError(BizError):
    code = "E_RECOVERY_INVALID"


class BaselineVersionError(BizError):
    code = "E_BASELINE_VERSION"


class BaselineMissingError(BizError):
    code = "E_BASELINE_MISSING"


class BaselineCorruptError(BizError):
    code = "E_BASELINE_CORRUPT"


class BaselineDivergedError(BizError):
    code = "E_BASELINE_DIVERGED"


class BusyError(BizError):
    code = "E_BUSY"


class CancelledError(BizError):
    code = "E_CANCELLED"


class HardCrash(Exception):
    """Simulated kill -9 / power loss: bypasses every cleanup/report path."""

    def __init__(self, step):
        super().__init__(f"simulated hard crash at step {step}")
        self.step = step
