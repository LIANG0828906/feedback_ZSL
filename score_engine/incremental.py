from __future__ import annotations

import copy
from dataclasses import dataclass, replace
from fractions import Fraction
from typing import Any

from .errors import score_invalid
from .model import (
    ExpressionAnchor,
    Note,
    NoteEdit,
    Part,
    PedalSpan,
    Score,
    SolverParameters,
    TempoSegment,
    replace_score_part,
)
from .publish import PublishedSnapshot, VersionedPublisher
from .solver import ExpressionCurve, PerformanceSolver, TempoMap
from .util import fraction_text, sha256_json
from .validation import validate_score


@dataclass
class IncrementalResult:
    sequence: dict[str, Any]
    affected_note_ids: tuple[str, ...]
    reused_note_ids: tuple[str, ...]
    affected_count: int
    reused_count: int
    reasons: dict[str, str]
    edits: list[dict[str, Any]]
    snapshot: PublishedSnapshot

    def trace_record(self, *, version: int | None = None) -> dict[str, Any]:
        return {
            "event": "incremental_solve",
            "version": self.snapshot.version if version is None else version,
            "affected_note_count": self.affected_count,
            "reused_note_count": self.reused_count,
            "affected_note_ids": list(self.affected_note_ids),
            "reused_note_ids": list(self.reused_note_ids),
            "reasons": dict(sorted(self.reasons.items())),
            "edits": self.edits,
            "content_hash": self.snapshot.content_hash,
        }


def _find_part(score: Score, part_id: str) -> Part:
    for part in score.parts:
        if part.id == part_id:
            return part
    raise score_invalid("编辑引用了不存在声部", note_id="-", field="part_id", part_id=part_id)


def _replace_note_velocity(score: Score, edit: NoteEdit) -> Score:
    if edit.part_id is None or edit.note_id is None:
        raise score_invalid("力度编辑必须提供 part_id 与 note_id", note_id=edit.note_id, field="edit")
    part = _find_part(score, edit.part_id)
    if isinstance(edit.value, bool) or not isinstance(edit.value, int) or not 1 <= edit.value <= 127:
        raise score_invalid("编辑后的力度必须位于 1..127", note_id=edit.note_id, field="velocity", part_id=part.id)
    found = False
    notes: list[Note] = []
    for note in part.notes:
        if note.id == edit.note_id:
            notes.append(replace(note, velocity=edit.value))
            found = True
        else:
            notes.append(note)
    if not found:
        raise score_invalid("力度编辑引用了不存在音符", note_id=edit.note_id, field="note_id", part_id=part.id)
    return replace_score_part(score, replace(part, notes=tuple(notes)))


def _replace_expression(score: Score, edit: NoteEdit) -> Score:
    if edit.index is None or not 0 <= edit.index < len(score.expression):
        raise score_invalid("表情编辑索引越界", note_id="-", field=f"expression[{edit.index}]")
    old = score.expression[edit.index]
    if isinstance(edit.value, ExpressionAnchor):
        new_anchor = edit.value
    elif isinstance(edit.value, dict):
        new_anchor = ExpressionAnchor(
            id=str(edit.value.get("id", old.id)),
            start=Fraction(str(edit.value.get("start", old.start))),
            value=Fraction(str(edit.value.get("value", old.value))),
            part_id=edit.value.get("part_id", old.part_id),
        )
    else:
        new_anchor = replace(old, value=Fraction(str(edit.value)))
    anchors = list(score.expression)
    anchors[edit.index] = new_anchor
    return replace(score, expression=tuple(anchors))


def _replace_tempo(score: Score, edit: NoteEdit) -> Score:
    if edit.index is None or not 0 <= edit.index < len(score.tempo_segments):
        raise score_invalid("速度编辑索引越界", note_id="-", field=f"tempo_segments[{edit.index}]")
    old = score.tempo_segments[edit.index]
    if isinstance(edit.value, TempoSegment):
        new_segment = edit.value
    elif isinstance(edit.value, dict):
        new_segment = TempoSegment(
            id=str(edit.value.get("id", old.id)),
            start=Fraction(str(edit.value.get("start", old.start))),
            end=(
                None
                if edit.value.get("end", old.end) is None
                else Fraction(str(edit.value.get("end", old.end)))
            ),
            bpm=Fraction(str(edit.value.get("bpm", old.bpm))),
        )
    else:
        new_segment = replace(old, bpm=Fraction(str(edit.value)))
    segments = sorted(
        [new_segment if index == edit.index else segment for index, segment in enumerate(score.tempo_segments)],
        key=lambda segment: (segment.start, segment.id),
    )
    normalised: list[TempoSegment] = []
    for index, segment in enumerate(segments):
        expected_end = segments[index + 1].start if index + 1 < len(segments) else None
        normalised.append(replace(segment, end=expected_end))
    return replace(score, tempo_segments=tuple(normalised))


def _replace_pedal(score: Score, edit: NoteEdit) -> Score:
    if edit.part_id is None:
        raise score_invalid("踏板编辑必须提供 part_id", note_id="-", field="part_id")
    part = _find_part(score, edit.part_id)
    if edit.index is None or not 0 <= edit.index < len(part.pedals):
        raise score_invalid("踏板编辑索引越界", note_id="-", field=f"pedals[{edit.index}]", part_id=part.id)
    pedals = list(part.pedals)
    if edit.value is None:
        pedals.pop(edit.index)
    elif isinstance(edit.value, PedalSpan):
        pedals[edit.index] = edit.value
    elif isinstance(edit.value, dict):
        old = pedals[edit.index]
        pedals[edit.index] = PedalSpan(
            id=str(edit.value.get("id", old.id)),
            start=Fraction(str(edit.value.get("start", old.start))),
            end=Fraction(str(edit.value.get("end", old.end))),
            value=Fraction(str(edit.value.get("value", old.value))),
        )
    else:
        pedals[edit.index] = replace(pedals[edit.index], value=Fraction(str(edit.value)))
    return replace_score_part(score, replace(part, pedals=tuple(pedals)))


def apply_edits(score: Score, edits: list[NoteEdit]) -> Score:
    updated = score
    for edit in edits:
        if edit.kind == "note_velocity":
            updated = _replace_note_velocity(updated, edit)
        elif edit.kind == "expression_anchor":
            updated = _replace_expression(updated, edit)
        elif edit.kind == "tempo_segment":
            updated = _replace_tempo(updated, edit)
        elif edit.kind == "pedal_span":
            updated = _replace_pedal(updated, edit)
        elif edit.kind == "unknown":
            continue
        else:
            raise score_invalid(f"未知编辑类型: {edit.kind}", note_id=edit.note_id, field="edit.kind")
    validate_score(updated)
    return updated


def _actual_end_beat(part: Part, note_index: int, parameters: SolverParameters) -> Fraction:
    note = part.notes[note_index]
    if note.articulation == "staccato":
        return note.start + note.duration * parameters.staccato_ratio
    if note.articulation == "legato":
        for candidate in part.notes[note_index + 1 :]:
            if candidate.start > note.start:
                limit = note.start + note.duration * parameters.legato_extension_limit
                if candidate.start <= limit:
                    return candidate.start
                break
    return note.end


def _pedal_ids(part: Part, start: Fraction, end: Fraction) -> tuple[str, ...]:
    return tuple(
        pedal.id
        for pedal in part.pedals
        if pedal.start < end and pedal.end > start
    )


def _old_note_map(sequence: dict[str, Any]) -> dict[str, dict[str, Any]]:
    return {
        note["note_id"]: note
        for part in sequence.get("parts", [])
        for note in part.get("notes", [])
    }


def calculate_affected(score: Score, edits: list[NoteEdit], parameters: SolverParameters) -> dict[str, str]:
    reasons: dict[str, str] = {}
    if any(edit.kind == "unknown" for edit in edits):
        for _part, note in score.all_notes():
            reasons[note.id] = "unknown_edit_conservative_full_resolve"
        return reasons
    for edit in edits:
        if edit.kind == "note_velocity":
            if edit.note_id is None:
                for _part, note in score.all_notes():
                    reasons[note.id] = "velocity_edit_without_note_scope_conservative"
            else:
                reasons[edit.note_id] = "velocity_value_changed"
        elif edit.kind == "expression_anchor":
            for part in score.parts:
                for note in part.notes:
                    midpoint = note.start + note.duration / 2
                    old_value = (
                        Fraction(str(edit.old_value))
                        if isinstance(edit.old_value, (int, float, str, Fraction))
                        else None
                    )
                    if old_value is None:
                        reasons[note.id] = "expression_dependency_cannot_be_proven_conservative"
                    else:
                        anchor_index = edit.index
                        if anchor_index is None or anchor_index >= len(score.expression):
                            reasons[note.id] = "expression_dependency_invalid_conservative"
                            continue
                        anchor = score.expression[anchor_index]
                        old_anchors = list(score.expression)
                        old_anchors[anchor_index] = replace(anchor, value=old_value)
                        old_score = replace(score, expression=tuple(old_anchors))
                        old_sample = ExpressionCurve(old_score).value_at(part.id, midpoint)
                        new_sample = ExpressionCurve(score).value_at(part.id, midpoint)
                        if old_sample != new_sample:
                            reasons[note.id] = "expression_interpolation_changed"
        elif edit.kind == "tempo_segment":
            if edit.index is None or not 0 <= edit.index < len(score.tempo_segments):
                for _part, note in score.all_notes():
                    reasons[note.id] = "tempo_edit_unscoped_conservative"
                continue
            if isinstance(edit.old_value, TempoSegment):
                old_segment = edit.old_value
            elif isinstance(edit.old_value, dict) and "start" in edit.old_value:
                old_segment = TempoSegment(
                    id=str(edit.old_value.get("id", score.tempo_segments[edit.index].id)),
                    start=Fraction(str(edit.old_value["start"])),
                    end=(
                        None
                        if edit.old_value.get("end") is None
                        else Fraction(str(edit.old_value["end"]))
                    ),
                    bpm=Fraction(str(edit.old_value.get("bpm", score.tempo_segments[edit.index].bpm))),
                )
            else:
                old_segment = None
            if old_segment is None:
                boundary = score.tempo_segments[edit.index].start
                for part in score.parts:
                    for note in part.notes:
                        if note.end > boundary or note.start >= boundary:
                            reasons[note.id] = "tempo_old_value_unknown_conservative"
                continue
            old_segments = [
                old_segment if index == edit.index else segment
                for index, segment in enumerate(score.tempo_segments)
            ]
            old_segments.sort(key=lambda segment: (segment.start, segment.id))
            normalised_old: list[TempoSegment] = []
            for index, segment in enumerate(old_segments):
                next_start = old_segments[index + 1].start if index + 1 < len(old_segments) else None
                normalised_old.append(replace(segment, end=next_start))
            old_score = replace(score, tempo_segments=tuple(normalised_old))
            old_tempo = TempoMap(old_score)
            new_tempo = TempoMap(score)
            for part in score.parts:
                for index, note in enumerate(part.notes):
                    actual_end = _actual_end_beat(part, index, parameters)
                    if (
                        old_tempo.seconds_at(note.start) != new_tempo.seconds_at(note.start)
                        or old_tempo.seconds_at(actual_end) != new_tempo.seconds_at(actual_end)
                    ):
                        reasons[note.id] = "tempo_actual_time_changed"
        elif edit.kind == "pedal_span":
            part = _find_part(score, edit.part_id or "")
            old_span: PedalSpan | None = None
            if isinstance(edit.old_value, PedalSpan):
                old_span = edit.old_value
            elif isinstance(edit.old_value, dict):
                try:
                    old_span = PedalSpan(
                        id=str(edit.old_value.get("id", "old")),
                        start=Fraction(str(edit.old_value["start"])),
                        end=Fraction(str(edit.old_value["end"])),
                        value=Fraction(str(edit.old_value.get("value", Fraction(4, 5)))),
                    )
                except (KeyError, ValueError):
                    old_span = None
            new_span = (
                part.pedals[edit.index]
                if edit.index is not None and 0 <= edit.index < len(part.pedals)
                else None
            )
            if old_span is None and not isinstance(edit.old_value, dict):
                for note in part.notes:
                    reasons[note.id] = "pedal_old_interval_unknown_conservative"
                continue
            intervals = [span for span in (old_span, new_span) if span is not None]
            for note in part.notes:
                if any(span.start < note.end and span.end > note.start for span in intervals):
                    reasons[note.id] = "pedal_interval_overlap_changed"
        else:
            for _part, note in score.all_notes():
                reasons[note.id] = "unrecognised_edit_conservative_full_resolve"
    return reasons


class IncrementalPerformanceEngine:
    def __init__(
        self,
        solver: PerformanceSolver | None = None,
        publisher: VersionedPublisher | None = None,
    ) -> None:
        self.solver = solver or PerformanceSolver()
        self.publisher = publisher or VersionedPublisher()
        self.score: Score | None = None
        self.parameters: SolverParameters | None = None
        self.sequence: dict[str, Any] | None = None

    def solve_initial(
        self,
        score: Score,
        *,
        parameters: SolverParameters | None = None,
        version: int = 1,
    ) -> IncrementalResult:
        parameters = parameters or self.solver.parameters
        self.solver = PerformanceSolver(parameters)
        sequence = self.solver.solve(score, parameters=parameters)
        snapshot = self.publisher.publish(sequence, version=version)
        all_ids = tuple(note.id for _part, note in score.all_notes())
        self.score = score
        self.parameters = parameters
        self.sequence = sequence
        return IncrementalResult(
            sequence=sequence,
            affected_note_ids=all_ids,
            reused_note_ids=(),
            affected_count=len(all_ids),
            reused_count=0,
            reasons={note_id: "initial_solve" for note_id in all_ids},
            edits=[],
            snapshot=snapshot,
        )

    def apply(
        self,
        edited_score: Score,
        edits: list[NoteEdit],
        *,
        parameters: SolverParameters | None = None,
    ) -> IncrementalResult:
        if self.score is None or self.sequence is None or self.parameters is None:
            raise score_invalid("必须先执行初始求解", note_id="-", field="initial_solve")
        validate_score(edited_score)
        parameters = parameters or self.parameters
        reasons = calculate_affected(edited_score, edits, parameters)
        if parameters.to_dict() != self.parameters.to_dict():
            for _part, note in edited_score.all_notes():
                reasons[note.id] = "solver_parameters_changed"
        old_notes = _old_note_map(self.sequence)
        tempo_map = TempoMap(edited_score)
        expression_curve = ExpressionCurve(edited_score)
        part_records: list[dict[str, Any]] = []
        affected_ids: list[str] = []
        reused_ids: list[str] = []
        for part in edited_score.parts:
            note_records: list[dict[str, Any]] = []
            for index, note in enumerate(part.notes):
                if note.id in reasons or note.id not in old_notes:
                    reasons.setdefault(note.id, "cache_miss_conservative")
                    affected_ids.append(note.id)
                    note_records.append(
                        self.solver.solve_note(
                            edited_score,
                            part,
                            index,
                            tempo_map=tempo_map,
                            expression_curve=expression_curve,
                        )
                    )
                else:
                    reused_ids.append(note.id)
                    note_records.append(copy.deepcopy(old_notes[note.id]))
            pedal_switches = self.solver._pedal_switches(edited_score, part, tempo_map)
            part_record: dict[str, Any] = {
                "part_id": part.id,
                "name": part.name,
                "instrument": part.instrument,
                "pitch_range": {"min": part.min_pitch, "max": part.max_pitch},
                "notes": note_records,
                "pedal_switches": pedal_switches,
            }
            part_record["part_hash"] = sha256_json(part_record)
            part_records.append(part_record)
        sequence = self.solver.assemble(edited_score, part_records, parameters=parameters)
        snapshot = self.publisher.publish(sequence)
        self.score = edited_score
        self.parameters = parameters
        self.sequence = sequence
        if not edits:
            affected_ids = []
            reused_ids = [note.id for _part, note in edited_score.all_notes()]
        return IncrementalResult(
            sequence=sequence,
            affected_note_ids=tuple(affected_ids),
            reused_note_ids=tuple(reused_ids),
            affected_count=len(affected_ids),
            reused_count=len(reused_ids),
            reasons=reasons,
            edits=[edit.to_dict() for edit in edits],
            snapshot=snapshot,
        )

