"""Schema 3.0 loader.

Characteristics of the newest dialect:

* time in microseconds, both as an absolute ``start_us`` and an in-bar
  ``anchor_us`` alongside the measure index;
* full articulation/mark vocabulary plus an ``interactiveLayer`` tag used by
  adaptive game music;
* ``pitchBend`` events and fractional pedal ``depth`` are expressible.
"""

from __future__ import annotations

from typing import Any

from ..errors import SemanticError
from ..model import ExpressionEvent, Note, Score, TimeSignature
from .common import (
    check_enum,
    default_fill,
    micros_to_seconds,
    normalisation_trace,
    require,
    seconds_to_micros,
)

VERSION = "3.0"

V3_ARTICULATIONS = ("staccato", "legato", "tenuto", "accent")
V3_MARKS = ("none", "cresc", "dim", "rit", "a_tempo", "fermata")
V3_EVENT_KINDS = ("velocity", "legato", "pedal", "pitch_bend")


def load(data: dict[str, Any], source_path: str, traces):
    info = require(
        data, "score_info", expected_type=dict, path="root",
        source_version=VERSION, source_path=source_path,
    )
    title = require(
        info, "name", expected_type=str, path="root.score_info",
        source_version=VERSION, source_path=source_path,
    )
    bpm = require(
        info, "tempo", expected_type=(int, float), path="root.score_info",
        source_version=VERSION, source_path=source_path,
    )
    time_sigs_raw = require(
        data, "meters", expected_type=list, path="root",
        source_version=VERSION, source_path=source_path,
    )
    notes_raw = require(
        data, "score_notes", expected_type=list, path="root",
        source_version=VERSION, source_path=source_path,
    )
    events_raw = require(
        data, "performance_events", expected_type=list, path="root",
        source_version=VERSION, source_path=source_path,
    )

    time_signatures = []
    for index, raw in enumerate(time_sigs_raw):
        obj_path = f"root.meters[{index}]"
        time_signatures.append(
            TimeSignature(
                measure=require(
                    raw, "measure", expected_type=int, path=obj_path,
                    source_version=VERSION, source_path=source_path,
                ),
                beats_per_bar=require(
                    raw, "numerator", expected_type=int, path=obj_path,
                    source_version=VERSION, source_path=source_path,
                ),
                beat_unit=require(
                    raw, "denominator", expected_type=int, path=obj_path,
                    source_version=VERSION, source_path=source_path,
                ),
            )
        )

    scaffold = Score(
        title=title, bpm=float(bpm), time_signatures=time_signatures,
        source_version=VERSION, source_path=source_path,
    )

    notes: list[Note] = []
    note_defaulted: dict[str, set[str]] = {}
    for index, raw in enumerate(notes_raw):
        obj_path = f"root.score_notes[{index}]"
        note_id = require(
            raw, "id", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        measure = require(
            raw, "measure", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        start_us = require(
            raw, "start_us", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        duration_us = require(
            raw, "duration_us", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        pitch = require(
            raw, "pitch", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        velocity = require(
            raw, "velocity", expected_type=(int, float), path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        articulation = require(
            raw, "articulation", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        check_enum(
            articulation, V3_ARTICULATIONS,
            field_path=f"{obj_path}.articulation",
            source_version=VERSION, source_path=source_path,
        )
        mark = require(
            raw, "mark", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        check_enum(
            mark, V3_MARKS, field_path=f"{obj_path}.mark",
            source_version=VERSION, source_path=source_path,
        )
        fields: set[str] = set()
        interactive_layer = raw.get("interactive_layer")
        if "interactive_layer" not in raw:
            fields.add("interactive_layer")
            default_fill(
                traces, source_version=VERSION, target_version="4.0",
                object_path=obj_path, field_path="interactive_layer",
                detail="absent -> null",
            )
        start_beat = scaffold.seconds_to_beat(micros_to_seconds(start_us))
        duration_beat = scaffold.seconds_to_beat(micros_to_seconds(duration_us))
        # start_us is an absolute offset; reduce it to an in-bar canonical beat.
        start_beat = start_beat - scaffold.bar_start_beat(measure)
        normalisation_trace(
            traces, source_version=VERSION, target_version="4.0",
            object_path=obj_path, field_path="start_us/duration_us",
            detail="microseconds -> canonical beats via tempo",
        )
        notes.append(
            Note(
                note_id=note_id,
                measure=measure,
                start_beat=start_beat,
                duration_beats=duration_beat,
                pitch=pitch,
                velocity=float(velocity),
                articulation=articulation,
                expression_mark=mark,
                interactive_layer=interactive_layer,
            )
        )
        note_defaulted[note_id] = fields

    events: list[ExpressionEvent] = []
    event_defaulted: dict[str, set[str]] = {}
    for index, raw in enumerate(events_raw):
        obj_path = f"root.performance_events[{index}]"
        event_id = require(
            raw, "id", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        kind = require(
            raw, "type", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        check_enum(
            kind, V3_EVENT_KINDS, field_path=f"{obj_path}.type",
            source_version=VERSION, source_path=source_path,
        )
        target_note = require(
            raw, "note_ref", expected_type=str, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        measure = require(
            raw, "measure", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        anchor_us = require(
            raw, "anchor_us", expected_type=int, path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        value = require(
            raw, "value", expected_type=(int, float), path=obj_path,
            source_version=VERSION, source_path=source_path,
        )
        fields: set[str] = set()
        duration_beats = 0.0
        depth = None
        if kind == "legato":
            length_us = require(
                raw, "length_us", expected_type=int, path=obj_path,
                source_version=VERSION, source_path=source_path,
            )
            duration_beats = scaffold.seconds_to_beat(
                micros_to_seconds(length_us)
            )
        elif kind == "pedal":
            if "depth" in raw:
                depth = float(raw["depth"])
            else:
                fields.add("depth")
                default_fill(
                    traces, source_version=VERSION, target_version="4.0",
                    object_path=obj_path, field_path="depth",
                    detail="absent -> null (boolean pedal only)",
                )

        absolute_anchor_beat = scaffold.seconds_to_beat(
            micros_to_seconds(anchor_us)
        )
        anchor_beat = absolute_anchor_beat - scaffold.bar_start_beat(measure)
        expected_seconds = scaffold.beat_to_seconds(
            scaffold.absolute_beat(measure, anchor_beat)
        )
        if abs(micros_to_seconds(anchor_us) - expected_seconds) > 1e-9:
            raise SemanticError(
                "anchor_us is inconsistent with the measure anchor",
                {
                    "file_path": source_path,
                    "version": VERSION,
                    "object_path": obj_path,
                    "anchor_us": anchor_us,
                    "expected_us": seconds_to_micros(expected_seconds),
                },
            )
        normalisation_trace(
            traces, source_version=VERSION, target_version="4.0",
            object_path=obj_path, field_path="anchor_us",
            detail="microseconds -> canonical beats via tempo",
        )
        events.append(
            ExpressionEvent(
                event_id=event_id,
                kind=kind,
                note_id=target_note,
                measure=measure,
                anchor_beat=anchor_beat,
                value=float(value),
                duration_beats=duration_beats,
                depth=depth,
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
        start_seconds = score.beat_to_seconds(
            score.absolute_beat(note.measure, note.start_beat)
        )
        duration_seconds = score.beat_to_seconds(note.duration_beats)
        item: dict[str, Any] = {
            "id": note.note_id,
            "measure": note.measure,
            "start_us": seconds_to_micros(start_seconds),
            "duration_us": seconds_to_micros(duration_seconds),
            "pitch": note.pitch,
            "velocity": note.velocity,
            "articulation": note.articulation,
            "mark": note.expression_mark,
        }
        if "interactive_layer" not in omitted:
            item["interactive_layer"] = note.interactive_layer
        notes_out.append(item)

    events_out = []
    for event in score.events:
        omitted = event_defaulted.get(event.event_id, set())
        absolute_beat = score.event_absolute_beat(event)
        anchor_us = seconds_to_micros(score.beat_to_seconds(absolute_beat))
        item: dict[str, Any] = {
            "id": event.event_id,
            "type": event.kind,
            "note_ref": event.note_id,
            "measure": event.measure,
            "anchor_us": anchor_us,
            "value": event.value,
        }
        if event.kind == "legato":
            item["length_us"] = seconds_to_micros(
                score.beat_to_seconds(event.duration_beats)
            )
        if event.kind == "pedal" and "depth" not in omitted:
            item["depth"] = event.depth
        events_out.append(item)

    return {
        "schema_version": VERSION,
        "score_info": {"name": score.title, "tempo": score.bpm},
        "meters": [
            {
                "measure": ts.measure,
                "numerator": ts.beats_per_bar,
                "denominator": ts.beat_unit,
            }
            for ts in score.time_signatures
        ],
        "score_notes": notes_out,
        "performance_events": events_out,
    }
