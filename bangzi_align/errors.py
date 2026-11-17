from __future__ import annotations

import json
import sys
from dataclasses import dataclass
from typing import Iterable

from .constants import EXIT_RESUME_MISMATCH, EXIT_VALIDATION


@dataclass(frozen=True)
class Diagnostic:
    code: str
    channel: str
    message: str

    def as_dict(self) -> dict[str, str]:
        return {
            "code": self.code,
            "channel": self.channel,
            "message": self.message,
        }


class DiagnosticError(Exception):
    exit_code = EXIT_VALIDATION

    def __init__(self, diagnostics: Iterable[Diagnostic]):
        self.diagnostics = list(diagnostics)
        super().__init__(self.diagnostics[0].message if self.diagnostics else "error")


class ResumeMismatch(DiagnosticError):
    exit_code = EXIT_RESUME_MISMATCH


def dump_diagnostics(diagnostics: Iterable[Diagnostic], stream=None) -> None:
    if stream is None:
        stream = sys.stderr
    payload = {
        "schema": "bangzi-diagnostic/v1",
        "ok": False,
        "diagnostics": [item.as_dict() for item in diagnostics],
    }
    json.dump(payload, stream, ensure_ascii=False, separators=(",", ":"))
    stream.write("\n")
    stream.flush()

