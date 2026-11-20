"""Stable business errors used by both the library and the demo CLI."""

from __future__ import annotations

from typing import Any, Mapping


class BusinessError(Exception):
    """An expected failure that is safe to expose to an integration caller."""

    code = "E_INTERNAL"

    def __init__(
        self,
        detail: str,
        message: str,
        *,
        field: str | None = None,
        offset: int | None = None,
        inject: str | None = None,
        injection_point: str | None = None,
        usage: Mapping[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.detail = detail
        self.message = message
        self.field = field
        self.offset = offset
        self.inject = inject
        self.injection_point = injection_point
        self.usage = dict(usage) if usage is not None else None

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "code": self.code,
            "detail": self.detail,
            "event": "error",
            "message": self.message,
        }
        if self.field is not None:
            result["field"] = self.field
        if self.offset is not None:
            result["offset"] = int(self.offset)
        if self.inject is not None:
            result["inject"] = self.inject
        if self.injection_point is not None:
            result["injection_point"] = self.injection_point
        if self.usage is not None:
            result["usage"] = self.usage
        return result


class VolumeInvalidError(BusinessError):
    code = "E_VOLUME_INVALID"


class TransferFunctionError(BusinessError):
    code = "E_TF_INVALID"


class BudgetExceededError(BusinessError):
    code = "E_BUDGET_EXCEEDED"


class SnapshotMismatchError(BusinessError):
    code = "E_SNAPSHOT_MISMATCH"


class RenderBusyError(BusinessError):
    code = "E_BUSY"


class RenderCancelledError(BusinessError):
    code = "E_CANCELLED"


class ResourceError(BusinessError):
    code = "E_RESOURCE"

