"""Edit sessions with explicit commit boundaries.

Edits mutate a private working copy only.  ``commit`` applies an entire batch
atomically: validation runs on the candidate model first, so a failing batch
leaves the committed model byte-for-byte untouched.
"""

from __future__ import annotations

import uuid
from dataclasses import dataclass
from typing import Any

from .errors import DanglingError, ParameterError
from .model import BatchRecord, ExpressionEvent, Note, Score


def _validate_velocity(value: float, event_id: str | None = None,
                       note_id: str | None = None) -> None:
    if not isinstance(value, (int, float)) or not 0.0 <= float(value) <= 1.0:
        raise ParameterError(
            "Velocity must stay within the closed range [0,1]",
            {"source_event_id": event_id, "note_id": note_id, "value": value},
        )


def _validate_beat(value: float, field: str, object_id: str) -> None:
    if not isinstance(value, (int, float)) or float(value) < 0:
        raise ParameterError(
            "Beat positions must be non-negative numbers",
            {"field": field, "object_id": object_id, "value": value},
        )


@dataclass
class PendingEdit:
    op: str
    target_id: str
    payload: dict[str, Any]


class EditSession:
    def __init__(self, score: Score):
        self._committed = score
        self._working = score.clone()
        self._pending: list[PendingEdit] = []
        self.batch_records: list[BatchRecord] = list(score.batch_records)

    # ---- access -------------------------------------------------------
    @property
    def working(self) -> Score:
        return self._working

    @property
    def committed(self) -> Score:
        return self._committed

    def pending_changes(self) -> list[dict[str, Any]]:
        return [
            {"op": edit.op, "target_id": edit.target_id, "payload": edit.payload}
            for edit in self._pending
        ]

    # ---- note edits ---------------------------------------------------
    def edit_note(self, note_id: str, **changes: Any) -> None:
        note = self._working.note_by_id(note_id)
        if note is None:
            raise KeyError(note_id)
        if "velocity" in changes:
            _validate_velocity(changes["velocity"], note_id=note_id)
        for beat_field in ("start_beat", "duration_beats"):
            if beat_field in changes:
                _validate_beat(changes[beat_field], beat_field, note_id)
        if "measure" in changes and int(changes["measure"]) < 0:
            raise ParameterError(
                "Measure index must be non-negative",
                {"field": "measure", "object_id": note_id},
            )
        old_measure = note.measure
        old_beat = note.start_beat
        self._working.replace_note(note_id, **changes)
        # Bound expression events migrate with the note anchor.
        new_note = self._working.note_by_id(note_id)
        if "measure" in changes or "start_beat" in changes:
            delta_measure = new_note.measure - old_measure
            delta_beat = new_note.start_beat - old_beat
            for event in self._working.events_for_note(note_id):
                event.measure += delta_measure
                event.anchor_beat += delta_beat
                _validate_beat(event.anchor_beat, "anchor_beat", event.event_id)
        self._pending.append(PendingEdit("edit_note", note_id, dict(changes)))

    def replace_note(self, new_note: Note) -> None:
        """Replace a note while migrating every bound expression event."""
        existing = self._working.note_by_id(new_note.note_id)
        if existing is None:
            raise KeyError(new_note.note_id)
        _validate_velocity(new_note.velocity, note_id=new_note.note_id)
        _validate_beat(new_note.start_beat, "start_beat", new_note.note_id)
        delta_measure = new_note.measure - existing.measure
        delta_beat = new_note.start_beat - existing.start_beat
        index = next(
            i for i, note in enumerate(self._working.notes)
            if note.note_id == new_note.note_id
        )
        self._working.notes[index] = new_note
        for event in self._working.events_for_note(new_note.note_id):
            event.measure += delta_measure
            event.anchor_beat += delta_beat
        self._working.rebuild_index()
        self._pending.append(
            PendingEdit("replace_note", new_note.note_id, new_note.to_dict())
        )

    def delete_note(self, note_id: str) -> None:
        """Delete a note and its bound events; reject orphans explicitly."""
        note = self._working.note_by_id(note_id)
        if note is None:
            raise KeyError(note_id)
        bound_event_ids = [
            event.event_id for event in self._working.events_for_note(note_id)
        ]
        self._working.notes = [
            item for item in self._working.notes if item.note_id != note_id
        ]
        self._working.events = [
            event for event in self._working.events if event.note_id != note_id
        ]
        self._working.rebuild_index()
        self._pending.append(
            PendingEdit(
                "delete_note", note_id, {"removed_events": bound_event_ids}
            )
        )

    def add_note(self, note: Note) -> None:
        _validate_velocity(note.velocity, note_id=note.note_id)
        self._working.notes.append(note)
        self._working.rebuild_index()
        self._pending.append(PendingEdit("add_note", note.note_id, note.to_dict()))

    # ---- expression edits --------------------------------------------
    def add_event(self, event: ExpressionEvent) -> None:
        if self._working.note_by_id(event.note_id) is None:
            raise DanglingError(
                "Cannot add an expression event without a bound note",
                {"event_id": event.event_id, "target_note_id": event.note_id},
            )
        if event.kind == "velocity":
            _validate_velocity(event.value, event_id=event.event_id)
        _validate_beat(event.anchor_beat, "anchor_beat", event.event_id)
        self._working.events.append(event)
        self._working.rebuild_index()
        self._pending.append(
            PendingEdit("add_event", event.event_id, event.to_dict())
        )

    def edit_event(self, event_id: str, **changes: Any) -> None:
        event = self._working.event_by_id(event_id)
        if event is None:
            raise KeyError(event_id)
        if "value" in changes and event.kind == "velocity":
            _validate_velocity(changes["value"], event_id=event_id)
        if "anchor_beat" in changes:
            _validate_beat(changes["anchor_beat"], "anchor_beat", event_id)
        for key, value in changes.items():
            setattr(event, key, value)
        self._working.rebuild_index()
        self._pending.append(PendingEdit("edit_event", event_id, dict(changes)))

    def delete_event(self, event_id: str) -> None:
        if self._working.event_by_id(event_id) is None:
            raise KeyError(event_id)
        self._working.events = [
            event for event in self._working.events if event.event_id != event_id
        ]
        self._working.rebuild_index()
        self._pending.append(PendingEdit("delete_event", event_id, {}))

    def set_velocity_curve(self, assignments: dict[str, float]) -> None:
        """Bulk velocity edit; all values are validated before any mutation."""
        for event_id, value in assignments.items():
            event = self._working.event_by_id(event_id)
            if event is None or event.kind != "velocity":
                raise ParameterError(
                    "Velocity curve target must be an existing velocity event",
                    {"source_event_id": event_id},
                )
            _validate_velocity(value, event_id=event_id)
        for event_id, value in assignments.items():
            self._working.event_by_id(event_id).value = value
            self._pending.append(
                PendingEdit("edit_event", event_id, {"value": value})
            )

    # ---- commit boundary ---------------------------------------------
    def commit(self) -> BatchRecord:
        candidate = self._working.clone()
        try:
            candidate.rebuild_index()
            candidate.validate_references()
        except DanglingError:
            self._working = self._committed.clone()
            raise
        affected = sorted({edit.target_id for edit in self._pending})
        record = BatchRecord(
            batch_id=f"batch-{uuid.uuid4().hex[:12]}",
            affected_objects=affected,
            operations=self.pending_changes(),
            committed=True,
        )
        candidate.batch_records = self._committed.batch_records + [record]
        self._committed = candidate
        self._working = candidate.clone()
        self._pending.clear()
        self.batch_records = list(candidate.batch_records)
        return record

    def rollback(self) -> None:
        self._working = self._committed.clone()
        self._pending.clear()

    def has_pending(self) -> bool:
        return bool(self._pending)
