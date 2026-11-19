"""Stable business error codes shared by every engine entry point."""

from __future__ import annotations


class EngineError(Exception):
    """Base class. ``code`` is the stable business error code."""

    code = "E_INTERNAL"
    exit_code = 70

    def __init__(self, message: str, *, details: dict | None = None):
        super().__init__(message)
        self.details = details or {}


class StateError(EngineError):
    """State backend locked, corrupted or otherwise unusable."""

    code = "E_STATE"
    exit_code = 71


class InputError(EngineError):
    """An input shard / batch cannot be read or fails schema validation."""

    code = "E_INPUT"
    exit_code = 72


class LimitError(EngineError):
    """Hard resource limit (memory / temp space) would be exceeded."""

    code = "E_LIMIT"
    exit_code = 73


class ConfigError(EngineError):
    """Job / fault configuration is invalid."""

    code = "E_CONFIG"
    exit_code = 74
