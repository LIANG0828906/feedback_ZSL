from __future__ import annotations

from dataclasses import dataclass
from dataclasses import field as dc_field
from enum import StrEnum
from typing import Any


class ErrorCode(StrEnum):
    WAVE_INVALID = "E_WAVE_INVALID"
    CONFIG_INVALID = "E_CONFIG_INVALID"
    BUSY = "E_BUSY"
    STALE_HANDLE = "E_STALE_HANDLE"
    RECOVERY_INVALID = "E_RECOVERY_INVALID"
    CANCELLED = "E_CANCELLED"
    IO = "E_IO"
    NOT_FOUND = "E_NOT_FOUND"


@dataclass
class HapticError(Exception):
    code: ErrorCode | str
    message: str
    segment_index: int | None = None
    field: str | None = None
    version: int | None = None
    details: dict[str, Any] = dc_field(default_factory=dict)

    def __post_init__(self) -> None:
        if not isinstance(self.code, ErrorCode):
            try:
                self.code = ErrorCode(self.code)
            except ValueError:
                pass
        super().__init__(self.message)

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": str(self.code),
            "message": self.message,
        }
        if self.segment_index is not None:
            payload["segment_index"] = self.segment_index
        if self.field is not None:
            payload["field"] = self.field
        if self.version is not None:
            payload["version"] = self.version
        if self.details:
            payload["details"] = self.details
        return payload

    def __str__(self) -> str:
        location: list[str] = []
        if self.segment_index is not None:
            location.append(f"segment={self.segment_index}")
        if self.field is not None:
            location.append(f"field={self.field}")
        if self.version is not None:
            location.append(f"version={self.version}")
        suffix = f" ({', '.join(location)})" if location else ""
        return f"{self.code}: {self.message}{suffix}"


def wave_error(
    message: str,
    *,
    segment_index: int | None = None,
    field_name: str | None = None,
) -> HapticError:
    return HapticError(
        ErrorCode.WAVE_INVALID,
        message,
        segment_index=segment_index,
        field=field_name,
    )


def config_error(message: str, *, field_name: str | None = None) -> HapticError:
    return HapticError(ErrorCode.CONFIG_INVALID, message, field=field_name)
