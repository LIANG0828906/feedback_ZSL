"""Unified internal domain model.

Canonical time unit is the *beat* (quarter note), measured on an absolute
timeline where bar ``m`` (0-based) starts at
``sum(time_signatures[:m].beats_per_bar)``.  Every version-specific loader
converts ticks / milliseconds / microseconds into this single unit, therefore
switching the source format can never shift an event's absolute time.
"""

from __future__ import annotations

from dataclasses import dataclass, field, replace
from typing import Any

from .constants import (
    DEFAULT_VELOCITY,
    DOMAIN_MODEL_VERSION,
    EVENT_KINDS,
)
from .errors import DanglingError, SemanticError


@dataclass
class TimeSignature:
    measure: int
    beats_per_bar: int
    beat_unit: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "measure": self.measure,
            "beats_per_bar": self.beats_per_bar,
            "beat_unit": self.beat_unit,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "TimeSignature":
        return cls(
            measure=int(data["measure"]),
            beats_per_bar=int(data["beats_per_bar"]),
            beat_unit=int(data["beat_unit"]),
        )


@dataclass
class Note:
    note_id: str
    measure: int
    start_beat: float
    duration_beats: float
    pitch: int
    velocity: float = DEFAULT_VELOCITY
    articulation: str = "legato"
    expression_mark: str = "none"
    interactive_layer: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "note_id": self.note_id,
            "measure": self.measure,
            "start_beat": self.start_beat,
            "duration_beats": self.duration_beats,
            "pitch": self.pitch,
            "velocity": self.velocity,
            "articulation": self.articulation,
            "expression_mark": self.expression_mark,
            "interactive_layer": self.interactive_layer,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Note":
        return cls(
            note_id=data["note_id"],
            measure=int(data["measure"]),
            start_beat=float(data["start_beat"]),
            duration_beats=float(data["duration_beats"]),
            pitch=int(data["pitch"]),
            velocity=float(data.get("velocity", DEFAULT_VELOCITY)),
            articulation=data.get("articulation", "legato"),
            expression_mark=data.get("expression_mark", "none"),
            interactive_layer=data.get("interactive_layer"),
        )


@dataclass
class ExpressionEvent:
    event_id: str
    kind: str
    note_id: str
    measure: int
    anchor_beat: float
    value: float
    duration_beats: float = 0.0
    depth: float | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "event_id": self.event_id,
            "kind": self.kind,
            "note_id": self.note_id,
            "measure": self.measure,
            "anchor_beat": self.anchor_beat,
            "value": self.value,
            "duration_beats": self.duration_beats,
            "depth": self.depth,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ExpressionEvent":
        return cls(
            event_id=data["event_id"],
            kind=data["kind"],
            note_id=data["note_id"],
            measure=int(data["measure"]),
            anchor_beat=float(data["anchor_beat"]),
            value=float(data["value"]),
            duration_beats=float(data.get("duration_beats", 0.0)),
            depth=(None if data.get("depth") is None else float(data["depth"])),
        )


@dataclass
class TraceRecord:
    """One traceable conversion action (default fill / unit normalisation ...)."""

    step: str
    source_version: str
    target_version: str
    object_path: str
    field_path: str
    action: str
    detail: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "step": self.step,
            "source_version": self.source_version,
            "target_version": self.target_version,
            "object_path": self.object_path,
            "field_path": self.field_path,
            "action": self.action,
            "detail": self.detail,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "TraceRecord":
        return cls(**data)


@dataclass
class BatchRecord:
    batch_id: str
    affected_objects: list[str]
    operations: list[dict[str, Any]]
    committed: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "batch_id": self.batch_id,
            "affected_objects": list(self.affected_objects),
            "operations": list(self.operations),
            "committed": self.committed,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "BatchRecord":
        return cls(
            batch_id=data["batch_id"],
            affected_objects=list(data["affected_objects"]),
            operations=list(data["operations"]),
            committed=bool(data["committed"]),
        )


@dataclass
class Score:
    title: str
    bpm: float
    time_signatures: list[TimeSignature]
    notes: list[Note] = field(default_factory=list)
    events: list[ExpressionEvent] = field(default_factory=list)
    source_version: str | None = None
    source_path: str | None = None
    traces: list[TraceRecord] = field(default_factory=list)
    batch_records: list[BatchRecord] = field(default_factory=list)
    domain_model_version: str = DOMAIN_MODEL_VERSION

    # ---- indexing -----------------------------------------------------
    def rebuild_index(self) -> None:
        """Re-derive note/event lookup indexes and validate consistency."""
        self._note_index = {note.note_id: note for note in self.notes}
        self._event_index = {event.event_id: event for event in self.events}
        if len(self._note_index) != len(self.notes):
            dupes = sorted(
                {nid for nid in self._note_index if
                 sum(1 for n in self.notes if n.note_id == nid) > 1}
            )
            raise SemanticError(
                "Duplicate note identifiers detected",
                {"object_path": "score.notes", "duplicate_ids": dupes},
            )
        if len(self._event_index) != len(self.events):
            raise SemanticError(
                "Duplicate event identifiers detected",
                {"object_path": "score.events"},
            )
        self.validate_references()

    def note_by_id(self, note_id: str) -> Note | None:
        return getattr(self, "_note_index", {}).get(note_id)

    def event_by_id(self, event_id: str) -> ExpressionEvent | None:
        return getattr(self, "_event_index", {}).get(event_id)

    def events_for_note(self, note_id: str) -> list[ExpressionEvent]:
        return [event for event in self.events if event.note_id == note_id]

    def validate_references(self) -> None:
        note_ids = {note.note_id for note in self.notes}
        for event in self.events:
            if event.kind not in EVENT_KINDS:
                raise SemanticError(
                    f"Unknown expression event kind {event.kind!r}",
                    {"object_path": f"score.events[{event.event_id}].kind",
                     "event_id": event.event_id},
                )
            if event.note_id not in note_ids:
                raise DanglingError(
                    "Expression event is not bound to an existing note",
                    {"event_id": event.event_id, "target_note_id": event.note_id},
                )

    # ---- time helpers -------------------------------------------------
    def bar_start_beat(self, measure: int) -> float:
        if measure < 0:
            raise SemanticError(
                "Measure index must be non-negative",
                {"object_path": "score.time_signatures", "measure": measure},
            )
        sigs = sorted(self.time_signatures, key=lambda ts: ts.measure)
        if not sigs or sigs[0].measure != 0:
            raise SemanticError(
                "A time signature at measure 0 is required",
                {"object_path": "score.time_signatures"},
            )
        start = 0.0
        current_beats = sigs[0].beats_per_bar
        sig_map = {ts.measure: ts for ts in sigs}
        for bar in range(1, measure + 1):
            start += current_beats
            if bar in sig_map:
                current_beats = sig_map[bar].beats_per_bar
        return start

    def _compute_bar_starts(self) -> list[float]:
        max_measure = max(
            [0] + [n.measure for n in self.notes] + [e.measure for e in self.events]
        )
        starts = [
            self.bar_start_beat(measure) for measure in range(max_measure + 2)
        ]
        self._bar_starts = starts
        return starts

    def absolute_beat(self, measure: int, beat_in_bar: float) -> float:
        return self.bar_start_beat(measure) + beat_in_bar

    def beat_to_seconds(self, beat: float) -> float:
        return beat * 60.0 / self.bpm

    def seconds_to_beat(self, seconds: float) -> float:
        return seconds * self.bpm / 60.0

    def total_beats(self) -> float:
        starts = self._compute_bar_starts()
        last_measure = max(
            [0] + [n.measure for n in self.notes] + [e.measure for e in self.events]
        )
        return starts[last_measure] + max(
            [0.0]
            + [n.start_beat + n.duration_beats for n in self.notes
               if n.measure == last_measure]
            + [e.anchor_beat for e in self.events if e.measure == last_measure]
        )

    def event_absolute_beat(self, event: ExpressionEvent) -> float:
        return self.absolute_beat(event.measure, event.anchor_beat)

    def note_absolute_beat(self, note: Note) -> float:
        return self.absolute_beat(note.measure, note.start_beat)

    # ---- serialisation ------------------------------------------------
    def to_dict(self) -> dict[str, Any]:
        return {
            "domain_model_version": self.domain_model_version,
            "title": self.title,
            "bpm": self.bpm,
            "time_signatures": [ts.to_dict() for ts in self.time_signatures],
            "notes": [note.to_dict() for note in self.notes],
            "events": [event.to_dict() for event in self.events],
            "source_version": self.source_version,
            "source_path": self.source_path,
            "traces": [trace.to_dict() for trace in self.traces],
            "batch_records": [batch.to_dict() for batch in self.batch_records],
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Score":
        score = cls(
            title=data["title"],
            bpm=float(data["bpm"]),
            time_signatures=[
                TimeSignature.from_dict(item) for item in data["time_signatures"]
            ],
            notes=[Note.from_dict(item) for item in data.get("notes", [])],
            events=[
                ExpressionEvent.from_dict(item) for item in data.get("events", [])
            ],
            source_version=data.get("source_version"),
            source_path=data.get("source_path"),
            traces=[TraceRecord.from_dict(item) for item in data.get("traces", [])],
            batch_records=[
                BatchRecord.from_dict(item) for item in data.get("batch_records", [])
            ],
            domain_model_version=data.get(
                "domain_model_version", DOMAIN_MODEL_VERSION
            ),
        )
        score.rebuild_index()
        return score

    def clone(self) -> "Score":
        return Score.from_dict(self.to_dict())

    def replace_note(self, note_id: str, **changes: Any) -> Note:
        note = self.note_by_id(note_id)
        if note is None:
            raise KeyError(note_id)
        updated = replace(note, **changes)
        index = next(i for i, n in enumerate(self.notes) if n.note_id == note_id)
        self.notes[index] = updated
        self.rebuild_index()
        return updated
