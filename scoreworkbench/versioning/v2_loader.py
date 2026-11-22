"""Schema 2.0 loader.

Characteristics of the 2.x dialect:

* camelCase field names;
* notes use float beats inside a measure;
* expression events carry an absolute ``timeMs`` plus a measure/beat anchor;
* velocity is already a normalised 0..1 float (default 0.75);
* ``accent`` articulation and ``rit`` / ``aTempo`` marks appear for the first
  time; pedal remains a boolean.
"""

from __future__ import annotations

from typing import Any

from ..constants import DEFAULT_VELOCITY
from ..errors import SemanticError
from ..model import ExpressionEvent, Note, Score, TimeSignature
from .common import (
    check_enum,
    default_fill,
    millis_to_seconds,
    normalisation_trace,
    require,
    seconds_to_millis,
)

VERSION = "2.0"

V2_ARTICULATIONS = ("staccato", "legato", "tenuto", "accent")
V2_MARKS = ("none", "cresc", "dim", "rit", "a_tempo")
V2_EVENT_KINDS = ("velocity", "legato", "pedal")

MARK_FROM_CAMEL = {
    "none": "none",
    "cresc": "cresc",
    "dim": "dim",
    "rit": "rit",
    "aTempo": "a_tempo",
}
MARK_TO_CAMEL = {value: key for key, value in MARK_FROM_CAMEL.items()}


def load(data: dict[str, Any], source_path: str, traces):
    meta = require(
        data, "meta", expected_type=dict, path="root",
        source_version=VERSION, source_path=source_path,
    )
    title = require(
        meta, "scoreTitle", expected_type=str, path="root.meta",
        source_version=VERSION, source_path=source_path,
    )
    bpm = require(
        meta, "tempoBpm", expected_type=(int, float), path="root.meta",
        source_version=VERSION, source_path=source_path,
    )
    time_sigs_raw = require(
        data, "barSignatures", expected_type=list, path="root",
        source_version=VERSION, source_path=source_path,
    )
    notes_raw = require(
        data, "noteList", expected_type=list, path="root",
        source_version=VERSION, source_path=source_path,
    )
    events_raw = require(
        data, "performanceEvents", expected_type=list, path="root",
        source_version=VERSION, source_path=source_path,
    )

    time_signatures = []
    for index, raw in enumerate(time_sigs_raw):
        obj_path = f"root.barSignatures[{index}]"
        time_signatures.append(
            TimeSignature(
                measure=require(
                    raw, "barIndex", expected_type=int, path=obj_path,
                    source_version=VERSION, source_path=source_path,
                ),
                beats_per_bar=require(
                    raw, "beatsPerBar", expected_type=int, path=obj_path,
                    source_version=VERSION, source_path=source_path,
                ),
                beat_unit=require(
                    raw, "beatUnit", expected_type=int, path=obj_path,
                    source_version=VERSION, source_path=source_path,
                ),
            )
        )

    # A temporary score is needed up-front to resolve ms -> absolute beats.
    scaffold = Score(
        title=title, bpm=float(bpm), time_signatures=time_signatures,
        source_version=VERSION, source_path=source_path,
    )

    notes: list[Note] = []
    note_defaulted: dict[str, set[str]] = {}
    for index, raw in enumerate(notes_raw):
        obj_path = f"root.noteList[{index}]"
        note_id = require(
            raw, "id", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        measure = require(
            raw, "barIndex", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        start_beat = require(
            raw, "beatPos", expected_type=(int, float), path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        duration = require(
            raw, "beatLength", expected_type=(int, float), path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        pitch = require(
            raw, "midiPitch", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        fields: set[str] = set()
        if "intensity" in raw:
            velocity = float(raw["intensity"])
        else:
            velocity = DEFAULT_VELOCITY
            fields.add("intensity")
            default_fill(
                traces, source_version=VERSION, target_version="4.0",
                object_path=obj_path, field_path="intensity",
                detail=f"absent -> {DEFAULT_VELOCITY}",
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
            articulation, V2_ARTICULATIONS, field_path=f"{obj_path}.articulation",
            source_version=VERSION, source_path=source_path,
        )
        mark_camel = raw.get("mark", "none")
        if "mark" not in raw:
            fields.add("mark")
            default_fill(
                traces, source_version=VERSION, target_version="4.0",
                object_path=obj_path, field_path="mark",
                detail="absent -> none",
            )
        check_enum(
            mark_camel, tuple(MARK_FROM_CAMEL), field_path=f"{obj_path}.mark",
            source_version=VERSION, source_path=source_path,
        )
        notes.append(
            Note(
                note_id=note_id,
                measure=measure,
                start_beat=float(start_beat),
                duration_beats=float(duration),
                pitch=pitch,
                velocity=velocity,
                articulation=articulation,
                expression_mark=MARK_FROM_CAMEL[mark_camel],
                interactive_layer=None,
            )
        )
        note_defaulted[note_id] = fields

    events: list[ExpressionEvent] = []
    event_defaulted: dict[str, set[str]] = {}
    for index, raw in enumerate(events_raw):
        obj_path = f"root.performanceEvents[{index}]"
        event_id = require(
            raw, "id", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        kind = require(
            raw, "kind", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        check_enum(
            kind, V2_EVENT_KINDS, field_path=f"{obj_path}.kind",
            source_version=VERSION, source_path=source_path,
        )
        target_note = require(
            raw, "boundNoteId", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        measure_1based = require(
            raw, "barNumber", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        beat_in_bar = require(
            raw, "beatInBar", expected_type=(int, float), path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        measure = measure_1based - 1
        time_ms = require(
            raw, "timeMs", expected_type=(int, float), path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        fields: set[str] = set()
        value = 0.0
        duration_beats = 0.0
        if kind == "velocity":
            if "intensity" in raw:
                value = float(raw["intensity"])
            else:
                value = DEFAULT_VELOCITY
                fields.add("intensity")
                default_fill(
                    traces, source_version=VERSION, target_version="4.0",
                    object_path=obj_path, field_path="intensity",
                    detail=f"absent -> {DEFAULT_VELOCITY}",
                )
        elif kind == "legato":
            length = require(
                raw, "lengthBeats", expected_type=(int, float), path=obj_path,
                source_version=VERSION, source_path=source_path,
            )
            duration_beats = float(length)
            value = 1.0
        else:
            value = 1.0 if raw.get("pedalDown", False) else 0.0
            if "pedalDown" not in raw:
                fields.add("pedalDown")
                default_fill(
                    traces, source_version=VERSION, target_version="4.0",
                    object_path=obj_path, field_path="pedalDown",
                    detail="absent -> false",
                )

        anchor_beat = float(beat_in_bar)
        expected_seconds = scaffold.beat_to_seconds(
            scaffold.absolute_beat(measure, anchor_beat)
        )
        if abs(millis_to_seconds(float(time_ms)) - expected_seconds) > 1e-6:
            raise SemanticError(
                "timeMs is inconsistent with barNumber/beatInBar anchor",
                {
                    "file_path": source_path,
                    "version": VERSION,
                    "object_path": obj_path,
                    "time_ms": time_ms,
                    "expected_ms": seconds_to_millis(expected_seconds),
                },
            )
        normalisation_trace(
            traces, source_version=VERSION, target_version="4.0",
            object_path=obj_path, field_path="timeMs",
            detail="milliseconds -> canonical beats via tempo",
        )
        events.append(
            ExpressionEvent(
                event_id=event_id,
                kind=kind,
                note_id=target_note,
                measure=measure,
                anchor_beat=anchor_beat,
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
        "note_defaulted": note_defaulted,
        "event_defaulted": event_defaulted,
    }


def export(score: Score, provenance: dict[str, Any] | None):
    provenance = provenance or {}
    note_defaulted: dict[str, set[str]] = provenance.get("note_defaulted", {})
    event_defaulted: dict[str, set[str]] = provenance.get("event_defaulted", {})

    notes_out = []
    for note in score.notes:
        omitted = note_defaulted.get(note.note_id, set())
        item: dict[str, Any] = {
            "id": note.note_id,
            "barIndex": note.measure,
            "beatPos": note.start_beat,
            "beatLength": note.duration_beats,
            "midiPitch": note.pitch,
        }
        if "intensity" not in omitted:
            item["intensity"] = note.velocity
        if "articulation" not in omitted:
            item["articulation"] = note.articulation
        if "mark" not in omitted:
            item["mark"] = MARK_TO_CAMEL[note.expression_mark]
        notes_out.append(item)

    events_out = []
    for event in score.events:
        omitted = event_defaulted.get(event.event_id, set())
        absolute_beat = score.event_absolute_beat(event)
        item: dict[str, Any] = {
            "id": event.event_id,
            "kind": event.kind,
            "boundNoteId": event.note_id,
            "barNumber": event.measure + 1,
            "beatInBar": event.anchor_beat,
            "timeMs": seconds_to_millis(score.beat_to_seconds(absolute_beat)),
        }
        if event.kind == "velocity":
            if "intensity" not in omitted:
                item["intensity"] = event.value
        elif event.kind == "legato":
            item["lengthBeats"] = event.duration_beats
        else:
            if "pedalDown" not in omitted:
                item["pedalDown"] = bool(round(event.value))
        events_out.append(item)

    return {
        "schemaVersion": VERSION,
        "meta": {"scoreTitle": score.title, "tempoBpm": score.bpm},
        "barSignatures": [
            {
                "barIndex": ts.measure,
                "beatsPerBar": ts.beats_per_bar,
                "beatUnit": ts.beat_unit,
            }
            for ts in score.time_signatures
        ],
        "noteList": notes_out,
        "performanceEvents": events_out,
    }
