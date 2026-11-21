from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import Any, Callable

from .errors import E_CANCELLED, EngineError, ErrorLocation
from .model import (
    ExpressionAnchor,
    Part,
    PedalSpan,
    Score,
    SolverParameters,
)
from .util import (
    canonical_json_bytes,
    clamp_int,
    fraction_text,
    round_half_up,
    rounded_float,
    sha256_bytes,
    sha256_json,
)
from .validation import validate_score


class SolverCancelled(EngineError):
    def __init__(self, message: str = "求解已由用户协作式取消") -> None:
        super().__init__(
            E_CANCELLED,
            message,
            ErrorLocation(field="cancellation"),
        )


CancelCheck = Callable[[], bool]


def _check_cancel(cancel_check: CancelCheck | None) -> None:
    if cancel_check is not None and cancel_check():
        raise SolverCancelled()


def note_performance_hash(record: dict[str, Any]) -> str:
    body = dict(record)
    body.pop("performance_hash", None)
    return sha256_json(body)


def sequence_content_hash(sequence: dict[str, Any]) -> str:
    body = dict(sequence)
    body.pop("content_hash", None)
    return sha256_json(body)


def sequence_bytes(sequence: dict[str, Any], *, newline: bool = True) -> bytes:
    payload = canonical_json_bytes(sequence)
    return payload + (b"\n" if newline else b"")


@dataclass(frozen=True)
class TempoMap:
    score: Score

    def seconds_at(self, beat: Fraction) -> Fraction:
        if beat < 0:
            raise ValueError("beat must be non-negative")
        seconds = Fraction(0)
        cursor = Fraction(0)
        for segment in self.score.tempo_segments:
            segment_start = segment.start
            segment_end = segment.end
            if beat <= segment_start:
                return seconds
            audible_start = max(cursor, segment_start)
            if segment_end is None or beat <= segment_end:
                seconds += (beat - audible_start) * 60 / segment.bpm
                return seconds
            seconds += (segment_end - audible_start) * 60 / segment.bpm
            cursor = segment_end
        last = self.score.tempo_segments[-1]
        seconds += (beat - max(cursor, last.start)) * 60 / last.bpm
        return seconds

    def segments_between(self, start: Fraction, end: Fraction) -> list[dict[str, Any]]:
        records: list[dict[str, Any]] = []
        if end <= start:
            return records
        for segment in self.score.tempo_segments:
            segment_end = segment.end
            audible_end = end if segment_end is None else min(end, segment_end)
            audible_start = max(start, segment.start)
            if audible_end <= audible_start:
                continue
            start_seconds = self.seconds_at(audible_start)
            end_seconds = self.seconds_at(audible_end)
            records.append(
                {
                    "tempo_id": segment.id,
                    "bpm": fraction_text(segment.bpm),
                    "start_beat": fraction_text(audible_start),
                    "end_beat": fraction_text(audible_end),
                    "start_sec": rounded_float(start_seconds),
                    "end_sec": rounded_float(end_seconds),
                }
            )
        return records

    def summary(self) -> list[dict[str, Any]]:
        return [
            {
                "id": segment.id,
                "start_beat": fraction_text(segment.start),
                "end_beat": None if segment.end is None else fraction_text(segment.end),
                "bpm": fraction_text(segment.bpm),
                "start_sec": rounded_float(self.seconds_at(segment.start)),
            }
            for segment in self.score.tempo_segments
        ]


@dataclass(frozen=True)
class ExpressionCurve:
    score: Score

    def _anchors_for(self, part_id: str) -> list[ExpressionAnchor]:
        by_beat: dict[Fraction, ExpressionAnchor] = {}
        for anchor in self.score.expression:
            if anchor.part_id is None:
                by_beat[anchor.start] = anchor
        for anchor in self.score.expression:
            if anchor.part_id == part_id:
                by_beat[anchor.start] = anchor
        return [by_beat[beat] for beat in sorted(by_beat)]

    def value_at(self, part_id: str, beat: Fraction) -> Fraction:
        anchors = self._anchors_for(part_id)
        if not anchors:
            return Fraction(1, 2)
        if beat <= anchors[0].start:
            return anchors[0].value
        if beat >= anchors[-1].start:
            return anchors[-1].value
        for left, right in zip(anchors, anchors[1:]):
            if left.start <= beat <= right.start:
                width = right.start - left.start
                if width == 0:
                    return right.value
                ratio = (beat - left.start) / width
                return left.value + (right.value - left.value) * ratio
        return anchors[-1].value

    def anchors_for(self, part_id: str) -> list[dict[str, Any]]:
        return [
            {
                "id": anchor.id,
                "start_beat": fraction_text(anchor.start),
                "value": rounded_float(anchor.value),
            }
            for anchor in self._anchors_for(part_id)
        ]


def _measure_segments(score: Score, start: Fraction, end: Fraction) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    if end <= start:
        return records
    future_starts = [signature.start for signature in score.time_signatures]
    position = start
    while position < end:
        signature = score.signature_at(position)
        bar_index = int((position - signature.start) // signature.bar_length)
        bar_end = signature.start + (bar_index + 1) * signature.bar_length
        next_signature = min(
            (candidate for candidate in future_starts if candidate > position),
            default=None,
        )
        segment_end = bar_end
        if next_signature is not None:
            segment_end = min(segment_end, next_signature)
        segment_end = min(segment_end, end)
        if segment_end <= position:
            segment_end = min(end, position + score.grid_resolution)
        records.append(
            {
                "meter_id": signature.id,
                "bar_index": bar_index,
                "start_beat": fraction_text(position),
                "end_beat": fraction_text(segment_end),
            }
        )
        position = segment_end
    return records


class PerformanceSolver:
    def __init__(self, parameters: SolverParameters | None = None) -> None:
        self.parameters = parameters or SolverParameters()

    def _context(self, score: Score) -> tuple[TempoMap, ExpressionCurve]:
        return TempoMap(score), ExpressionCurve(score)

    def solve_note(
        self,
        score: Score,
        part: Part,
        note_index: int,
        *,
        tempo_map: TempoMap | None = None,
        expression_curve: ExpressionCurve | None = None,
    ) -> dict[str, Any]:
        tempo_map = tempo_map or TempoMap(score)
        expression_curve = expression_curve or ExpressionCurve(score)
        note = part.notes[note_index]
        next_notes = [candidate for candidate in part.notes[note_index + 1 :] if candidate.start > note.start]
        next_note = next_notes[0] if next_notes else None
        explanations: list[dict[str, Any]] = []
        actual_duration = note.duration
        if note.articulation == "staccato":
            actual_duration = note.duration * self.parameters.staccato_ratio
            explanations.append(
                {
                    "code": "staccato_shortened",
                    "detail": f"时值按 {fraction_text(self.parameters.staccato_ratio)} 缩短",
                }
            )
        elif note.articulation == "legato":
            if next_note is not None:
                extension_limit = note.start + note.duration * self.parameters.legato_extension_limit
                if next_note.start <= extension_limit:
                    actual_duration = next_note.start - note.start
                    code = "legato_extended_to_next_onset" if next_note.start > note.end else "legato_clipped_at_next_onset"
                    explanations.append(
                        {
                            "code": code,
                            "next_note_id": next_note.id,
                            "detail": "连奏实际结束点对齐下一音符起点",
                        }
                    )
                else:
                    explanations.append(
                        {
                            "code": "legato_limit_reached",
                            "next_note_id": next_note.id,
                            "detail": "下一音符超出连奏延伸上限，保留书写时值",
                        }
                    )
            else:
                explanations.append({"code": "legato_no_next_note", "detail": "末音符保留书写时值"})
        actual_end = note.start + actual_duration
        midpoint = note.start + note.duration / 2
        expression = expression_curve.value_at(part.id, midpoint)
        scale = (
            Fraction(1)
            - self.parameters.expression_strength / 2
            + self.parameters.expression_strength * expression
        )
        raw_velocity = Fraction(note.velocity) * scale * self.parameters.velocity_scale
        velocity = clamp_int(round_half_up(raw_velocity), 1, 127)
        timing_segments = tempo_map.segments_between(note.start, actual_end)
        if len(timing_segments) > 1:
            explanations.append(
                {
                    "code": "note_crosses_tempo_boundary",
                    "detail": f"音符跨越 {len(timing_segments) - 1} 个速度变化边界，逐段积分计算实际时刻",
                }
            )
        measure_segments = _measure_segments(score, note.start, actual_end)
        if len(measure_segments) > 1:
            explanations.append(
                {
                    "code": "note_crosses_measure_boundary",
                    "detail": f"音符跨越 {len(measure_segments)} 个小节片段",
                }
            )
        covering_pedals = [
            pedal.id
            for pedal in part.pedals
            if pedal.start < actual_end and pedal.end > note.start
        ]
        start_seconds = tempo_map.seconds_at(note.start)
        end_seconds = tempo_map.seconds_at(actual_end)
        record: dict[str, Any] = {
            "note_id": note.id,
            "part_id": part.id,
            "pitch": note.pitch,
            "start_beat": fraction_text(note.start),
            "written_duration": fraction_text(note.duration),
            "actual_end_beat": fraction_text(actual_end),
            "start_sec": rounded_float(start_seconds),
            "end_sec": rounded_float(end_seconds),
            "duration_sec": rounded_float(end_seconds - start_seconds),
            "start_sec_exact": fraction_text(start_seconds),
            "end_sec_exact": fraction_text(end_seconds),
            "duration_sec_exact": fraction_text(end_seconds - start_seconds),
            "base_velocity": note.velocity,
            "velocity": velocity,
            "expression_value": rounded_float(expression),
            "expression_value_exact": fraction_text(expression),
            "articulation": note.articulation,
            "pedal_ids": covering_pedals,
            "tempo_segments": timing_segments,
            "measure_segments": measure_segments,
            "explanations": explanations,
        }
        record["performance_hash"] = note_performance_hash(record)
        return record

    def solve_part(
        self,
        score: Score,
        part: Part,
        *,
        tempo_map: TempoMap | None = None,
        expression_curve: ExpressionCurve | None = None,
        cancel_check: CancelCheck | None = None,
    ) -> dict[str, Any]:
        tempo_map = tempo_map or TempoMap(score)
        expression_curve = expression_curve or ExpressionCurve(score)
        notes: list[dict[str, Any]] = []
        for index, _note in enumerate(part.notes):
            _check_cancel(cancel_check)
            notes.append(
                self.solve_note(
                    score,
                    part,
                    index,
                    tempo_map=tempo_map,
                    expression_curve=expression_curve,
                )
            )
        pedal_switches = self._pedal_switches(score, part, tempo_map)
        part_record: dict[str, Any] = {
            "part_id": part.id,
            "name": part.name,
            "instrument": part.instrument,
            "pitch_range": {"min": part.min_pitch, "max": part.max_pitch},
            "notes": notes,
            "pedal_switches": pedal_switches,
        }
        part_record["part_hash"] = sha256_json(part_record)
        return part_record

    def _pedal_switches(
        self,
        score: Score,
        part: Part,
        tempo_map: TempoMap,
    ) -> list[dict[str, Any]]:
        events: list[dict[str, Any]] = []
        for pedal in part.pedals:
            bar_segments = _measure_segments(score, pedal.start, pedal.end)
            cross_explanation = (
                "踏板区间连续保持并跨小节拆分记录" if len(bar_segments) > 1 else "单小节踏板区间"
            )
            down_sec = tempo_map.seconds_at(pedal.start)
            up_sec = tempo_map.seconds_at(pedal.end)
            events.append(
                {
                    "part_id": part.id,
                    "pedal_id": pedal.id,
                    "state": "down",
                    "beat": fraction_text(pedal.start),
                    "sec": rounded_float(down_sec),
                    "sec_exact": fraction_text(down_sec),
                    "value": rounded_float(pedal.value),
                    "bar_segments": bar_segments,
                    "explanation": cross_explanation,
                }
            )
            events.append(
                {
                    "part_id": part.id,
                    "pedal_id": pedal.id,
                    "state": "up",
                    "beat": fraction_text(pedal.end),
                    "sec": rounded_float(up_sec),
                    "sec_exact": fraction_text(up_sec),
                    "value": 0.0,
                    "bar_segments": bar_segments,
                    "explanation": cross_explanation,
                }
            )
        events.sort(key=lambda event: (Fraction(event["sec_exact"]), 0 if event["state"] == "up" else 1, event["pedal_id"]))
        return events

    def assemble(
        self,
        score: Score,
        part_records: list[dict[str, Any]],
        *,
        parameters: SolverParameters | None = None,
    ) -> dict[str, Any]:
        parameters = parameters or self.parameters
        tempo_map = TempoMap(score)
        body: dict[str, Any] = {
            "schema": "performance-sequence-v1",
            "format_version": "3.0",
            "source_version": score.source_version,
            "score_hash": score.score_hash(),
            "grid_resolution": fraction_text(score.grid_resolution),
            "start_offset": fraction_text(score.start_offset),
            "time_signatures": [signature.to_dict() for signature in score.time_signatures],
            "tempo_map": tempo_map.summary(),
            "expression_by_part": {
                part.id: ExpressionCurve(score).anchors_for(part.id)
                for part in score.parts
            },
            "parameters": parameters.to_dict(),
            "parts": part_records,
        }
        body["content_hash"] = sha256_json(body)
        return body

    def solve(
        self,
        score: Score,
        *,
        parameters: SolverParameters | None = None,
        cancel_check: CancelCheck | None = None,
        skip_parts: set[str] | None = None,
        precomputed_parts: dict[str, dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        validate_score(score)
        parameters = parameters or self.parameters
        tempo_map = TempoMap(score)
        expression_curve = ExpressionCurve(score)
        skip_parts = skip_parts or set()
        precomputed_parts = precomputed_parts or {}
        part_records: list[dict[str, Any]] = []
        for part in score.parts:
            _check_cancel(cancel_check)
            if part.id in precomputed_parts:
                part_records.append(precomputed_parts[part.id])
            elif part.id in skip_parts:
                continue
            else:
                part_records.append(
                    self.solve_part(
                        score,
                        part,
                        tempo_map=tempo_map,
                        expression_curve=expression_curve,
                        cancel_check=cancel_check,
                    )
                )
        _check_cancel(cancel_check)
        return self.assemble(score, part_records, parameters=parameters)

