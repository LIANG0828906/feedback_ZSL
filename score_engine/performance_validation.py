from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import Any

import numpy as np

from .model import Part, Score, SolverParameters
from .solver import ExpressionCurve, TempoMap, sequence_content_hash
from .util import clamp_int, fraction_text, round_half_up, rounded_float


@dataclass(frozen=True)
class ValidationTolerance:
    timing_ms: float = 2.0
    velocity: float = 1.0
    pitch_range: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "timing_ms": self.timing_ms,
            "velocity": self.velocity,
            "pitch_range": self.pitch_range,
        }


def _actual_duration(part: Part, note_index: int, parameters: SolverParameters) -> Fraction:
    note = part.notes[note_index]
    if note.articulation == "staccato":
        return note.duration * parameters.staccato_ratio
    if note.articulation == "legato":
        for candidate in part.notes[note_index + 1 :]:
            if candidate.start > note.start:
                limit = note.start + note.duration * parameters.legato_extension_limit
                if candidate.start <= limit:
                    return candidate.start - note.start
                break
    return note.duration


def _find_note_record(sequence: dict[str, Any], part_id: str, note_id: str) -> dict[str, Any] | None:
    for part in sequence.get("parts", []):
        if part.get("part_id") != part_id:
            continue
        for note in part.get("notes", []):
            if note.get("note_id") == note_id:
                return note
    return None


def validate_performance(
    score: Score,
    sequence: dict[str, Any],
    *,
    tolerance: ValidationTolerance | None = None,
    parameters: SolverParameters | None = None,
) -> dict[str, Any]:
    tolerance = tolerance or ValidationTolerance()
    parameters = parameters or SolverParameters()
    tempo_map = TempoMap(score)
    expression_curve = ExpressionCurve(score)
    reports: list[dict[str, Any]] = []
    violations: list[dict[str, Any]] = []
    missing_note_ids: list[str] = []
    sequence_hash = sequence.get("content_hash")
    computed_hash = sequence_content_hash(sequence)
    sequence_hash_valid = sequence_hash == computed_hash
    score_hash_valid = sequence.get("score_hash") == score.score_hash()
    for part in score.parts:
        for note_index, note in enumerate(part.notes):
            record = _find_note_record(sequence, part.id, note.id)
            if record is None:
                missing_note_ids.append(note.id)
                reports.append(
                    {
                        "note_id": note.id,
                        "part_id": part.id,
                        "pitch": note.pitch,
                        "flags": ["missing_sequence_record"],
                        "within_tolerance": False,
                    }
                )
                violations.append({"note_id": note.id, "part_id": part.id, "fields": ["record"]})
                continue
            expected_duration = _actual_duration(part, note_index, parameters)
            expected_end_beat = note.start + expected_duration
            expected_start_sec = tempo_map.seconds_at(note.start)
            expected_end_sec = tempo_map.seconds_at(expected_end_beat)
            midpoint = note.start + note.duration / 2
            expression = expression_curve.value_at(part.id, midpoint)
            scale = Fraction(1) - parameters.expression_strength / 2 + parameters.expression_strength * expression
            expected_velocity = clamp_int(
                round_half_up(Fraction(note.velocity) * scale * parameters.velocity_scale),
                1,
                127,
            )
            actual_start_beat = Fraction(str(record.get("start_beat", "0")))
            actual_end_beat = Fraction(str(record.get("actual_end_beat", "0")))
            start_beat_error = abs(actual_start_beat - note.start)
            end_beat_error = abs(actual_end_beat - expected_end_beat)
            start_error_ms = abs(record.get("start_sec", 0.0) - rounded_float(expected_start_sec)) * 1000
            end_error_ms = abs(record.get("end_sec", 0.0) - rounded_float(expected_end_sec)) * 1000
            velocity_error = abs(int(record.get("velocity", 0)) - expected_velocity)
            pitch_error = abs(int(record.get("pitch", -1)) - note.pitch)
            if part.min_pitch <= note.pitch <= part.max_pitch:
                range_excess = 0
            else:
                range_excess = min(abs(note.pitch - part.min_pitch), abs(note.pitch - part.max_pitch))
            flags: list[str] = []
            if start_error_ms > tolerance.timing_ms:
                flags.append("start_time_out_of_tolerance")
            if end_error_ms > tolerance.timing_ms:
                flags.append("end_time_out_of_tolerance")
            if velocity_error > tolerance.velocity:
                flags.append("velocity_out_of_tolerance")
            if start_beat_error != 0:
                flags.append("start_beat_mismatch")
            if end_beat_error != 0:
                flags.append("end_beat_mismatch")
            if pitch_error != 0:
                flags.append("pitch_mismatch")
            if range_excess > tolerance.pitch_range:
                flags.append("pitch_out_of_range")
            if record.get("performance_hash") not in {None, _record_hash(record)}:
                flags.append("performance_hash_mismatch")
            if flags:
                violations.append(
                    {
                        "note_id": note.id,
                        "part_id": part.id,
                        "fields": flags,
                        "pitch": note.pitch,
                    }
                )
            reports.append(
                {
                    "note_id": note.id,
                    "part_id": part.id,
                    "pitch": note.pitch,
                    "declared_pitch_range": {"min": part.min_pitch, "max": part.max_pitch},
                    "expected": {
                        "start_beat": fraction_text(note.start),
                        "end_beat": fraction_text(expected_end_beat),
                        "start_sec": rounded_float(expected_start_sec),
                        "end_sec": rounded_float(expected_end_sec),
                        "velocity": expected_velocity,
                    },
                    "actual": {
                        "start_beat": record.get("start_beat"),
                        "end_beat": record.get("actual_end_beat"),
                        "start_sec": record.get("start_sec"),
                        "end_sec": record.get("end_sec"),
                        "velocity": record.get("velocity"),
                    },
                    "deviations": {
                        "start_beat": rounded_float(start_beat_error),
                        "end_beat": rounded_float(end_beat_error),
                        "start_time_ms": round(start_error_ms, 6),
                        "end_time_ms": round(end_error_ms, 6),
                        "velocity": velocity_error,
                        "pitch": pitch_error,
                        "pitch_range": range_excess,
                    },
                    "flags": flags,
                    "within_tolerance": not flags,
                }
            )
    complete_reports = [report for report in reports if "deviations" in report]
    start_errors = np.asarray(
        [report["deviations"]["start_time_ms"] for report in complete_reports],
        dtype=np.float64,
    )
    end_errors = np.asarray(
        [report["deviations"]["end_time_ms"] for report in complete_reports],
        dtype=np.float64,
    )
    velocity_errors = np.asarray(
        [report["deviations"]["velocity"] for report in complete_reports],
        dtype=np.float64,
    )
    start_beat_errors = np.asarray(
        [report["deviations"]["start_beat"] for report in complete_reports],
        dtype=np.float64,
    )
    end_beat_errors = np.asarray(
        [report["deviations"]["end_beat"] for report in complete_reports],
        dtype=np.float64,
    )
    pitch_errors = np.asarray(
        [report["deviations"]["pitch"] for report in complete_reports],
        dtype=np.float64,
    )
    range_errors = np.asarray(
        [report["deviations"]["pitch_range"] for report in complete_reports],
        dtype=np.float64,
    )

    def metric_stats(values: np.ndarray) -> dict[str, float]:
        if values.size == 0:
            return {"mean": 0.0, "max": 0.0, "median": 0.0, "sum": 0.0}
        return {
            "mean": rounded_float(float(np.mean(values))),
            "max": rounded_float(float(np.max(values))),
            "median": rounded_float(float(np.median(values))),
            "sum": rounded_float(float(np.sum(values))),
        }

    metrics = {
        "start_time_ms": start_errors,
        "end_time_ms": end_errors,
        "velocity": velocity_errors,
        "start_beat": start_beat_errors,
        "end_beat": end_beat_errors,
        "pitch": pitch_errors,
        "pitch_range": range_errors,
    }
    max_per_metric: dict[str, dict[str, Any] | None] = {}
    for metric_name, values in metrics.items():
        if values.size == 0:
            max_per_metric[metric_name] = None
            continue
        maximum_index = int(np.argmax(values))
        report = complete_reports[maximum_index]
        max_per_metric[metric_name] = {
            "note_id": report["note_id"],
            "part_id": report["part_id"],
            "value": rounded_float(float(values[maximum_index])),
        }
    normalized_scores: list[float] = []
    tolerance_values = {
        "start_time_ms": tolerance.timing_ms,
        "end_time_ms": tolerance.timing_ms,
        "velocity": tolerance.velocity,
        "start_beat": 1e-9,
        "end_beat": 1e-9,
        "pitch": 1.0,
        "pitch_range": max(tolerance.pitch_range, 1),
    }
    for report in complete_reports:
        normalized = max(
            report["deviations"][metric] / tolerance_values[metric]
            for metric in tolerance_values
        )
        normalized_scores.append(normalized)
    maximum_note = None
    if normalized_scores:
        maximum_index = max(range(len(normalized_scores)), key=lambda index: (normalized_scores[index], -index))
        maximum_report = complete_reports[maximum_index]
        maximum_note = {
            "note_id": maximum_report["note_id"],
            "part_id": maximum_report["part_id"],
            "normalized_deviation": rounded_float(normalized_scores[maximum_index]),
            "deviations": maximum_report["deviations"],
        }
    return {
        "schema": "performance-validation-v1",
        "score_hash_valid": score_hash_valid,
        "sequence_hash_valid": sequence_hash_valid,
        "sequence_hash": sequence_hash,
        "computed_sequence_hash": computed_hash,
        "tolerance": tolerance.to_dict(),
        "summary": {
            "note_count": score.note_count(),
            "validated_note_count": len(complete_reports),
            "missing_note_count": len(missing_note_ids),
            "violation_count": len(violations),
            "within_tolerance": not violations and not missing_note_ids and sequence_hash_valid and score_hash_valid,
            "statistics": {
                "start_time_ms": metric_stats(start_errors),
                "end_time_ms": metric_stats(end_errors),
                "velocity": metric_stats(velocity_errors),
                "start_beat": metric_stats(start_beat_errors),
                "end_beat": metric_stats(end_beat_errors),
                "pitch": metric_stats(pitch_errors),
                "pitch_range": metric_stats(range_errors),
            },
        },
        "max_per_metric": max_per_metric,
        "maximum_deviation_note": maximum_note,
        "violations": violations,
        "missing_note_ids": missing_note_ids,
        "notes": reports,
    }


def _record_hash(record: dict[str, Any]) -> str:
    from .solver import note_performance_hash

    return note_performance_hash(record)

