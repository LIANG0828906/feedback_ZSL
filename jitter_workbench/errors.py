"""Error codes and exceptions for the jitter workbench.

Every recoverable failure surfaces as one of these codes so that the
realtime / resume / offline paths report identical failure semantics.
"""
from __future__ import annotations

E_META = "E_META"      # manifest/metadata validation failure
E_JITTER = "E_JITTER"  # unknown jitter pattern tag, whole sample rejected
E_PARA = "E_PARA"      # controller parameter out of range
E_DECODE = "E_DECODE"  # frame stream corrupted / undecodable
E_ORDER = "E_ORDER"    # media timestamp inversion in arrival stream
E_DUP = "E_DUP"        # duplicate frames beyond tolerance
E_DIFF = "E_DIFF"      # three-path consistency mismatch


class WorkbenchError(Exception):
    """Base error carrying a stable machine-readable code."""

    code = "E_UNKNOWN"

    def __init__(self, message, *, field=None, detail=None):
        super().__init__(message)
        self.field = field
        self.detail = detail or {}

    def to_dict(self):
        return {
            "code": self.code,
            "message": str(self),
            "field": self.field,
            "detail": self.detail,
        }


class MetaError(WorkbenchError):
    code = E_META


class JitterError(WorkbenchError):
    code = E_JITTER


class ParamError(WorkbenchError):
    code = E_PARA


class DecodeError(WorkbenchError):
    code = E_DECODE


class OrderError(WorkbenchError):
    code = E_ORDER


class DupError(WorkbenchError):
    code = E_DUP


class DiffError(WorkbenchError):
    code = E_DIFF
