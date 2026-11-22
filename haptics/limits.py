from __future__ import annotations

from dataclasses import dataclass

from .errors import ErrorCode, HapticError, Issue


@dataclass(frozen=True)
class ResourceLimits:
    max_parallelism: int = 4
    max_events_per_task: int = 256
    max_packets_per_task: int = 128

    def validate(self) -> None:
        if not 1 <= self.max_parallelism <= 32:
            raise HapticError(
                Issue(
                    ErrorCode.E_PARA,
                    "max_parallelism must be within [1, 32]",
                    field="max_parallelism",
                    details={"value": self.max_parallelism},
                )
            )
        if not 1 <= self.max_events_per_task <= 100_000:
            raise HapticError(
                Issue(
                    ErrorCode.E_PARA,
                    "max_events_per_task must be within [1, 100000]",
                    field="max_events_per_task",
                    details={"value": self.max_events_per_task},
                )
            )
        if not 1 <= self.max_packets_per_task <= 100_000:
            raise HapticError(
                Issue(
                    ErrorCode.E_PARA,
                    "max_packets_per_task must be within [1, 100000]",
                    field="max_packets_per_task",
                    details={"value": self.max_packets_per_task},
                )
            )

    def to_dict(self) -> dict[str, int]:
        return {
            "max_parallelism": self.max_parallelism,
            "max_events_per_task": self.max_events_per_task,
            "max_packets_per_task": self.max_packets_per_task,
        }

