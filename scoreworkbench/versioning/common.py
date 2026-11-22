"""Shared helpers for the version-specific loaders and exporters."""

from __future__ import annotations

from typing import Any

from ..constants import (
    ARTICULATIONS,
    DEFAULT_VELOCITY,
    EXPRESSION_MARKS,
    MILLIS_PER_SECOND,
    MICROS_PER_SECOND,
    PPQ,
)
from ..errors import SchemaError, SemanticError
from ..model import TraceRecord


def require(
    data: dict[str, Any],
    key: str,
    *,
    path: str,
    source_version: str,
    source_path: str,
    expected_type: type | tuple[type, ...] | None = None,
) -> Any:
    """Return a required field or raise E_SCHEMA with the first violation."""
    if key not in data or data[key] is None:
        raise SchemaError(
            f"Required field {path}.{key} is missing",
            {
                "file_path": source_path,
                "version": source_version,
                "field_path": f"{path}.{key}",
            },
        )
    value = data[key]
    if expected_type and not isinstance(value, expected_type):
        raise SchemaError(
            f"Field {path}.{key} has an unexpected type",
            {
                "file_path": source_path,
                "version": source_version,
                "field_path": f"{path}.{key}",
                "expected": getattr(expected_type, "__name__", str(expected_type)),
                "actual": type(value).__name__,
            },
        )
    return value


def default_fill(
    traces: list[TraceRecord],
    *,
    source_version: str,
    target_version: str,
    object_path: str,
    field_path: str,
    detail: str,
) -> None:
    traces.append(
        TraceRecord(
            step=f"upgrade {source_version}->{target_version}",
            source_version=source_version,
            target_version=target_version,
            object_path=object_path,
            field_path=field_path,
            action="default_fill",
            detail=detail,
        )
    )


def normalisation_trace(
    traces: list[TraceRecord],
    *,
    source_version: str,
    target_version: str,
    object_path: str,
    field_path: str,
    detail: str,
) -> None:
    traces.append(
        TraceRecord(
            step=f"upgrade {source_version}->{target_version}",
            source_version=source_version,
            target_version=target_version,
            object_path=object_path,
            field_path=field_path,
            action="normalise",
            detail=detail,
        )
    )


def check_enum(
    value: str,
    allowed: tuple[str, ...],
    *,
    field_path: str,
    source_version: str,
    source_path: str,
) -> None:
    if value not in allowed:
        raise SemanticError(
            f"Value {value!r} is not allowed for {field_path}",
            {
                "file_path": source_path,
                "version": source_version,
                "field_path": field_path,
                "allowed": list(allowed),
            },
        )


def validate_canonical_enums(score) -> None:
    for note in score.notes:
        if note.articulation not in ARTICULATIONS:
            raise SemanticError(
                f"Unknown articulation {note.articulation!r}",
                {"object_path": f"score.notes[{note.note_id}].articulation"},
            )
        if note.expression_mark not in EXPRESSION_MARKS:
            raise SemanticError(
                f"Unknown expression mark {note.expression_mark!r}",
                {"object_path": f"score.notes[{note.note_id}].expression_mark"},
            )
        if not 0.0 <= note.velocity <= 1.0:
            raise SemanticError(
                "Velocity must stay within [0,1]",
                {"object_path": f"score.notes[{note.note_id}].velocity"},
            )


# ---------------------------------------------------------------------
# Deterministic unit conversions.  All use exact integer arithmetic where
# possible so that load -> export round-trips are bit-stable.
# ---------------------------------------------------------------------
def ticks_to_beats(ticks: int) -> float:
    return ticks / PPQ


def beats_to_ticks(beats: float) -> int:
    return int(round(beats * PPQ))


def millis_to_seconds(millis: float) -> float:
    return millis / MILLIS_PER_SECOND


def seconds_to_millis(seconds: float) -> float:
    return seconds * MILLIS_PER_SECOND


def micros_to_seconds(micros: int) -> float:
    return micros / MICROS_PER_SECOND


def seconds_to_micros(seconds: float) -> int:
    return int(round(seconds * MICROS_PER_SECOND))


def midi_velocity_to_normalised(midi_velocity: int) -> float:
    return midi_velocity / 127.0


def normalised_velocity_to_midi(velocity: float) -> int:
    return int(round(velocity * 127))


CANONICAL_DEFAULT_VELOCITY = DEFAULT_VELOCITY
