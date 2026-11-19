"""Stable business errors exposed by the executor."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class ExecutorError(Exception):
    """An error that is safe to surface as a stable business result."""

    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"


ERROR_RETRY_EXHAUSTED = "E_PARTITION_RETRY_EXHAUSTED"
ERROR_MEMORY_LIMIT = "E_MEMORY_LIMIT"
ERROR_TEMP_LIMIT = "E_TEMP_LIMIT"
ERROR_IO = "E_IO"
ERROR_SQL = "E_SQL_UNSUPPORTED"
ERROR_CONFIG = "E_CONFIG_INVALID"
ERROR_PARTIAL_RESULT = "E_PARTIAL_RESULT_FORBIDDEN"
ERROR_INTERNAL = "E_INTERNAL"

# Process exit codes are intentionally small and stable for Windows shells.
EXIT_CODES = {
    ERROR_RETRY_EXHAUSTED: 20,
    ERROR_MEMORY_LIMIT: 21,
    ERROR_TEMP_LIMIT: 22,
    ERROR_IO: 23,
    ERROR_SQL: 24,
    ERROR_CONFIG: 25,
    ERROR_PARTIAL_RESULT: 26,
    ERROR_INTERNAL: 1,
}

