"""Deterministically generate multi-version score samples offline.

Run with::

    python -m scripts.generate_samples --output samples --seed 20261123

The same musical content is emitted in schema 1.0 (ticks/MIDI velocity),
2.0 (beats + milliseconds) and 3.0 (microseconds), together with a set of
deliberately broken documents used to exercise the error paths.  No network
access is required.
"""

from __future__ import annotations

import argparse
import json
import random
from pathlib import Path

from scoreworkbench.constants import PPQ

SEED = 20261123
BPM = 96.0


def beats_to_seconds(beats: float) -> float:
    return beats * 60.0 / BPM


def build_canonical_content():
    rng = random.Random(SEED)
    time_signatures = [
        {"measure": 0, "beats_per_bar": 4, "beat_unit": 4},
        {"measure": 2, "beats_per_bar": 3, "beat_unit": 4},
    ]
    notes = []
    pitch_pool = [60, 62, 64, 67, 69, 72, 71, 67, 65, 64, 62, 60]
    articulations = ["legato", "staccato", "tenuto", "accent"]
    marks = ["none", "cresc", "dim", "rit", "a_tempo", "fermata"]
    for index in range(12):
        measure = index // 4
        beat_in_bar = float(index % 4)
        notes.append(
            {
                "note_id": f"n{index:03d}",
                "measure": measure,
                "start_beat": beat_in_bar,
                "duration_beats": 1.0 if index % 3 else 0.5,
                "pitch": pitch_pool[index],
                "velocity": round(0.55 + 0.35 * rng.random(), 4),
                "articulation": articulations[index % len(articulations)],
                "expression_mark": marks[index % len(marks)],
                "interactive_layer": ("combat" if index % 4 == 3 else None),
            }
        )

    events = []
    sequence = [
        ("velocity", 0, 0, 0.62, 0.0, None),
        ("velocity", 2, 0, 0.81, 0.0, None),
        ("legato", 1, 0, 1.0, 2.0, None),
        ("pedal", 0, 0, 1.0, 0.0, 0.5),
        ("velocity", 4, 0, 0.7, 0.0, None),
        ("pedal", 4, 2, 0.0, 0.0, None),
        ("velocity", 6, 0, 0.9, 0.0, None),
        ("legato", 5, 0, 1.0, 3.0, None),
        ("pitch_bend", 8, 0, 0.25, 0.0, None),
        ("velocity", 10, 0, 0.58, 0.0, None),
    ]
    for index, (kind, note_index, beat, value, duration, depth) in enumerate(sequence):
        note = notes[note_index]
        events.append(
            {
                "event_id": f"e{index:03d}",
                "kind": kind,
                "note_id": note["note_id"],
                "measure": note["measure"],
                "anchor_beat": note["start_beat"] + beat,
                "value": value,
                "duration_beats": duration,
                "depth": depth,
            }
        )
    return time_signatures, notes, events


def bar_offset(measure: int) -> float:
    if measure < 2:
        return measure * 4.0
    return 8.0 + (measure - 2) * 3.0


def absolute_beat(measure: int, beat_in_bar: float) -> float:
    return bar_offset(measure) + beat_in_bar


def emit_v1(time_signatures, notes, events) -> dict:
    note_out = []
    for index, note in enumerate(notes):
        item = {
            "note_id": note["note_id"],
            "measure": note["measure"],
            "start_ticks": int(round(note["start_beat"] * PPQ)),
            "duration_ticks": int(round(note["duration_beats"] * PPQ)),
            "pitch": note["pitch"],
            "articulation": (
                note["articulation"] if note["articulation"] != "accent"
                else "tenuto"
            ),
        }
        if index % 2 == 1:
            item["velocity_midi"] = int(round(note["velocity"] * 127))
        if note["expression_mark"] in ("none", "cresc", "dim") and index % 3:
            item["expression_mark"] = note["expression_mark"]
        note_out.append(item)

    event_out = []
    for event in events:
        if event["kind"] == "pitch_bend":
            continue
        item = {
            "event_id": event["event_id"],
            "event_type": event["kind"],
            "target_note": event["note_id"],
            "measure": event["measure"],
            "position_ticks": int(round(event["anchor_beat"] * PPQ)),
        }
        if event["kind"] == "velocity":
            item["velocity_midi"] = int(round(event["value"] * 127))
        elif event["kind"] == "legato":
            item["length_ticks"] = int(round(event["duration_beats"] * PPQ))
        else:
            item["pedal"] = bool(round(event["value"]))
        event_out.append(item)

    v1_sigs = [
        {"measure": sig["measure"], "beats_per_bar": sig["beats_per_bar"],
         "beat_unit": sig["beat_unit"]}
        for sig in time_signatures
    ]
    return {
        "schema_version": "1.0",
        "header": {"title": "Adaptive Battle Cue", "bpm": BPM},
        "time_signatures": v1_sigs,
        "notes": note_out,
        "expression_events": event_out,
    }


def emit_v2(time_signatures, notes, events) -> dict:
    mark_camel = {
        "none": "none", "cresc": "cresc", "dim": "dim",
        "rit": "rit", "a_tempo": "aTempo", "fermata": "none",
    }
    note_out = []
    for index, note in enumerate(notes):
        item = {
            "id": note["note_id"],
            "barIndex": note["measure"],
            "beatPos": note["start_beat"],
            "beatLength": note["duration_beats"],
            "midiPitch": note["pitch"],
            "articulation": note["articulation"],
            "mark": mark_camel[note["expression_mark"]],
        }
        if index % 2 == 0:
            item["intensity"] = note["velocity"]
        note_out.append(item)

    event_out = []
    for event in events:
        if event["kind"] == "pitch_bend":
            continue
        abs_beat = absolute_beat(event["measure"], event["anchor_beat"])
        item = {
            "id": event["event_id"],
            "kind": event["kind"],
            "boundNoteId": event["note_id"],
            "barNumber": event["measure"] + 1,
            "beatInBar": event["anchor_beat"],
            "timeMs": round(beats_to_seconds(abs_beat) * 1000.0, 6),
        }
        if event["kind"] == "velocity":
            item["intensity"] = event["value"]
        elif event["kind"] == "legato":
            item["lengthBeats"] = event["duration_beats"]
        else:
            item["pedalDown"] = bool(round(event["value"]))
        event_out.append(item)

    v2_sigs = [
        {"barIndex": sig["measure"], "beatsPerBar": sig["beats_per_bar"],
         "beatUnit": sig["beat_unit"]}
        for sig in time_signatures
    ]
    return {
        "schemaVersion": "2.0",
        "meta": {"scoreTitle": "Adaptive Battle Cue", "tempoBpm": BPM},
        "barSignatures": v2_sigs,
        "noteList": note_out,
        "performanceEvents": event_out,
    }


def emit_v3(time_signatures, notes, events) -> dict:
    note_out = []
    for note in notes:
        abs_beat = absolute_beat(note["measure"], note["start_beat"])
        item = {
            "id": note["note_id"],
            "measure": note["measure"],
            "start_us": int(round(beats_to_seconds(abs_beat) * 1_000_000)),
            "duration_us": int(
                round(beats_to_seconds(note["duration_beats"]) * 1_000_000)
            ),
            "pitch": note["pitch"],
            "velocity": note["velocity"],
            "articulation": note["articulation"],
            "mark": note["expression_mark"],
        }
        if note["interactive_layer"] is not None:
            item["interactive_layer"] = note["interactive_layer"]
        note_out.append(item)

    event_out = []
    for event in events:
        abs_beat = absolute_beat(event["measure"], event["anchor_beat"])
        item = {
            "id": event["event_id"],
            "type": event["kind"],
            "note_ref": event["note_id"],
            "measure": event["measure"],
            "anchor_us": int(round(beats_to_seconds(abs_beat) * 1_000_000)),
            "value": event["value"],
        }
        if event["kind"] == "legato":
            item["length_us"] = int(
                round(beats_to_seconds(event["duration_beats"]) * 1_000_000)
            )
        if event["kind"] == "pedal" and event["depth"] is not None:
            item["depth"] = event["depth"]
        event_out.append(item)

    v3_sigs = [
        {"measure": sig["measure"], "numerator": sig["beats_per_bar"],
         "denominator": sig["beat_unit"]}
        for sig in time_signatures
    ]
    return {
        "schema_version": "3.0",
        "score_info": {"name": "Adaptive Battle Cue", "tempo": BPM},
        "meters": v3_sigs,
        "score_notes": note_out,
        "performance_events": event_out,
    }


def write(path: Path, document: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(document, handle, indent=2, sort_keys=True)
        handle.write("\n")


def generate_broken(output: Path, v1: dict, v2: dict, v3: dict) -> None:
    broken_dir = output / "broken"

    corrupted = json.dumps(v1)
    write(broken_dir / "corrupted.json", {"placeholder": True})
    with open(broken_dir / "corrupted.json", "w", encoding="utf-8") as handle:
        handle.write(corrupted[: len(corrupted) // 2])

    unknown = dict(v2)
    unknown["schemaVersion"] = "9.9"
    write(broken_dir / "unknown_version.json", unknown)

    missing_field = json.loads(json.dumps(v3))
    del missing_field["score_notes"][0]["pitch"]
    write(broken_dir / "missing_required.json", missing_field)

    dangle = json.loads(json.dumps(v1))
    dangle["expression_events"][0]["target_note"] = "n-does-not-exist"
    write(broken_dir / "dangling_event.json", dangle)

    semantic = json.loads(json.dumps(v2))
    semantic["performanceEvents"][0]["timeMs"] = 999999.0
    write(broken_dir / "semantic_conflict.json", semantic)

    duplicate = json.loads(json.dumps(v3))
    duplicate["score_notes"][1]["id"] = duplicate["score_notes"][0]["id"]
    write(broken_dir / "duplicate_id.json", duplicate)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", default="samples")
    parser.add_argument("--seed", type=int, default=SEED)
    args = parser.parse_args()
    if args.seed != SEED:
        raise SystemExit("Sample generation seed is fixed for determinism")
    output = Path(args.output)
    time_signatures, notes, events = build_canonical_content()
    v1 = emit_v1(time_signatures, notes, events)
    v2 = emit_v2(time_signatures, notes, events)
    v3 = emit_v3(time_signatures, notes, events)
    write(output / "score_v1.json", v1)
    write(output / "score_v2.json", v2)
    write(output / "score_v3.json", v3)
    generate_broken(output, v1, v2, v3)
    print(f"Generated samples in {output.resolve()}")


if __name__ == "__main__":
    main()
