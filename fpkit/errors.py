"""Typed error codes used across the fingerprint pipeline."""


class FpError(Exception):
    code = "E_UNKNOWN"

    def __init__(self, message="", field=None, sample_id=None, **ctx):
        super().__init__(message or self.code)
        self.message = message or self.code
        self.field = field
        self.sample_id = sample_id
        self.ctx = ctx

    def to_dict(self):
        out = {"error": self.code, "message": self.message}
        if self.field is not None:
            out["field"] = self.field
        if self.sample_id is not None:
            out["sample_id"] = self.sample_id
        out.update(self.ctx)
        return out


class AudioUnsupported(FpError):
    code = "E_AUDIO_UNSUPPORTED"


class Busy(FpError):
    code = "E_BUSY"


class BudgetExceeded(FpError):
    code = "E_BUDGET_EXCEEDED"


class LibMismatch(FpError):
    code = "E_LIB_MISMATCH"


class AlignMismatch(FpError):
    code = "E_ALIGN_MISMATCH"


class RecoveryInvalid(FpError):
    code = "E_RECOVERY_INVALID"
