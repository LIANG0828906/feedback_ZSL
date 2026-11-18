"""Business errors and validation issue helpers."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass(frozen=True)
class Issue:
    code: str
    file: str
    message: str

    def as_dict(self) -> dict[str, str]:
        return {"code": self.code, "file": self.file, "message": self.message}


@dataclass
class BusinessError(Exception):
    code: str
    file: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        return f"{self.code} [{self.file}]: {self.message}"

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "code": self.code,
            "file": self.file,
            "message": self.message,
        }
        if self.details:
            result["details"] = self.details
        return result


class ManifestError(BusinessError):
    def __init__(self, manifest_path: str, issues: list[Issue]):
        super().__init__(
            code="E_MANIFEST_INVALID",
            file=manifest_path,
            message=f"manifest validation failed with {len(issues)} issue(s)",
            details={"issues": [issue.as_dict() for issue in issues]},
        )
        self.issues = issues
