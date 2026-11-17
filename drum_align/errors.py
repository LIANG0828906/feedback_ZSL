"""Structured business errors used by both the API and the CLI."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class BusinessError(Exception):
    code: str
    reason: str
    camera: str | None = None
    severity: str = "hard"

    def to_dict(self) -> dict[str, Any]:
        item: dict[str, Any] = {
            "code": self.code,
            "severity": self.severity,
            "camera": self.camera,
            "reason": self.reason,
        }
        return item

    def __str__(self) -> str:
        camera = f" camera={self.camera}" if self.camera else ""
        return f"{self.code}{camera}: {self.reason}"
