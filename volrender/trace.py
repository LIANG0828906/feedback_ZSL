"""Deterministic JSON-lines trace output."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any, Mapping

from .params import RenderParameters
from .renderer import RenderResult


def _json_safe(value: Any) -> Any:
    if isinstance(value, Path):
        return str(value)
    if hasattr(value, "item"):
        return value.item()
    if isinstance(value, tuple):
        return [_json_safe(item) for item in value]
    if isinstance(value, list):
        return [_json_safe(item) for item in value]
    if isinstance(value, dict):
        return {str(key): _json_safe(item) for key, item in value.items()}
    return value


class DeterministicTrace:
    """No wall-clock values and no absolute paths are written."""

    def __init__(self, path: str | os.PathLike[str], *, reset: bool = True) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._stream = self.path.open("w" if reset else "a", encoding="utf-8", newline="\n")
        self._sequence = 0

    def record(self, event: str, **fields: Any) -> None:
        payload = {
            "event": str(event),
            "sequence": self._sequence,
            **_json_safe(fields),
        }
        self._sequence += 1
        self._stream.write(
            json.dumps(payload, ensure_ascii=True, sort_keys=True, separators=(",", ":"), allow_nan=False)
            + "\n"
        )
        self._stream.flush()

    def record_render(
        self,
        event: str,
        params: RenderParameters,
        result: RenderResult,
        **extra: Any,
    ) -> None:
        result.assert_bound_to(params)
        self.record(
            event,
            parameter_hash=params.parameter_hash,
            buffer_hash=result.buffer_hash,
            view_matrix=[list(params.view_matrix[row * 4 : row * 4 + 4]) for row in range(4)],
            viewport=list(params.viewport),
            step_size=params.step_size,
            mask_enabled=params.mask_enabled,
            mask_hash=params.mask_hash,
            window_center=params.window_center,
            window_width=params.window_width,
            control_points=[point.to_dict() for point in params.control_points],
            **extra,
        )

    def record_mapping(self, event: str, **fields: Any) -> None:
        self.record(event, **fields)

    def flush(self) -> None:
        self._stream.flush()

    def close(self) -> None:
        if not self._stream.closed:
            self._stream.flush()
            self._stream.close()

    def __enter__(self) -> "DeterministicTrace":
        return self

    def __exit__(self, exc_type: object, exc: object, traceback: object) -> None:
        self.close()


def read_trace(path: str | os.PathLike[str]) -> list[Mapping[str, Any]]:
    records: list[Mapping[str, Any]] = []
    with Path(path).open("r", encoding="utf-8") as stream:
        for line in stream:
            if line.strip():
                records.append(json.loads(line))
    return records
