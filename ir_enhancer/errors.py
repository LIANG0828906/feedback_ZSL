"""统一错误码与异常定义。"""


class EnhancerError(Exception):
    """所有业务错误的基类，携带错误码与结构化上下文。"""

    code = "E_UNKNOWN"

    def __init__(self, message="", **context):
        super().__init__(message or self.code)
        self.message = message or self.code
        self.context = context

    def to_dict(self):
        return {"code": self.code, "message": self.message, "context": self.context}


class WavFormatError(EnhancerError):
    code = "E_WAV_FORMAT"


class NoiseEstInvalidError(EnhancerError):
    code = "E_NOISE_EST_INVALID"


class OnsetProtectedError(EnhancerError):
    code = "E_ONSET_PROTECTED"


class OnsetShiftError(EnhancerError):
    code = "E_ONSET_SHIFT"


class OnsetGainError(EnhancerError):
    code = "E_ONSET_GAIN"


class BusyError(EnhancerError):
    code = "E_BUSY"


class RecoveryInvalidError(EnhancerError):
    code = "E_RECOVERY_INVALID"


class CancelledError(EnhancerError):
    code = "E_CANCELLED"
