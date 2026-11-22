"""Deterministic memory accounting and declared degradation stages."""

from __future__ import annotations

import threading
from dataclasses import dataclass
from typing import Any, Callable

from .errors import LCFTError


@dataclass(frozen=True)
class StageSpec:
    name: str
    row_bytes: int
    text_numerator: int
    text_denominator: int
    text_fixed_per_row: int
    intermediate_bytes: int


DEGRADATION_STAGES: tuple[StageSpec, ...] = (
    StageSpec("full", 320, 1, 1, 0, 512),
    StageSpec("compact_text", 320, 2, 5, 64, 512),
    StageSpec("sparse_precompute", 320, 2, 5, 0, 192),
    StageSpec("minimal", 128, 0, 1, 0, 64),
)


@dataclass(frozen=True)
class Usage:
    stage: str
    stage_index: int
    total_bytes: int
    sentence_count: int
    text_bytes: int
    intermediate_bytes: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "stage": self.stage,
            "stage_index": self.stage_index,
            "total_bytes": self.total_bytes,
            "sentence_count": self.sentence_count,
            "text_bytes": self.text_bytes,
            "intermediate_bytes": self.intermediate_bytes,
        }


def usage_for_stage(
    stage: StageSpec,
    *,
    sentence_count: int,
    text_bytes: int,
    stage_index: int,
) -> Usage:
    retained_text = (
        text_bytes * stage.text_numerator // stage.text_denominator
        + sentence_count * stage.text_fixed_per_row
    )
    total = (
        sentence_count * stage.row_bytes
        + retained_text
        + sentence_count * stage.intermediate_bytes
    )
    return Usage(
        stage=stage.name,
        stage_index=stage_index,
        total_bytes=total,
        sentence_count=sentence_count,
        text_bytes=retained_text,
        intermediate_bytes=sentence_count * stage.intermediate_bytes,
    )


def select_degradation(
    *,
    sentence_count: int,
    text_bytes: int,
    job_quota: int,
    global_quota: int,
) -> Usage:
    attempts: list[dict[str, Any]] = []
    for index, stage in enumerate(DEGRADATION_STAGES):
        usage = usage_for_stage(
            stage,
            sentence_count=sentence_count,
            text_bytes=text_bytes,
            stage_index=index,
        )
        attempts.append(
            {
                "stage": usage.stage,
                "stage_index": index,
                "total_bytes": usage.total_bytes,
                "job_quota": job_quota,
                "global_quota": global_quota,
            }
        )
        if usage.total_bytes <= job_quota and usage.total_bytes <= global_quota:
            usage_dict = usage.to_dict()
            usage_dict["attempts"] = attempts
            if index:
                usage_dict["degraded"] = True
            return UsageWithAttempts(usage, attempts)
    raise LCFTError(
        "E_BUDGET_EXCEEDED",
        "task remains over budget after every declared degradation stage",
        field="job_quota",
        details={
            "attempts": attempts,
            "job_quota": job_quota,
            "global_quota": global_quota,
        },
    )


class UsageWithAttempts(Usage):
    """Usage subtype carrying the per-stage retry ledger."""

    def __new__(cls, usage: Usage, attempts: list[dict[str, Any]]):
        obj = object.__new__(cls)
        return obj

    def __init__(self, usage: Usage, attempts: list[dict[str, Any]]):
        object.__setattr__(self, "stage", usage.stage)
        object.__setattr__(self, "stage_index", usage.stage_index)
        object.__setattr__(self, "total_bytes", usage.total_bytes)
        object.__setattr__(self, "sentence_count", usage.sentence_count)
        object.__setattr__(self, "text_bytes", usage.text_bytes)
        object.__setattr__(self, "intermediate_bytes", usage.intermediate_bytes)
        object.__setattr__(self, "attempts", attempts)

    def to_dict(self) -> dict[str, Any]:
        result = super().to_dict()
        result["attempts"] = self.attempts
        result["degraded"] = self.stage_index > 0
        return result


class BudgetController:
    """Global byte quota with blocking acquisition and a ledger."""

    def __init__(
        self,
        global_quota: int,
        *,
        on_event: Callable[[dict[str, Any]], None] | None = None,
    ) -> None:
        if global_quota <= 0:
            raise ValueError("global_quota must be positive")
        self.global_quota = global_quota
        self.used_bytes = 0
        self.ledger: list[dict[str, Any]] = []
        self._condition = threading.Condition()
        self._on_event = on_event

    def _emit(self, value: dict[str, Any]) -> None:
        self.ledger.append(value)
        if self._on_event is not None:
            self._on_event(value)

    def acquire(self, job: str, usage: Usage) -> None:
        if usage.total_bytes > self.global_quota:
            raise LCFTError(
                "E_BUDGET_EXCEEDED",
                "single task usage exceeds the global memory quota",
                field="global_quota",
                details={
                    "job": job,
                    "usage_bytes": usage.total_bytes,
                    "global_quota": self.global_quota,
                },
            )
        queued = False
        with self._condition:
            while self.used_bytes + usage.total_bytes > self.global_quota:
                if not queued:
                    queued = True
                    self._emit(
                        {
                            "event": "queued",
                            "job": job,
                            "stage": usage.stage,
                            "usage_bytes": usage.total_bytes,
                            "used_bytes": self.used_bytes,
                        }
                    )
                self._condition.wait()
            self.used_bytes += usage.total_bytes
            self._emit(
                {
                    "event": "acquired",
                    "job": job,
                    "stage": usage.stage,
                    "bytes": usage.total_bytes,
                    "used_after": self.used_bytes,
                    "queued": queued,
                }
            )

    def release(self, job: str, usage: Usage) -> None:
        with self._condition:
            self.used_bytes = max(0, self.used_bytes - usage.total_bytes)
            self._emit(
                {
                    "event": "released",
                    "job": job,
                    "stage": usage.stage,
                    "bytes": usage.total_bytes,
                    "used_after": self.used_bytes,
                }
            )
            self._condition.notify_all()

