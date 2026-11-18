"""Stable business error codes for the job."""


class EngineError(Exception):
    """Fatal job error carrying a stable business error code."""

    code = "E_ENGINE"
    exit_code = 70


class StateError(EngineError):
    """State database locked, corrupted or otherwise unusable."""

    code = "E_STATE"
    exit_code = 70


class MemoryLimitError(EngineError):
    """Job exceeded its configured memory ceiling."""

    code = "E_MEM"
    exit_code = 71


class InputError(EngineError):
    """Input data does not match the persisted state fingerprint."""

    code = "E_INPUT"
    exit_code = 72
