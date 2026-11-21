"""Structured error types whose payloads are useful in logs and tests."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class MotionReviewError(Exception):
    code: str
    message: str
    clip_id: str | None = None
    frame: int | None = None
    joint: str | None = None
    constraint_id: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    @property
    def error_code(self) -> str:
        return self.code

    def __str__(self) -> str:
        where: list[str] = []
        if self.clip_id is not None:
            where.append(f"clip={self.clip_id}")
        if self.frame is not None:
            where.append(f"frame={self.frame}")
        if self.joint is not None:
            where.append(f"joint={self.joint}")
        if self.constraint_id is not None:
            where.append(f"constraint={self.constraint_id}")
        suffix = (" [" + ", ".join(where) + "]") if where else ""
        return f"{self.code}: {self.message}{suffix}"

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "clip_id": self.clip_id,
            "frame": self.frame,
            "joint": self.joint,
            "constraint_id": self.constraint_id,
            "details": self.details,
        }


class BVHInvalidError(MotionReviewError):
    def __init__(
        self,
        message: str,
        *,
        frame: int | None = None,
        joint: str | None = None,
        clip_id: str | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(
            "E_BVH_INVALID",
            message,
            clip_id=clip_id,
            frame=frame,
            joint=joint,
            details=details or {},
        )


class BudgetExceededError(MotionReviewError):
    def __init__(
        self,
        message: str,
        *,
        clip_id: str | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(
            "E_BUDGET_EXCEEDED",
            message,
            clip_id=clip_id,
            details=details or {},
        )


class BusyError(MotionReviewError):
    def __init__(self, message: str, *, clip_id: str | None = None) -> None:
        super().__init__("E_BUSY", message, clip_id=clip_id)


class CommitFailedError(MotionReviewError):
    def __init__(
        self,
        message: str,
        *,
        clip_id: str | None = None,
        frame: int | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(
            "E_COMMIT_FAILED",
            message,
            clip_id=clip_id,
            frame=frame,
            details=details or {},
        )


class RecoveryInvalidError(MotionReviewError):
    def __init__(
        self,
        message: str,
        *,
        clip_id: str | None = None,
        frame: int | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(
            "E_RECOVERY_INVALID",
            message,
            clip_id=clip_id,
            frame=frame,
            details=details or {},
        )
