from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable


@dataclass(frozen=True)
class ValidationIssue:
    code: str
    layer_index: int | None
    reason: str

    def as_dict(self) -> dict:
        return {
            "code": self.code,
            "layer_index": self.layer_index,
            "reason": self.reason,
        }


class SceneValidationError(Exception):
    """A structured business validation failure."""

    def __init__(self, issues: Iterable[ValidationIssue]):
        self.issues = list(issues)
        super().__init__(f"scene validation failed with {len(self.issues)} issue(s)")


class SnapshotError(Exception):
    pass


class RenderMismatchError(Exception):
    def __init__(self, differences: list[dict], total: int):
        self.differences = differences
        self.total = total
        super().__init__(f"resume mismatch in {total} output component(s)")

