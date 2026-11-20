"""业务错误码与统一异常类型。"""

E_INPUT_INVALID = "E_INPUT_INVALID"
E_QUOTA_EXCEEDED = "E_QUOTA_EXCEEDED"
E_CHECKPOINT_WRITE = "E_CHECKPOINT_WRITE"
E_RESUME_INVALID = "E_RESUME_INVALID"
E_BASELINE_VERSION = "E_BASELINE_VERSION"
E_CANCELLED = "E_CANCELLED"


class ReplayError(Exception):
    """携带业务错误码与结构化上下文的异常。"""

    def __init__(self, code, message, **context):
        super().__init__(message)
        self.code = code
        self.message = message
        self.context = context

    def __str__(self):
        if self.context:
            detail = ", ".join(f"{k}={v!r}" for k, v in self.context.items())
            return f"{self.code}: {self.message} ({detail})"
        return f"{self.code}: {self.message}"
