"""Pipeline error codes and exception hierarchy.

Every failure raised by the pipeline is an :class:`AFPipeError` carrying a
stable machine readable ``code`` so callers can branch without parsing text.
"""

from __future__ import annotations


# Stable error codes referenced by the CLI, tests and technical report.
E_META = "E_META"          # manifest field disagrees with decoded reality
E_DECODE = "E_DECODE"      # ffmpeg could not decode a source file
E_PARA = "E_PARA"          # invalid / out of range configuration parameter
E_VER = "E_VER"            # index format or parameter version mismatch
E_SHORT = "E_SHORT"        # query shorter than minimum searchable length
E_BUDGET = "E_BUDGET"      # query exceeds configured resource budget
E_SCHEMA = "E_SCHEMA"      # manifest malformed or unreadable
E_CHAN = "E_CHAN"          # channel count exceeds declared cap
E_CORRUPT = "E_CORRUPT"    # on-disk index/checkpoint corruption
E_INTERNAL = "E_INTERNAL"  # unexpected per-file processing error


class AFPipeError(Exception):
    """Base class for all pipeline errors."""

    code = "E_INTERNAL"

    def __init__(self, message: str = "", *, details: dict | None = None):
        super().__init__(message or self.code)
        self.message = message or self.code
        self.details = details or {}

    def to_dict(self) -> dict:
        out = {"code": self.code, "message": self.message}
        if self.details:
            out["details"] = self.details
        return out


class MetaError(AFPipeError):
    code = E_META


class DecodeError(AFPipeError):
    code = E_DECODE


class ParameterError(AFPipeError):
    code = E_PARA


class VersionError(AFPipeError):
    code = E_VER


class ShortQueryError(AFPipeError):
    code = E_SHORT


class BudgetError(AFPipeError):
    code = E_BUDGET


class SchemaError(AFPipeError):
    code = E_SCHEMA


class ChannelError(AFPipeError):
    code = E_CHAN


class CorruptionError(AFPipeError):
    code = E_CORRUPT


class InternalError(AFPipeError):
    code = E_INTERNAL


CODE_TO_EXCEPTION: dict[str, type[AFPipeError]] = {
    cls.code: cls
    for cls in (
        MetaError,
        DecodeError,
        ParameterError,
        VersionError,
        ShortQueryError,
        BudgetError,
        SchemaError,
        ChannelError,
        CorruptionError,
        InternalError,
    )
}
