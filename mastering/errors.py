from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable


EXIT_VALIDATION = 2
EXIT_RESUME_MISMATCH = 3
EXIT_RENDER = 4
EXIT_INTERRUPTED = 0


@dataclass(frozen=True)
class BusinessError:
    code: str
    message: str
    track: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        value: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.track is not None:
            value["track"] = self.track
        if self.details:
            value["details"] = self.details
        return value


class BusinessFailure(Exception):
    def __init__(self, errors: Iterable[BusinessError], exit_code: int) -> None:
        self.errors = list(errors)
        self.exit_code = exit_code
        super().__init__("; ".join(error.message for error in self.errors))


def fail(code: str, message: str, *, track: str | None = None, exit_code: int = EXIT_RENDER, **details: Any) -> None:
    raise BusinessFailure([BusinessError(code, message, track, details)], exit_code)
