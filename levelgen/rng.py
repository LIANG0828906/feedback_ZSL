"""Local deterministic random source with auditable draws."""

from __future__ import annotations

import hashlib
from collections.abc import Callable, Sequence
from typing import Any, TypeVar


DrawRecorder = Callable[[str, list[Any], Any, str], None]
T = TypeVar("T")


class DeterministicRNG:
    """SplitMix64 seeded independently per attempt and pipeline stage."""

    _MASK = (1 << 64) - 1

    def __init__(
        self,
        seed: int,
        attempt: int,
        stream: str,
        recorder: DrawRecorder | None = None,
        checkpoint: Callable[[], None] | None = None,
    ) -> None:
        digest = hashlib.sha256(
            f"levelgen-rng/1|{seed}|{attempt}|{stream}".encode("utf-8")
        ).digest()
        self._state = int.from_bytes(digest[:8], "little")
        self._stream = stream
        self._recorder = recorder
        self._checkpoint = checkpoint

    def _next_u64(self) -> int:
        if self._checkpoint is not None:
            self._checkpoint()
        self._state = (self._state + 0x9E3779B97F4A7C15) & self._MASK
        value = self._state
        value = ((value ^ (value >> 30)) * 0xBF58476D1CE4E5B9) & self._MASK
        value = ((value ^ (value >> 27)) * 0x94D049BB133111EB) & self._MASK
        return (value ^ (value >> 31)) & self._MASK

    def _randbelow(self, bound: int) -> int:
        if bound <= 0:
            raise ValueError("bound must be positive")
        if bound & (bound - 1) == 0:
            return self._next_u64() & (bound - 1)
        limit = ((1 << 64) // bound) * bound
        while True:
            value = self._next_u64()
            if value < limit:
                return value % bound

    def randint(self, low: int, high: int) -> int:
        if high < low:
            raise ValueError("high must be >= low")
        result = low + self._randbelow(high - low + 1)
        if self._recorder is not None:
            self._recorder("randint", [low, high], result, self._stream)
        return result

    def choice(self, values: Sequence[T]) -> T:
        if not values:
            raise ValueError("cannot choose from an empty sequence")
        index = self._randbelow(len(values))
        result = values[index]
        if self._recorder is not None:
            self._recorder("choice", [len(values)], index, self._stream)
        return result

    def shuffle(self, values: list[T]) -> None:
        for index in range(len(values) - 1, 0, -1):
            swap_index = self._randbelow(index + 1)
            values[index], values[swap_index] = values[swap_index], values[index]
            if self._recorder is not None:
                self._recorder(
                    "swap",
                    [index, swap_index],
                    [
                        values[index].id
                        if hasattr(values[index], "id")
                        else values[index],
                        values[swap_index].id
                        if hasattr(values[swap_index], "id")
                        else values[swap_index],
                    ],
                    self._stream,
                )
