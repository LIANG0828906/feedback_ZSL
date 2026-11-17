"""Application error types and stable business codes."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class MasterError(Exception):
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)
    exit_code: int = 2

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"


class ProjectInvalid(MasterError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_PROJECT_INVALID", message, details, 2)


class MemoryLimit(MasterError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_MEMORY_LIMIT", message, details, 2)


class CheckpointCorrupt(MasterError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_CHECKPOINT_CORRUPT", message, details, 2)


class RenderFailed(MasterError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_RENDER_FAILED", message, details, 1)
