"""Schema 1.0 loader.

Characteristics of the legacy dialect:

* snake_case field names;
* time expressed in integer MIDI ticks (480 PPQ), measure-relative;
* velocity as MIDI integers (0..127), absent means 96;
* only ``staccato`` / ``legato`` / ``tenuto`` articulations and
  ``none`` / ``cresc`` / ``dim`` marks;
* pedal is an on/off boolean; no pitch bend events.
"""

from __future__ import annotations

from typing import Any

from ..constants import DEFAULT_VELOCITY
from ..constants import PPQ
from ..errors import SchemaError
from ..model import ExpressionEvent, Note, Score, TimeSignature
from .common import (
    beats_to_ticks,
    check_enum,
    default_fill,
    midi_velocity_to_normalised,
    normalisation_trace,
    normalised_velocity_to_midi,
    require,
    ticks_to_beats,
)

VERSION = "1.0"

V1_ARTICULATIONS = ("staccato", "legato", "tenuto")
V1_MARKS = ("none", "cresc", "dim")
V1_EVENT_KINDS = ("velocity", "legato", "pedal")

DEFAULT_MIDI_VELOCITY = 96


def load(data: dict[str, Any], source_path: str, traces):
    header = require(
        data, "header", expected_type=dict, path="root",
        source_version=VERSION, source_path=source_path,
    )
    title = require(
        header, "title", expected_type=str, path="root.header",
        source_version=VERSION, source_path=source_path,
    )
    bpm = require(
        header, "bpm", expected_type=(int, float), path="root.header",
        source_version=VERSION, source_path=source_path,
    )
    time_sigs_raw = require(
        data, "time_signatures", expected_type=list, path="root",
        source_version=VERSION, source_path=source_path,
    )
    notes_raw = require(
        data, "notes", expected_type=list, path="root",
        source_version=VERSION, source_path=source_path,
    )
    events_raw = require(
        data, "expression_events", expected_type=list, path="root",
        source_version=VERSION, source_path=source_path,
    )

    time_signatures = []
    for index, raw in enumerate(time_sigs_raw):
        measure = require(
            raw, "measure", expected_type=int,
            path=f"root.time_signatures[{index}]",
            source_version=VERSION, source_path=source_path,
        )
        beats = require(
            raw, "beats_per_bar", expected_type=int,
            path=f"root.time_signatures[{index}]",
            source_version=VERSION, source_path=source_path,
        )
        unit = require(
            raw, "beat_unit", expected_type=int,
            path=f"root.time_signatures[{index}]",
            source_version=VERSION, source_path=source_path,
        )
        time_signatures.append(TimeSignature(measure, beats, unit))

    notes: list[Note] = []
    defaulted: dict[str, set[str]] = {}
    for index, raw in enumerate(notes_raw):
        obj_path = f"root.notes[{index}]"
        note_id = require(
            raw, "note_id", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        measure = require(
            raw, "measure", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        start_ticks = require(
            raw, "start_ticks", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        duration_ticks = require(
            raw, "duration_ticks", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        pitch = require(
            raw, "pitch", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        fields: set[str] = set()
        if "velocity_midi" in raw:
            midi_velocity = raw["velocity_midi"]
            if not isinstance(midi_velocity, int):
                raise SchemaError(
                    "velocity_midi must be an integer",
                    {"file_path": source_path, "version": VERSION,
                     "field_path": f"{obj_path}.velocity_midi"},
                )
        else:
            midi_velocity = DEFAULT_MIDI_VELOCITY
            fields.add("velocity_midi")
            default_fill(
                traces, source_version=VERSION, target_version="4.0",
                object_path=obj_path, field_path="velocity_midi",
                detail=f"absent -> MIDI {DEFAULT_MIDI_VELOCITY}",
            )
        articulation = raw.get("articulation", "legato")
        if "articulation" not in raw:
            fields.add("articulation")
            default_fill(
                traces, source_version=VERSION, target_version="4.0",
                object_path=obj_path, field_path="articulation",
                detail="absent -> legato",
            )
        check_enum(
            articulation, V1_ARTICULATIONS, field_path=f"{obj_path}.articulation",
            source_version=VERSION, source_path=source_path,
        )
        mark = raw.get("expression_mark", "none")
        if "expression_mark" not in raw:
            fields.add("expression_mark")
            default_fill(
                traces, source_version=VERSION, target_version="4.0",
                object_path=obj_path, field_path="expression_mark",
                detail="absent -> none",
            )
        check_enum(
            mark, V1_MARKS, field_path=f"{obj_path}.expression_mark",
            source_version=VERSION, source_path=source_path,
        )
        start_beat = ticks_to_beats(start_ticks)
        duration_beat = ticks_to_beats(duration_ticks)
        normalisation_trace(
            traces, source_version=VERSION, target_version="4.0",
            object_path=obj_path, field_path="start_ticks/duration_ticks",
            detail=f"ticks({PPQ}) -> beats",
        )
        notes.append(
            Note(
                note_id=note_id,
                measure=measure,
                start_beat=start_beat,
                duration_beats=duration_beat,
                pitch=pitch,
                velocity=midi_velocity_to_normalised(midi_velocity),
                articulation=articulation,
                expression_mark=mark,
                interactive_layer=None,
            )
        )
        defaulted[note_id] = fields

    events: list[ExpressionEvent] = []
    event_defaulted: dict[str, set[str]] = {}
    for index, raw in enumerate(events_raw):
        obj_path = f"root.expression_events[{index}]"
        event_id = require(
            raw, "event_id", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        kind = require(
            raw, "event_type", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        check_enum(
            kind, V1_EVENT_KINDS, field_path=f"{obj_path}.event_type",
            source_version=VERSION, source_path=source_path,
        )
        target_note = require(
            raw, "target_note", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        measure = require(
            raw, "measure", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        position_ticks = require(
            raw, "position_ticks", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        fields: set[str] = set()
        value = 0.0
        duration_beats = 0.0
        if kind == "velocity":
            if "velocity_midi" in raw:
                value = midi_velocity_to_normalised(raw["velocity_midi"])
            else:
                value = DEFAULT_VELOCITY
                fields.add("velocity_midi")
                default_fill(
                    traces, source_version=VERSION, target_version="4.0",
                    object_path=obj_path, field_path="velocity_midi",
                    detail=f"absent -> {DEFAULT_VELOCITY}",
                )
        elif kind == "legato":
            length_ticks = require(
                raw, "length_ticks", expected_type=int, path=obj_path,
                source_version=VERSION, source_path=source_path,
            )
            duration_beats = ticks_to_beats(length_ticks)
            value = 1.0
        else:  # pedal
            value = 1.0 if raw.get("pedal", False) else 0.0
            if "pedal" not in raw:
                fields.add("pedal")
                default_fill(
                    traces, source_version=VERSION, target_version="4.0",
                    object_path=obj_path, field_path="pedal",
                    detail="absent -> off",
                )
        normalisation_trace(
            traces, source_version=VERSION, target_version="4.0",
            object_path=obj_path, field_path="position_ticks",
            detail="ticks -> beats",
        )
        events.append(
            ExpressionEvent(
                event_id=event_id,
                kind=kind,
                note_id=target_note,
                measure=measure,
                anchor_beat=ticks_to_beats(position_ticks),
                value=value,
                duration_beats=duration_beats,
                depth=None,
            )
        )
        event_defaulted[event_id] = fields

    score = Score(
        title=title,
        bpm=float(bpm),
        time_signatures=time_signatures,
        notes=notes,
        events=events,
        source_version=VERSION,
        source_path=source_path,
        traces=list(traces),
    )
    return score, {
        "note_defaulted": defaulted,
        "event_defaulted": event_defaulted,
    }

def export(score: Score, provenance: dict[str, Any] | None):
    """Serialise the canonical model back to schema 1.0."""
    provenance = provenance or {}
    note_defaulted: dict[str, set[str]] = provenance.get("note_defaulted", {})
    event_defaulted: dict[str, set[str]] = provenance.get("event_defaulted", {})

    notes_out = []
    for note in score.notes:
        omitted = note_defaulted.get(note.note_id, set())
        item: dict[str, Any] = {
            "note_id": note.note_id,
            "measure": note.measure,
            "start_ticks": beats_to_ticks(note.start_beat),
            "duration_ticks": beats_to_ticks(note.duration_beats),
            "pitch": note.pitch,
        }
        if "velocity_midi" not in omitted:
            item["velocity_midi"] = normalised_velocity_to_midi(note.velocity)
        if "articulation" not in omitted:
            item["articulation"] = note.articulation
        if "expression_mark" not in omitted:
            item["expression_mark"] = note.expression_mark
        notes_out.append(item)

    events_out = []
    for event in score.events:
        omitted = event_defaulted.get(event.event_id, set())
        item: dict[str, Any] = {
            "event_id": event.event_id,
            "event_type": event.kind,
            "target_note": event.note_id,
            "measure": event.measure,
            "position_ticks": beats_to_ticks(event.anchor_beat),
        }
        if event.kind == "velocity":
            if "velocity_midi" not in omitted:
                item["velocity_midi"] = normalised_velocity_to_midi(event.value)
        elif event.kind == "legato":
            item["length_ticks"] = beats_to_ticks(event.duration_beats)
        else:
            if "pedal" not in omitted:
                item["pedal"] = bool(round(event.value))
        events_out.append(item)

    return {
        "schema_version": VERSION,
        "header": {"title": score.title, "bpm": score.bpm},
        "time_signatures": [ts.to_dict() for ts in score.time_signatures],
        "notes": notes_out,
        "expression_events": events_out,
    }
