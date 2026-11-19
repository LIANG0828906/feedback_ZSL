from __future__ import annotations


class SettlementError(RuntimeError):
    """Stable, user-facing executor error."""

    def __init__(self, code: str, message: str, *, details: dict | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details or {}

    def as_dict(self) -> dict:
        return {"code": self.code, "message": self.message, "details": self.details}


class SqlParseError(SettlementError):
    def __init__(self, message: str, *, details: dict | None = None):
        super().__init__("E_SQL", message, details=details)


class DataError(SettlementError):
    def __init__(self, message: str, *, details: dict | None = None):
        super().__init__("E_DATA", message, details=details)


class StateError(SettlementError):
    def __init__(self, message: str, *, details: dict | None = None):
        super().__init__("E_STATE", message, details=details)


class MemoryLimitError(SettlementError):
    def __init__(self, message: str, *, details: dict | None = None):
        super().__init__("E_MEMORY", message, details=details)


class PlanError(SettlementError):
    def __init__(self, message: str, *, details: dict | None = None):
        super().__init__("E_PLAN", message, details=details)


class ExecutionStopped(SettlementError):
    def __init__(self, message: str, *, details: dict | None = None):
        super().__init__("E_STOPPED", message, details=details)

