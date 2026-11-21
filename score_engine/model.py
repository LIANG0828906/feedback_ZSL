from __future__ import annotations

from dataclasses import dataclass, field, replace
from fractions import Fraction
from typing import Any, Iterable

from .util import (
    as_fraction,
    canonical_json_bytes,
    fraction_text,
    rounded_float,
    sha256_bytes,
)


ARTICULATIONS = {"normal", "legato", "staccato"}


@dataclass(frozen=True)
class TimeSignature:
    id: str
    start: Fraction
    numerator: int
    denominator: int

    @property
    def bar_length(self) -> Fraction:
        return Fraction(self.numerator * 4, self.denominator)

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "start": fraction_text(self.start),
            "numerator": self.numerator,
            "denominator": self.denominator,
            "bar_length": fraction_text(self.bar_length),
        }


@dataclass(frozen=True)
class TempoSegment:
    id: str
    start: Fraction
    bpm: Fraction
    end: Fraction | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "start": fraction_text(self.start),
            "end": None if self.end is None else fraction_text(self.end),
            "bpm": fraction_text(self.bpm),
        }


@dataclass(frozen=True)
class ExpressionAnchor:
    id: str
    start: Fraction
    value: Fraction
    part_id: str | None = None

    def to_dict(self, origin: Fraction = Fraction(0)) -> dict[str, Any]:
        return {
            "id": self.id,
            "part_id": self.part_id,
            "start": fraction_text(self.start - origin),
            "value": fraction_text(self.value),
        }


@dataclass(frozen=True)
class PedalSpan:
    id: str
    start: Fraction
    end: Fraction
    value: Fraction = Fraction(4, 5)

    def to_dict(self, origin: Fraction = Fraction(0)) -> dict[str, Any]:
        return {
            "id": self.id,
            "start": fraction_text(self.start - origin),
            "end": fraction_text(self.end - origin),
            "value": fraction_text(self.value),
        }


@dataclass(frozen=True)
class Note:
    id: str
    pitch: int
    start: Fraction
    duration: Fraction
    velocity: int = 80
    articulation: str = "normal"

    @property
    def end(self) -> Fraction:
        return self.start + self.duration

    def to_dict(self, origin: Fraction = Fraction(0)) -> dict[str, Any]:
        return {
            "id": self.id,
            "pitch": self.pitch,
            "start": fraction_text(self.start - origin),
            "duration": fraction_text(self.duration),
            "velocity": self.velocity,
            "articulation": self.articulation,
        }


@dataclass(frozen=True)
class Part:
    id: str
    name: str
    instrument: str
    min_pitch: int
    max_pitch: int
    notes: tuple[Note, ...]
    pedals: tuple[PedalSpan, ...] = ()

    @property
    def note_ids(self) -> tuple[str, ...]:
        return tuple(note.id for note in self.notes)

    def to_dict(self, origin: Fraction = Fraction(0)) -> dict[str, Any]:
        return {
            "id": self.id,
            "name": self.name,
            "instrument": self.instrument,
            "pitch_range": {"min": self.min_pitch, "max": self.max_pitch},
            "notes": [note.to_dict(origin) for note in self.notes],
            "pedals": [pedal.to_dict(origin) for pedal in self.pedals],
        }


@dataclass(frozen=True)
class SolverParameters:
    expression_strength: Fraction = Fraction(1)
    velocity_scale: Fraction = Fraction(1)
    legato_extension_limit: Fraction = Fraction(2)
    staccato_ratio: Fraction = Fraction(11, 20)
    pedal_default_value: Fraction = Fraction(4, 5)

    def to_dict(self) -> dict[str, Any]:
        return {
            "expression_strength": fraction_text(self.expression_strength),
            "velocity_scale": fraction_text(self.velocity_scale),
            "legato_extension_limit": fraction_text(self.legato_extension_limit),
            "staccato_ratio": fraction_text(self.staccato_ratio),
            "pedal_default_value": fraction_text(self.pedal_default_value),
        }


@dataclass(frozen=True)
class Score:
    title: str
    start_offset: Fraction
    grid_resolution: Fraction
    time_signatures: tuple[TimeSignature, ...]
    tempo_segments: tuple[TempoSegment, ...]
    parts: tuple[Part, ...]
    expression: tuple[ExpressionAnchor, ...]
    source_version: int
    provenance: dict[str, str] = field(default_factory=dict)
    composer: str = ""

    def all_notes(self) -> Iterable[tuple[Part, Note]]:
        for part in self.parts:
            for note in part.notes:
                yield part, note

    def note_count(self) -> int:
        return sum(len(part.notes) for part in self.parts)

    def note_by_id(self, note_id: str) -> tuple[Part, Note]:
        for part, note in self.all_notes():
            if note.id == note_id:
                return part, note
        raise KeyError(note_id)

    def signature_at(self, beat: Fraction) -> TimeSignature:
        active = self.time_signatures[0]
        for signature in self.time_signatures:
            if signature.start <= beat:
                active = signature
            else:
                break
        return active

    def to_internal_dict(self) -> dict[str, Any]:
        return {
            "schema": "score-internal-v3",
            "format_version": "3.0",
            "source_version": self.source_version,
            "title": self.title,
            "composer": self.composer,
            "start_offset": {
                "value": fraction_text(self.start_offset),
                "unit": "quarter_beats",
            },
            "grid_resolution": {
                "value": fraction_text(self.grid_resolution),
                "unit": "quarter_beats",
            },
            "time_signatures": [signature.to_dict() for signature in self.time_signatures],
            "tempo_segments": [segment.to_dict() for segment in self.tempo_segments],
            "expression": [anchor.to_dict(origin=self.start_offset) for anchor in self.expression],
            "parts": [part.to_dict(origin=self.start_offset) for part in self.parts],
            "provenance": dict(sorted(self.provenance.items())),
        }

    def score_hash(self) -> str:
        payload = self.to_internal_dict()
        payload.pop("source_version", None)
        return sha256_bytes(canonical_json_bytes(payload))


def make_v3_score(
    *,
    title: str,
    start_offset: Fraction,
    grid_resolution: Fraction,
    time_signatures: list[TimeSignature] | tuple[TimeSignature, ...],
    tempo_segments: list[TempoSegment] | tuple[TempoSegment, ...],
    parts: list[Part] | tuple[Part, ...],
    expression: list[ExpressionAnchor] | tuple[ExpressionAnchor, ...],
    source_version: int = 3,
    provenance: dict[str, str] | None = None,
    composer: str = "",
) -> Score:
    return Score(
        title=title,
        start_offset=start_offset,
        grid_resolution=grid_resolution,
        time_signatures=tuple(time_signatures),
        tempo_segments=tuple(tempo_segments),
        parts=tuple(parts),
        expression=tuple(expression),
        source_version=source_version,
        provenance=dict(provenance or {}),
        composer=composer,
    )


@dataclass(frozen=True)
class NoteEdit:
    kind: str
    part_id: str | None = None
    note_id: str | None = None
    index: int | None = None
    value: Any = None
    old_value: Any = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "kind": self.kind,
            "part_id": self.part_id,
            "note_id": self.note_id,
            "index": self.index,
            "value": _edit_value(self.value),
            "old_value": _edit_value(self.old_value),
        }


def _edit_value(value: Any) -> Any:
    if isinstance(value, Fraction):
        return fraction_text(value)
    if isinstance(value, (TempoSegment, ExpressionAnchor, PedalSpan, Note)):
        return value.to_dict()
    return value


def replace_score_part(score: Score, part: Part) -> Score:
    parts = tuple(part if current.id == part.id else current for current in score.parts)
    return replace(score, parts=parts)


def derived_fraction(value: Any, field_name: str) -> Fraction:
    return as_fraction(value, field=field_name)

