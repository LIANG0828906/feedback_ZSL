"""Business errors and stable JSON diagnostics."""

from __future__ import annotations

import json
import sys
from dataclasses import dataclass
from typing import Any


@dataclass(slots=True)
class BusinessError(Exception):
    code: str
    message: str
    path: str | None = None
    details: dict[str, Any] | None = None

    def __str__(self) -> str:
        suffix = f" at {self.path}" if self.path else ""
        return f"{self.code}: {self.message}{suffix}"

    def as_json(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "ok": False,
            "error": {
                "code": self.code,
                "message": self.message,
                "path": self.path,
            },
        }
        if self.details is not None:
            payload["error"]["details"] = self.details
        return payload


class RenderInterrupted(Exception):
    """A cooperative interruption observed at a committed block boundary."""


def emit_error(error: BusinessError, *, stream: Any = sys.stderr) -> None:
    json.dump(error.as_json(), stream, ensure_ascii=False, sort_keys=False)
    stream.write("\n")
    stream.flush()
