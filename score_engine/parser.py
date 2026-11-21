from __future__ import annotations

import json
from collections.abc import Mapping
from fractions import Fraction
from pathlib import Path
from typing import Any

from .errors import score_invalid
from .model import (
    ARTICULATIONS,
    ExpressionAnchor,
    Note,
    Part,
    PedalSpan,
    Score,
    TempoSegment,
    TimeSignature,
)
from .util import as_fraction


_UNIT_FACTORS = {
    "whole": Fraction(4),
    "whole_note": Fraction(4),
    "whole-notes": Fraction(4),
    "semibreve": Fraction(4),
    "half": Fraction(2),
    "half_note": Fraction(2),
    "minim": Fraction(2),
    "quarter": Fraction(1),
    "quarter_note": Fraction(1),
    "quarter_beats": Fraction(1),
    "beat": Fraction(1),
    "beats": Fraction(1),
    "eighth": Fraction(1, 2),
    "eighth_note": Fraction(1, 2),
    "quaver": Fraction(1, 2),
    "sixteenth": Fraction(1, 4),
    "sixteenth_note": Fraction(1, 4),
    "semiquaver": Fraction(1, 4),
    "32nd": Fraction(1, 8),
}


def _field(mapping: Mapping[str, Any], key: str, *, note_id: str | None, part_id: str | None = None) -> Any:
    if key not in mapping or mapping[key] is None:
        raise score_invalid(
            f"缺少必需字段 {key}",
            note_id=note_id,
            field=key,
            part_id=part_id,
        )
    return mapping[key]


def _string_field(
    mapping: Mapping[str, Any],
    key: str,
    *,
    default: str | None = None,
    note_id: str | None,
    part_id: str | None = None,
) -> str:
    if key not in mapping or mapping[key] is None:
        if default is not None:
            return default
        raise score_invalid(
            f"缺少必需字段 {key}",
            note_id=note_id,
            field=key,
            part_id=part_id,
        )
    value = mapping[key]
    if not isinstance(value, str) or not value:
        raise score_invalid(
            f"字段 {key} 必须是非空字符串",
            note_id=note_id,
            field=key,
            part_id=part_id,
        )
    return value


def _version(value: Any) -> int:
    if value is None:
        raise score_invalid("缺少版本号字段 format_version", note_id="-", field="format_version")
    text = str(value).strip().lower().removesuffix(".0")
    if text in {"1", "v1"}:
        return 1
    if text in {"2", "v2"}:
        return 2
    if text in {"3", "v3"}:
        return 3
    raise score_invalid(
        f"未知乐谱版本号: {value!r}",
        note_id="-",
        field="format_version",
    )


def _unit_factor(value: Any, *, default: str = "quarter_beats", note_id: str | None = None, field: str = "unit") -> Fraction:
    unit = default if value is None else str(value).strip().lower()
    if unit in _UNIT_FACTORS:
        return _UNIT_FACTORS[unit]
    raise score_invalid(
        f"未知时值单位: {value!r}",
        note_id=note_id,
        field=field,
    )


def _time_value(
    value: Any,
    *,
    unit: Any,
    ticks_per_quarter: Any = None,
    note_id: str | None,
    field: str,
    part_id: str | None = None,
) -> Fraction:
    if isinstance(value, Mapping):
        inner_unit = value.get("unit")
        inner_value = _field(value, "value", note_id=note_id, part_id=part_id)
        return _time_value(
            inner_value,
            unit=inner_unit,
            ticks_per_quarter=ticks_per_quarter,
            note_id=note_id,
            field=field,
            part_id=part_id,
        )
    number = as_fraction(value, field=field, note_id=note_id, part_id=part_id)
    unit_text = "quarter_beats" if unit is None else str(unit).strip().lower()
    if unit_text in {"tick", "ticks", "ppq"}:
        if ticks_per_quarter is None:
            raise score_invalid(
                "使用 tick 单位时必须声明 ticks_per_quarter",
                note_id=note_id,
                field=field,
                part_id=part_id,
            )
        resolution = as_fraction(
            ticks_per_quarter,
            field="ticks_per_quarter",
            note_id=note_id,
            part_id=part_id,
        )
        if resolution <= 0:
            raise score_invalid(
                "ticks_per_quarter 必须大于零",
                note_id=note_id,
                field="ticks_per_quarter",
                part_id=part_id,
            )
        return number / resolution
    return number * _unit_factor(unit_text, note_id=note_id, field=field)


def _pitch(value: Any, *, note_id: str, field: str = "pitch") -> int:
    if isinstance(value, str):
        names = {"C": 0, "D": 2, "E": 4, "F": 5, "G": 7, "A": 9, "B": 11}
        text = value.strip()
        if len(text) >= 2 and text[0].upper() in names:
            try:
                octave = int(text[1:])
                return (octave + 1) * 12 + names[text[0].upper()]
            except ValueError:
                pass
    if isinstance(value, bool) or not isinstance(value, int):
        raise score_invalid(f"音高必须是 MIDI 整数: {value!r}", note_id=note_id, field=field)
    return value


def _meter_object(value: Any, *, start: Fraction, default_id: str, note_id: str = "-") -> TimeSignature:
    if not isinstance(value, Mapping):
        raise score_invalid("拍号必须是对象", note_id=note_id, field="time_signature")
    numerator = _field(value, "numerator", note_id=note_id)
    denominator = _field(value, "denominator", note_id=note_id)
    if isinstance(numerator, bool) or not isinstance(numerator, int) or numerator <= 0:
        raise score_invalid("拍号分子必须是正整数", note_id=note_id, field="time_signature.numerator")
    if isinstance(denominator, bool) or not isinstance(denominator, int) or denominator <= 0:
        raise score_invalid("拍号分母必须是正整数", note_id=note_id, field="time_signature.denominator")
    if denominator & (denominator - 1):
        raise score_invalid("拍号分母必须是 2 的幂", note_id=note_id, field="time_signature.denominator")
    return TimeSignature(
        id=str(value.get("id", default_id)),
        start=start,
        numerator=numerator,
        denominator=denominator,
    )


def _tempo_value(value: Any, *, note_id: str = "-", field: str = "tempo_bpm") -> Fraction:
    bpm = as_fraction(value, field=field, note_id=note_id)
    if bpm <= 0:
        raise score_invalid("速度必须大于零", note_id=note_id, field=field)
    return bpm


def _velocity(value: Any, *, note_id: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise score_invalid("力度必须是整数", note_id=note_id, field="velocity")
    if not 1 <= value <= 127:
        raise score_invalid("力度必须位于 1..127", note_id=note_id, field="velocity")
    return value


def _articulation(value: Any, *, note_id: str) -> str:
    text = str(value).strip().lower()
    if text not in ARTICULATIONS:
        raise score_invalid(
            f"未知连奏标记: {value!r}",
            note_id=note_id,
            field="articulation",
        )
    return text


def _parse_pedal(
    value: Mapping[str, Any],
    *,
    index: int,
    offset: Fraction,
    note_id: str,
    part_id: str,
    start_unit: Any = "quarter_beats",
) -> PedalSpan:
    pedal_id = str(value.get("id", f"{part_id}-PEDAL-{index + 1:03d}"))
    start_raw = value.get("start", value.get("start_beat"))
    end_raw = value.get("end", value.get("end_beat"))
    if end_raw is None and "duration" in value:
        start = _time_value(
            start_raw,
            unit=start_unit,
            note_id=note_id,
            field="pedal.start",
            part_id=part_id,
        )
        duration = _time_value(
            value["duration"],
            unit=value.get("unit", start_unit),
            note_id=note_id,
            field="pedal.duration",
            part_id=part_id,
        )
        end = start + duration
    else:
        start = _time_value(
            start_raw,
            unit=value.get("unit", start_unit),
            note_id=note_id,
            field="pedal.start",
            part_id=part_id,
        )
        end = _time_value(
            end_raw,
            unit=value.get("unit", start_unit),
            note_id=note_id,
            field="pedal.end",
            part_id=part_id,
        )
    pedal_value = as_fraction(
        value.get("value", Fraction(4, 5)),
        field="pedal.value",
        note_id=note_id,
        part_id=part_id,
    )
    return PedalSpan(
        id=pedal_id,
        start=start + offset,
        end=end + offset,
        value=pedal_value,
    )


def _parse_v1(payload: Mapping[str, Any], source_name: str) -> Score:
    meter_raw = payload.get("time_signature", payload.get("meter"))
    if meter_raw is None:
        raise score_invalid("v1 缺少 time_signature", note_id="-", field="time_signature")
    offset_seconds = as_fraction(
        payload.get("start_offset_seconds", 0),
        field="start_offset_seconds",
        note_id="-",
    )
    bpm = _tempo_value(payload.get("tempo_bpm", 120), field="tempo_bpm")
    offset = offset_seconds * bpm / 60
    unit = payload.get("duration_unit", payload.get("time_unit", "whole_note"))
    factor = _unit_factor(unit, note_id="-", field="duration_unit")
    grid = as_fraction(payload.get("grid_resolution", Fraction(1, 8)), field="grid_resolution", note_id="-")
    time_signature = _meter_object(meter_raw, start=Fraction(0), default_id="meter-1")
    notes_raw = _field(payload, "notes", note_id="-")
    if not isinstance(notes_raw, list):
        raise score_invalid("notes 必须是数组", note_id="-", field="notes")
    notes: list[Note] = []
    provenance: dict[str, str] = {}
    seen_ids: set[str] = set()
    for index, raw in enumerate(notes_raw):
        note_id = str(raw.get("id", f"P1-N{index + 1:03d}")) if isinstance(raw, Mapping) else f"P1-N{index + 1:03d}"
        if note_id in seen_ids:
            raise score_invalid("音符编号重复", note_id=note_id, field="id")
        if not isinstance(raw, Mapping):
            raise score_invalid("音符必须是对象", note_id=note_id, field="note")
        pitch = _pitch(_field(raw, "pitch", note_id=note_id), note_id=note_id)
        start = as_fraction(_field(raw, "start", note_id=note_id), field="start", note_id=note_id) * factor
        duration = as_fraction(_field(raw, "duration", note_id=note_id), field="duration", note_id=note_id) * factor
        velocity_value = raw.get("velocity")
        if velocity_value is None:
            velocity = 80
            provenance[f"{note_id}.velocity"] = "default_v1_velocity_80"
        else:
            velocity = _velocity(velocity_value, note_id=note_id)
        articulation_value = raw.get("articulation")
        if articulation_value is None:
            articulation = "normal"
            provenance[f"{note_id}.articulation"] = "default_v1_articulation_normal"
        else:
            articulation = _articulation(articulation_value, note_id=note_id)
        notes.append(
            Note(
                id=note_id,
                pitch=pitch,
                start=start + offset,
                duration=duration,
                velocity=velocity,
                articulation=articulation,
            )
        )
        seen_ids.add(note_id)
    if not notes:
        raise score_invalid("v1 乐谱至少需要一个音符", note_id="-", field="notes")
    min_pitch = min(note.pitch for note in notes)
    max_pitch = max(note.pitch for note in notes)
    provenance["P1.pitch_range"] = "derived_from_observed_v1_pitches"
    pedals = _derive_v1_phrase_pedals(notes, offset, time_signature)
    for pedal in pedals:
        provenance[f"{pedal.id}"] = "derived_v1_bar_phrase_pedal"
    return Score(
        title=str(payload.get("title", source_name)),
        composer=str(payload.get("composer", "")),
        start_offset=offset,
        grid_resolution=grid,
        time_signatures=(time_signature,),
        tempo_segments=(TempoSegment(id="tempo-1", start=Fraction(0), bpm=bpm, end=None),),
        parts=(
            Part(
                id="P1",
                name=str(payload.get("part_name", "Voice 1")),
                instrument=str(payload.get("instrument", "unknown")),
                min_pitch=min_pitch,
                max_pitch=max_pitch,
                notes=tuple(notes),
                pedals=tuple(pedals),
            ),
        ),
        expression=(),
        source_version=1,
        provenance=provenance,
    )


def _derive_v1_phrase_pedals(
    notes: list[Note],
    offset: Fraction,
    meter: TimeSignature,
) -> list[PedalSpan]:
    spans: list[PedalSpan] = []
    if not notes:
        return spans
    bar_length = meter.bar_length
    grouped: dict[int, list[Note]] = {}
    for note in notes:
        relative = note.start - offset
        if relative < 0:
            continue
        bar = int(relative // bar_length)
        grouped.setdefault(bar, []).append(note)
    for bar in sorted(grouped):
        bar_notes = grouped[bar]
        start = min(note.start for note in bar_notes)
        end = max(note.end for note in bar_notes)
        bar_end = offset + (bar + 1) * bar_length
        end = min(end, bar_end)
        if end > start:
            spans.append(
                PedalSpan(
                    id=f"P1-PEDAL-{len(spans) + 1:03d}",
                    start=start,
                    end=end,
                    value=Fraction(4, 5),
                )
            )
    return spans


def _parse_v2(payload: Mapping[str, Any], source_name: str) -> Score:
    offset = as_fraction(
        payload.get("origin_offset_beats", payload.get("start_offset", 0)),
        field="origin_offset_beats",
        note_id="-",
    )
    grid = as_fraction(payload.get("grid_resolution", Fraction(1, 8)), field="grid_resolution", note_id="-")
    unit = payload.get("time_unit", "quarter")
    ticks_per_quarter = payload.get("ticks_per_quarter")
    meter_changes_raw = payload.get("meter_changes")
    signatures: list[TimeSignature] = []
    if meter_changes_raw is None:
        meter_raw = payload.get("time_signature", payload.get("meter"))
        if meter_raw is None:
            raise score_invalid("v2 缺少 meter_changes", note_id="-", field="meter_changes")
        signatures.append(_meter_object(meter_raw, start=Fraction(0), default_id="meter-1"))
    else:
        if not isinstance(meter_changes_raw, list) or not meter_changes_raw:
            raise score_invalid("meter_changes 必须是非空数组", note_id="-", field="meter_changes")
        for index, raw in enumerate(meter_changes_raw):
            if not isinstance(raw, Mapping):
                raise score_invalid("拍号变化必须是对象", note_id="-", field=f"meter_changes[{index}]")
            start = _time_value(
                _field(raw, "beat", note_id="-"),
                unit=raw.get("unit", unit),
                ticks_per_quarter=ticks_per_quarter,
                note_id="-",
                field=f"meter_changes[{index}].beat",
            )
            signatures.append(
                _meter_object(
                    raw,
                    start=start,
                    default_id=f"meter-{index + 1}",
                )
            )
    bpm = _tempo_value(payload.get("tempo_bpm", 120), field="tempo_bpm")
    tracks_raw = payload.get("tracks", payload.get("parts"))
    if not isinstance(tracks_raw, list) or not tracks_raw:
        raise score_invalid("v2 缺少 tracks", note_id="-", field="tracks")
    parts: list[Part] = []
    provenance: dict[str, str] = {}
    global_note_ids: set[str] = set()
    for part_index, track in enumerate(tracks_raw):
        if not isinstance(track, Mapping):
            raise score_invalid("track 必须是对象", note_id="-", field=f"tracks[{part_index}]")
        part_id = _string_field(track, "id", default=f"P{part_index + 1}", note_id="-")
        notes_raw = _field(track, "notes", note_id="-", part_id=part_id)
        if not isinstance(notes_raw, list):
            raise score_invalid("notes 必须是数组", note_id="-", field="notes", part_id=part_id)
        notes: list[Note] = []
        for note_index, raw in enumerate(notes_raw):
            note_id = (
                str(raw.get("id", f"{part_id}-N{note_index + 1:03d}"))
                if isinstance(raw, Mapping)
                else f"{part_id}-N{note_index + 1:03d}"
            )
            if note_id in global_note_ids:
                raise score_invalid("音符编号重复", note_id=note_id, field="id", part_id=part_id)
            if not isinstance(raw, Mapping):
                raise score_invalid("音符必须是对象", note_id=note_id, field="note", part_id=part_id)
            pitch = _pitch(_field(raw, "pitch", note_id=note_id, part_id=part_id), note_id=note_id)
            start = _time_value(
                raw.get("start_beat", raw.get("start")),
                unit=raw.get("unit", unit),
                ticks_per_quarter=ticks_per_quarter,
                note_id=note_id,
                field="start_beat",
                part_id=part_id,
            )
            duration = _time_value(
                raw.get("duration_beats", raw.get("duration")),
                unit=raw.get("unit", unit),
                ticks_per_quarter=ticks_per_quarter,
                note_id=note_id,
                field="duration_beats",
                part_id=part_id,
            )
            velocity_raw = raw.get("velocity")
            if velocity_raw is None:
                velocity = 80
                provenance[f"{note_id}.velocity"] = "default_v2_velocity_80"
            else:
                velocity = _velocity(velocity_raw, note_id=note_id)
            articulation_raw = raw.get("articulation")
            if articulation_raw is None:
                articulation = "normal"
                provenance[f"{note_id}.articulation"] = "default_v2_articulation_normal"
            else:
                articulation = _articulation(articulation_raw, note_id=note_id)
            notes.append(
                Note(
                    id=note_id,
                    pitch=pitch,
                    start=start + offset,
                    duration=duration,
                    velocity=velocity,
                    articulation=articulation,
                )
            )
            global_note_ids.add(note_id)
        if not notes:
            raise score_invalid("track 至少需要一个音符", note_id="-", field="notes", part_id=part_id)
        pedals_raw = track.get("pedal_spans")
        pedals: list[PedalSpan] = []
        if pedals_raw is None:
            for note in notes:
                provenance[f"{note.id}.pedal_spans"] = "v2_absent_pedal_spans_none"
        elif not isinstance(pedals_raw, list):
            raise score_invalid("pedal_spans 必须是数组", note_id="-", field="pedal_spans", part_id=part_id)
        else:
            for index, raw in enumerate(pedals_raw):
                if not isinstance(raw, Mapping):
                    raise score_invalid("踏板区间必须是对象", note_id="-", field=f"pedal_spans[{index}]", part_id=part_id)
                pedals.append(
                    _parse_pedal(
                        raw,
                        index=index,
                        offset=offset,
                        note_id="-",
                        part_id=part_id,
                        start_unit=unit,
                    )
                )
        pitch_range = track.get("pitch_range")
        if pitch_range is None:
            min_pitch = min(note.pitch for note in notes)
            max_pitch = max(note.pitch for note in notes)
            provenance[f"{part_id}.pitch_range"] = "derived_from_observed_v2_pitches"
        else:
            if not isinstance(pitch_range, Mapping):
                raise score_invalid("pitch_range 必须是对象", note_id="-", field="pitch_range", part_id=part_id)
            min_pitch = _pitch(_field(pitch_range, "min", note_id="-", part_id=part_id), note_id="-", field="pitch_range.min")
            max_pitch = _pitch(_field(pitch_range, "max", note_id="-", part_id=part_id), note_id="-", field="pitch_range.max")
        parts.append(
            Part(
                id=part_id,
                name=str(track.get("name", part_id)),
                instrument=str(track.get("instrument", "unknown")),
                min_pitch=min_pitch,
                max_pitch=max_pitch,
                notes=tuple(notes),
                pedals=tuple(pedals),
            )
        )
    return Score(
        title=str(payload.get("title", source_name)),
        composer=str(payload.get("composer", "")),
        start_offset=offset,
        grid_resolution=grid,
        time_signatures=tuple(signatures),
        tempo_segments=(TempoSegment(id="tempo-1", start=Fraction(0), bpm=bpm, end=None),),
        parts=tuple(parts),
        expression=(),
        source_version=2,
        provenance=provenance,
    )


def _parse_v3(payload: Mapping[str, Any], source_name: str) -> Score:
    offset_raw = payload.get("start_offset", payload.get("origin_offset", 0))
    if isinstance(offset_raw, Mapping):
        offset = _time_value(
            offset_raw,
            unit=offset_raw.get("unit", "quarter_beats"),
            note_id="-",
            field="start_offset",
        )
    else:
        offset = as_fraction(offset_raw, field="start_offset", note_id="-")
    grid_raw = payload.get("grid_resolution", Fraction(1, 8))
    if isinstance(grid_raw, Mapping):
        grid = _time_value(
            grid_raw,
            unit=grid_raw.get("unit", "quarter_beats"),
            note_id="-",
            field="grid_resolution",
        )
    else:
        grid = as_fraction(grid_raw, field="grid_resolution", note_id="-")
    signatures_raw = _field(payload, "time_signatures", note_id="-")
    if not isinstance(signatures_raw, list) or not signatures_raw:
        raise score_invalid("time_signatures 必须是非空数组", note_id="-", field="time_signatures")
    signatures: list[TimeSignature] = []
    for index, raw in enumerate(signatures_raw):
        if not isinstance(raw, Mapping):
            raise score_invalid("拍号必须是对象", note_id="-", field=f"time_signatures[{index}]")
        start = _time_value(
            _field(raw, "start", note_id="-"),
            unit=raw.get("unit", "quarter_beats"),
            note_id="-",
            field=f"time_signatures[{index}].start",
        )
        signatures.append(_meter_object(raw, start=start, default_id=f"meter-{index + 1}"))
    tempo_raw = _field(payload, "tempo_segments", note_id="-")
    if not isinstance(tempo_raw, list) or not tempo_raw:
        raise score_invalid("tempo_segments 必须是非空数组", note_id="-", field="tempo_segments")
    tempo_segments: list[TempoSegment] = []
    for index, raw in enumerate(tempo_raw):
        if not isinstance(raw, Mapping):
            raise score_invalid("速度段必须是对象", note_id="-", field=f"tempo_segments[{index}]")
        start = _time_value(
            _field(raw, "start", note_id="-"),
            unit=raw.get("unit", "quarter_beats"),
            note_id="-",
            field=f"tempo_segments[{index}].start",
        )
        end_raw = raw.get("end")
        end = (
            None
            if end_raw is None
            else _time_value(
                end_raw,
                unit=raw.get("unit", "quarter_beats"),
                note_id="-",
                field=f"tempo_segments[{index}].end",
            )
        )
        bpm = _tempo_value(
            _field(raw, "bpm", note_id="-"),
            field=f"tempo_segments[{index}].bpm",
        )
        tempo_segments.append(
            TempoSegment(
                id=str(raw.get("id", f"tempo-{index + 1}")),
                start=start,
                end=end,
                bpm=bpm,
            )
        )
    parts_raw = _field(payload, "parts", note_id="-")
    if not isinstance(parts_raw, list) or not parts_raw:
        raise score_invalid("parts 必须是非空数组", note_id="-", field="parts")
    parts: list[Part] = []
    global_note_ids: set[str] = set()
    provenance: dict[str, str] = dict(payload.get("provenance", {}))
    for part_index, raw_part in enumerate(parts_raw):
        if not isinstance(raw_part, Mapping):
            raise score_invalid("声部必须是对象", note_id="-", field=f"parts[{part_index}]")
        part_id = _string_field(raw_part, "id", default=f"P{part_index + 1}", note_id="-")
        range_raw = _field(raw_part, "pitch_range", note_id="-", part_id=part_id)
        if not isinstance(range_raw, Mapping):
            raise score_invalid("pitch_range 必须是对象", note_id="-", field="pitch_range", part_id=part_id)
        min_pitch = _pitch(
            _field(range_raw, "min", note_id="-", part_id=part_id),
            note_id="-",
            field="pitch_range.min",
        )
        max_pitch = _pitch(
            _field(range_raw, "max", note_id="-", part_id=part_id),
            note_id="-",
            field="pitch_range.max",
        )
        notes_raw = _field(raw_part, "notes", note_id="-", part_id=part_id)
        if not isinstance(notes_raw, list) or not notes_raw:
            raise score_invalid("notes 必须是非空数组", note_id="-", field="notes", part_id=part_id)
        notes: list[Note] = []
        for note_index, raw_note in enumerate(notes_raw):
            note_id = (
                str(raw_note.get("id", f"{part_id}-N{note_index + 1:03d}"))
                if isinstance(raw_note, Mapping)
                else f"{part_id}-N{note_index + 1:03d}"
            )
            if note_id in global_note_ids:
                raise score_invalid("音符编号重复", note_id=note_id, field="id", part_id=part_id)
            if not isinstance(raw_note, Mapping):
                raise score_invalid("音符必须是对象", note_id=note_id, field="note", part_id=part_id)
            pitch = _pitch(
                _field(raw_note, "pitch", note_id=note_id, part_id=part_id),
                note_id=note_id,
            )
            start_raw = raw_note.get("start", raw_note.get("start_beat"))
            duration_raw = raw_note.get("duration", raw_note.get("duration_beats"))
            start = _time_value(
                _field({"start": start_raw}, "start", note_id=note_id, part_id=part_id),
                unit=raw_note.get("unit", "quarter_beats"),
                note_id=note_id,
                field="start",
                part_id=part_id,
            )
            duration = _time_value(
                _field({"duration": duration_raw}, "duration", note_id=note_id, part_id=part_id),
                unit=raw_note.get("unit", "quarter_beats"),
                note_id=note_id,
                field="duration",
                part_id=part_id,
            )
            velocity = _velocity(_field(raw_note, "velocity", note_id=note_id, part_id=part_id), note_id=note_id)
            articulation = _articulation(
                _field(raw_note, "articulation", note_id=note_id, part_id=part_id),
                note_id=note_id,
            )
            notes.append(
                Note(
                    id=note_id,
                    pitch=pitch,
                    start=start + offset,
                    duration=duration,
                    velocity=velocity,
                    articulation=articulation,
                )
            )
            global_note_ids.add(note_id)
        pedals_raw = raw_part.get("pedals", raw_part.get("pedal_spans", []))
        if not isinstance(pedals_raw, list):
            raise score_invalid("pedals 必须是数组", note_id="-", field="pedals", part_id=part_id)
        pedals = tuple(
            _parse_pedal(
                raw,
                index=index,
                offset=offset,
                note_id="-",
                part_id=part_id,
                start_unit="quarter_beats",
            )
            for index, raw in enumerate(pedals_raw)
            if isinstance(raw, Mapping)
        )
        if len(pedals) != len(pedals_raw):
            raise score_invalid("踏板区间必须是对象", note_id="-", field="pedals", part_id=part_id)
        parts.append(
            Part(
                id=part_id,
                name=str(raw_part.get("name", part_id)),
                instrument=str(raw_part.get("instrument", "unknown")),
                min_pitch=min_pitch,
                max_pitch=max_pitch,
                notes=tuple(notes),
                pedals=pedals,
            )
        )
    expression_raw = _field(payload, "expression", note_id="-")
    if not isinstance(expression_raw, list):
        raise score_invalid("expression 必须是数组", note_id="-", field="expression")
    expression: list[ExpressionAnchor] = []
    for index, raw in enumerate(expression_raw):
        if not isinstance(raw, Mapping):
            raise score_invalid("表情锚点必须是对象", note_id="-", field=f"expression[{index}]")
        start = _time_value(
            _field(raw, "start", note_id="-"),
            unit=raw.get("unit", "quarter_beats"),
            note_id="-",
            field=f"expression[{index}].start",
        )
        value = as_fraction(
            _field(raw, "value", note_id="-"),
            field=f"expression[{index}].value",
            note_id="-",
        )
        expression.append(
            ExpressionAnchor(
                id=str(raw.get("id", f"expr-{index + 1}")),
                start=start + offset,
                value=value,
                part_id=raw.get("part_id"),
            )
        )
    return Score(
        title=str(payload.get("title", source_name)),
        composer=str(payload.get("composer", "")),
        start_offset=offset,
        grid_resolution=grid,
        time_signatures=tuple(signatures),
        tempo_segments=tuple(tempo_segments),
        parts=tuple(parts),
        expression=tuple(expression),
        source_version=3,
        provenance=provenance,
    )


def parse_score(source: str | Path | Mapping[str, Any]) -> Score:
    if isinstance(source, Mapping):
        payload = dict(source)
        source_name = str(payload.get("title", "inline-score"))
    else:
        path = Path(source)
        try:
            with path.open("r", encoding="utf-8") as handle:
                payload = json.load(handle)
        except (OSError, json.JSONDecodeError) as exc:
            raise score_invalid(
                f"无法读取乐谱 JSON: {exc}",
                note_id="-",
                field="file",
            ) from exc
        source_name = path.stem
    if not isinstance(payload, Mapping):
        raise score_invalid("乐谱根节点必须是对象", note_id="-", field="root")
    if "format_version" not in payload and "version" not in payload:
        raise score_invalid("缺少版本号字段 format_version", note_id="-", field="format_version")
    version = _version(payload.get("format_version", payload.get("version")))
    from .validation import validate_score

    if version == 1:
        score = _parse_v1(payload, source_name)
    elif version == 2:
        score = _parse_v2(payload, source_name)
    else:
        score = _parse_v3(payload, source_name)
    _normalise_tempo(score)
    validate_score(score)
    return _normalise_tempo(score)


def _normalise_tempo(score: Score) -> Score:
    from dataclasses import replace

    segments = sorted(score.tempo_segments, key=lambda segment: (segment.start, segment.id))
    normalised: list[TempoSegment] = []
    for index, segment in enumerate(segments):
        expected_end = segments[index + 1].start if index + 1 < len(segments) else None
        if segment.end is not None and expected_end is not None and segment.end != expected_end:
            raise score_invalid(
                "速度段结束点必须与下一速度段起点连续",
                note_id="-",
                field=f"tempo_segments[{index}].end",
            )
        normalised.append(replace(segment, end=expected_end))
    return replace(score, tempo_segments=tuple(normalised))

