from __future__ import annotations

from typing import Any


class EdgeForgeError(RuntimeError):
    """Base application error carrying a stable machine-readable code."""

    code = "E_APP"

    def __init__(
        self,
        message: str,
        *,
        field: str | None = None,
        block_id: int | None = None,
        stage: str | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.message = message
        self.field = field
        self.block_id = block_id
        self.stage = stage
        self.details = details or {}

    def as_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
        }
        if self.field is not None:
            payload["field"] = self.field
        if self.block_id is not None:
            payload["block_id"] = self.block_id
        if self.stage is not None:
            payload["stage"] = self.stage
        if self.details:
            payload["details"] = self.details
        return payload

    def __str__(self) -> str:
        parts = [f"[{self.code}] {self.message}"]
        if self.field:
            parts.append(f"field={self.field}")
        if self.block_id is not None:
            parts.append(f"block={self.block_id}")
        if self.stage:
            parts.append(f"stage={self.stage}")
        return " ".join(parts)


class DecodeError(EdgeForgeError):
    code = "E_DECODE"


class MetaError(EdgeForgeError):
    code = "E_META"


class ParameterError(EdgeForgeError):
    code = "E_PARA"


class StateError(EdgeForgeError):
    code = "E_STATE"


class BandError(EdgeForgeError):
    code = "E_BAND"


class BudgetError(EdgeForgeError):
    code = "E_BUDGET"


class CheckpointError(EdgeForgeError):
    code = "E_CHECKPOINT"


class CancelledError(EdgeForgeError):
    code = "E_CANCEL"


class PipelineFailure(EdgeForgeError):
    code = "E_PIPELINE"

    def __init__(
        self,
        message: str,
        *,
        failures: list[dict[str, Any]] | None = None,
        stage: str | None = None,
    ) -> None:
        super().__init__(message, stage=stage, details={"failures": failures or []})
        self.failures = failures or []

