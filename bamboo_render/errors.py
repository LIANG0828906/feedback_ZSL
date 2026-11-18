from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class SceneError(Exception):
    code: str
    message: str
    layer: int | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        suffix = f" (layer {self.layer})" if self.layer is not None else ""
        return f"{self.code}: {self.message}{suffix}"

    def as_dict(self) -> dict[str, Any]:
        error: dict[str, Any] = {
            "code": self.code,
            "layer": self.layer,
            "reason": self.message,
        }
        for key in sorted(self.details):
            error[key] = self.details[key]
        return {"ok": False, "stage": "validation", "error": error}


class SnapshotInvalid(Exception):
    def __init__(self, code: str, reason: str, block_id: int | None = None) -> None:
        super().__init__(reason)
        self.code = code
        self.reason = reason
        self.block_id = block_id


class RenderInterrupted(Exception):
    def __init__(self, completed_blocks: int, total_blocks: int) -> None:
        super().__init__("requested deterministic interruption")
        self.completed_blocks = completed_blocks
        self.total_blocks = total_blocks
