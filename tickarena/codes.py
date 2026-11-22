"""Stable public status and error codes."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

E_OK = "E_OK"
E_ORDER = "E_ORDER"
E_UNIT = "E_UNIT"
E_COOLDOWN = "E_COOLDOWN"
E_RES = "E_RES"
E_NUM = "E_NUM"
E_DIVERGE = "E_DIVERGE"
E_REPLAY = "E_REPLAY"
E_STATE = "E_STATE"
E_ACTION = "E_ACTION"
E_MOVE = "E_MOVE"
E_RANGE = "E_RANGE"
E_ITEM = "E_ITEM"
E_BUFFER = "E_BUFFER"
E_MEMORY = "E_MEMORY"
E_FINISHED = "E_FINISHED"
E_PAUSED = "E_PAUSED"
E_CANCELLED = "E_CANCELLED"
E_MORE = "E_MORE"
E_ARG = "E_ARG"


@dataclass(frozen=True)
class Result:
    """Small transport-neutral result object used by public APIs."""

    code: str
    message: str = ""
    tick: int | None = None
    rule: str | None = None
    recorded: Any = None
    recomputed: Any = None
    details: dict[str, Any] = field(default_factory=dict)

    @property
    def ok(self) -> bool:
        return self.code == E_OK


class TickArenaError(Exception):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message

