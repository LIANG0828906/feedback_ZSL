"""Business errors with stable machine-readable codes."""

from __future__ import annotations

from typing import Any


class BusinessError(Exception):
    code = "E_BUSINESS"

    def __init__(self, message: str, **details: Any) -> None:
        super().__init__(message)
        self.message = message
        self.details = details

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"code": self.code, "message": self.message}
        result.update(self.details)
        return result


class PointCloudInvalid(BusinessError):
    code = "E_PC_INVALID"

    def __init__(
        self,
        message: str,
        *,
        point_index: int | None = None,
        **details: Any,
    ) -> None:
        if point_index is not None:
            details["point_index"] = point_index
        super().__init__(message, **details)
        self.point_index = point_index


class SelectionInvalid(BusinessError):
    code = "E_SELECTION_INVALID"


class OperatorInvalid(BusinessError):
    code = "E_OP_INVALID"


class CommitFailed(BusinessError):
    code = "E_COMMIT_FAILED"


class OperationCancelled(BusinessError):
    code = "E_CANCELLED"


class RepositoryInvalid(BusinessError):
    code = "E_REPOSITORY_INVALID"


class EditBusy(BusinessError):
    code = "E_BUSY"
