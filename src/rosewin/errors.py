"""Structured validation errors and their stable business codes."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass(frozen=True)
class ValidationIssue:
    """A single deterministic scene validation problem."""

    code: str
    reason: str
    quadrant: int | None = None
    facet: int | None = None

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {"code": self.code, "reason": self.reason}
        if self.quadrant is not None:
            payload["quadrant"] = self.quadrant
        if self.facet is not None:
            payload["facet"] = self.facet
        return payload


class SceneValidationError(Exception):
    """Raised when the JSON scene fails deterministic structural validation."""

    def __init__(self, issues: list[ValidationIssue]):
        self.issues = issues
        codes = sorted({issue.code for issue in issues})
        super().__init__("scene validation failed: " + ",".join(codes))

    def to_payload(self) -> dict[str, Any]:
        return {
            "schema_version": "cathedral-rose-error/v1",
            "status": "invalid_scene",
            "issue_count": len(self.issues),
            "issues": [issue.to_dict() for issue in self.issues],
        }


@dataclass
class PixelDifference:
    """Structured per-pixel mismatch used by the recovery verifier."""

    chunk_id: str
    pixel: list[int]
    channel: str
    baseline: int
    recovered: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "chunk_id": self.chunk_id,
            "pixel_xy": self.pixel,
            "channel": self.channel,
            "baseline": self.baseline,
            "recovered": self.recovered,
        }


@dataclass
class RecoveryMismatchError(Exception):
    differences: list[PixelDifference] = field(default_factory=list)

    def __post_init__(self) -> None:
        super().__init__(f"recovery mismatch with {len(self.differences)} differing pixels")

    def to_payload(self) -> dict[str, Any]:
        return {
            "schema_version": "cathedral-rose-error/v1",
            "status": "recovery_mismatch",
            "difference_count": len(self.differences),
            "differences": [item.to_dict() for item in self.differences],
        }
