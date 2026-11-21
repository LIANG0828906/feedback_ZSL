class BakeError(Exception):
    """Base error carrying a stable machine-readable code."""

    code = "E_UNKNOWN"

    def __init__(self, message="", **context):
        super().__init__(message or self.code)
        self.context = context

    def __str__(self):
        base = super().__str__()
        if self.context:
            ctx = ", ".join(f"{k}={v!r}" for k, v in sorted(self.context.items()))
            return f"{self.code}: {base} ({ctx})"
        return f"{self.code}: {base}"


class SpectrumInvalid(BakeError):
    code = "E_SPECTRUM_INVALID"


class BusyError(BakeError):
    code = "E_BUSY"


class BudgetExceeded(BakeError):
    code = "E_BUDGET_EXCEEDED"


class OrderNondeterminism(BakeError):
    code = "E_ORDER_NONDETERMIN"


class RecoveryInvalid(BakeError):
    code = "E_RECOVERY_INVALID"
