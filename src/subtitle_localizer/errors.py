from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping


E_BUSY = "E_BUSY"
E_TIMEBASE_INVALID = "E_TIMEBASE_INVALID"
E_DESC_CONFLICT = "E_DESC_CONFLICT"
E_LAYOUT_MISMATCH = "E_LAYOUT_MISMATCH"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_CANCELLED = "E_CANCELLED"
E_INVALID_INPUT = "E_INVALID_INPUT"


@dataclass
class BusinessError(Exception):
    """A stable, serialisable application error.

    The command line and the Qt workbench both report the same business code
    and context dictionary.  Callers should not have to parse human text.
    """

    code: str
    message: str
    context: Mapping[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)
        object.__setattr__(self, "context", dict(self.context))

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "context": dict(self.context),
            "message": self.message,
        }


class CancelledError(BusinessError):
    def __init__(self, message: str = "operation cancelled", context: Mapping[str, Any] | None = None):
        super().__init__(E_CANCELLED, message, context or {})


def timebase_error(message: str, *, sentence_id: str | None = None, field: str | None = None,
                   value: Any = None, **extra: Any) -> BusinessError:
    context: dict[str, Any] = {}
    if sentence_id is not None:
        context["sentence_id"] = sentence_id
    if field is not None:
        context["field"] = field
    if value is not None:
        context["value"] = value
    context.update(extra)
    return BusinessError(E_TIMEBASE_INVALID, message, context)

