"""Input-derived deterministic random source with replay support."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Protocol

from .errors import E_REPLAY_MISMATCH, E_TRACE_INVALID, WorkbenchError
from .util import canonical_bytes, content_hash, sha256_text


MASK64 = (1 << 64) - 1


def derive_seed(seed: int, constraints: Any, params: Any) -> int:
    material = canonical_bytes({"seed": seed, "constraints": constraints, "params": params})
    return int.from_bytes(__import__("hashlib").sha256(material).digest()[:8], "big")


@dataclass
class RandomDraw:
    index: int
    stage: str
    name: str
    low: int
    high: int
    value: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "stage": self.stage,
            "name": self.name,
            "low": self.low,
            "high": self.high,
            "value": self.value,
        }


class RandomSource(Protocol):
    def randint(self, stage: str, name: str, low: int, high: int) -> int: ...

    def choice(self, stage: str, name: str, values: list[Any] | tuple[Any, ...]) -> Any: ...

    def shuffle(self, stage: str, name: str, values: list[Any]) -> None: ...

    def snapshot(self) -> dict[str, Any]: ...

    def restore(self, snapshot: dict[str, Any]) -> None: ...


class DeterministicRandom:
    """SplitMix64 driven only by a seed derived from the input document."""

    def __init__(self, seed: int, trace: Any | None = None) -> None:
        self._state = seed & MASK64
        self._cursor = 0
        self._draw_count = 0
        self._trace = trace

    def _next_u64(self) -> int:
        self._state = (self._state + 0x9E3779B97F4A7C15) & MASK64
        value = self._state
        value = ((value ^ (value >> 30)) * 0xBF58476D1CE4E5B9) & MASK64
        value = ((value ^ (value >> 27)) * 0x94D049BB133111EB) & MASK64
        return (value ^ (value >> 31)) & MASK64

    def _draw_int(self, stage: str, name: str, low: int, high: int) -> int:
        if high < low:
            raise ValueError(f"invalid random range [{low}, {high}]")
        span = high - low + 1
        limit = (1 << 64) - ((1 << 64) % span)
        while True:
            candidate = self._next_u64()
            if candidate < limit:
                break
        value = low + (candidate % span)
        draw = RandomDraw(self._draw_count, stage, name, low, high, value)
        self._draw_count += 1
        self._cursor += 1
        if self._trace is not None:
            self._trace.record_draw(draw)
        return value

    def randint(self, stage: str, name: str, low: int, high: int) -> int:
        return self._draw_int(stage, name, int(low), int(high))

    def choice(self, stage: str, name: str, values: list[Any] | tuple[Any, ...]) -> Any:
        if not values:
            raise ValueError("cannot choose from an empty sequence")
        index = self._draw_int(stage, f"{name}.index", 0, len(values) - 1)
        return values[index]

    def shuffle(self, stage: str, name: str, values: list[Any]) -> None:
        for index in range(len(values) - 1, 0, -1):
            other = self._draw_int(stage, f"{name}.swap_{index}", 0, index)
            values[index], values[other] = values[other], values[index]

    def snapshot(self) -> dict[str, Any]:
        return {
            "state": self._state,
            "cursor": self._cursor,
            "draw_count": self._draw_count,
        }

    def restore(self, snapshot: dict[str, Any]) -> None:
        self._state = int(snapshot["state"]) & MASK64
        self._cursor = int(snapshot["cursor"])
        self._draw_count = int(snapshot["draw_count"])


class ReplayRandom:
    """Consumes recorded samples and rejects any generator drift."""

    def __init__(self, draws: list[dict[str, Any]], trace: Any | None = None) -> None:
        self._draws = draws
        self._cursor = 0
        self._trace = trace

    def _consume(self, stage: str, name: str, low: int, high: int) -> int:
        if self._cursor >= len(self._draws):
            raise WorkbenchError(
                E_TRACE_INVALID,
                "trace is truncated before all random samples were replayed",
                details={"stage": stage, "name": name, "cursor": self._cursor},
            )
        draw = self._draws[self._cursor]
        expected = (stage, name, int(low), int(high))
        actual = (draw.get("stage"), draw.get("name"), draw.get("low"), draw.get("high"))
        if expected != actual:
            raise WorkbenchError(
                E_REPLAY_MISMATCH,
                "random draw request does not match the recorded trace",
                details={
                    "cursor": self._cursor,
                    "expected": expected,
                    "recorded": actual,
                },
            )
        value = draw.get("value")
        if not isinstance(value, int) or value < low or value > high:
            raise WorkbenchError(
                E_TRACE_INVALID,
                "recorded random value is outside its declared range",
                details={"cursor": self._cursor, "draw": draw},
            )
        if self._trace is not None:
            recorded_index = draw.get("index")
            if not isinstance(recorded_index, int):
                recorded_index = self._cursor
            self._trace.record_draw(
                RandomDraw(
                    index=recorded_index,
                    stage=stage,
                    name=name,
                    low=low,
                    high=high,
                    value=value,
                )
            )
        self._cursor += 1
        return value

    def randint(self, stage: str, name: str, low: int, high: int) -> int:
        return self._consume(stage, name, low, high)

    def choice(self, stage: str, name: str, values: list[Any] | tuple[Any, ...]) -> Any:
        if not values:
            raise ValueError("cannot choose from an empty sequence")
        index = self._consume(stage, f"{name}.index", 0, len(values) - 1)
        return values[index]

    def shuffle(self, stage: str, name: str, values: list[Any]) -> None:
        for index in range(len(values) - 1, 0, -1):
            other = self._consume(stage, f"{name}.swap_{index}", 0, index)
            values[index], values[other] = values[other], values[index]

    def snapshot(self) -> dict[str, Any]:
        return {"cursor": self._cursor}

    def restore(self, snapshot: dict[str, Any]) -> None:
        # Replay is the ordered transcript, not a second random simulation.
        # Generation rollbacks must not rewind the transcript cursor or the
        # next attempt would consume samples from the rolled-back branch.
        return None

    @property
    def consumed(self) -> int:
        return self._cursor

    @property
    def remaining(self) -> int:
        return len(self._draws) - self._cursor


def random_source_label(seed: int, constraints: Any, params: Any) -> str:
    return sha256_text(f"{seed}:{content_hash(constraints)}:{content_hash(params)}")
