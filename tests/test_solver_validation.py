from __future__ import annotations

import copy
from fractions import Fraction
from pathlib import Path

import pytest

from score_engine.conversion import parse_score
from score_engine.errors import E_CANCELLED, EngineError
from score_engine.model import (
    Note,
    Part,
    PedalSpan,
    Score,
    TempoSegment,
    TimeSignature,
)
from score_engine.performance_validation import ValidationTolerance, validate_performance
from score_engine.solver import PerformanceSolver
from score_engine.util import canonical_json_bytes


def _tempo_score() -> Score:
    notes = (
        Note("X", 60, Fraction(2), Fraction(3), 80, "normal"),
        Note("L", 64, Fraction(5), Fraction(1), 80, "legato"),
        Note("N", 67, Fraction(7), Fraction(1), 80, "staccato"),
    )
    part = Part(
        "P1",
        "Piano",
        "piano",
        36,
        84,
        notes,
        (PedalSpan("PED", Fraction(1), Fraction(6), Fraction(4, 5)),),
    )
    return Score(
        title="tempo",
        start_offset=Fraction(0),
        grid_resolution=Fraction(1, 8),
        time_signatures=(
            TimeSignature("m1", Fraction(0), 4, 4),
            TimeSignature("m2", Fraction(4), 3, 4),
        ),
        tempo_segments=(
            TempoSegment("t1", Fraction(0), Fraction(120), Fraction(4)),
            TempoSegment("t2", Fraction(4), Fraction(60), None),
        ),
        parts=(part,),
        expression=(),
        source_version=3,
    )


def test_tempo_boundaries_legato_staccato_and_cross_bar_pedal() -> None:
    score = _tempo_score()
    sequence = PerformanceSolver().solve(score)
    part = sequence["parts"][0]
    records = {note["note_id"]: note for note in part["notes"]}
    assert records["X"]["start_sec"] == 1.0
    assert records["X"]["end_sec"] == 3.0
    assert len(records["X"]["tempo_segments"]) == 2
    assert any(item["code"] == "note_crosses_tempo_boundary" for item in records["X"]["explanations"])
    assert records["L"]["actual_end_beat"] == "7/1"
    assert records["L"]["start_sec"] == 3.0
    assert records["L"]["end_sec"] == 5.0
    assert any(item["code"] == "legato_extended_to_next_onset" for item in records["L"]["explanations"])
    assert records["N"]["end_sec"] == 5.55
    assert "staccato_shortened" in {item["code"] for item in records["N"]["explanations"]}
    down = next(event for event in part["pedal_switches"] if event["state"] == "down")
    assert len(down["bar_segments"]) == 2
    assert "跨小节" in down["explanation"]
    validation = validate_performance(score, sequence, tolerance=ValidationTolerance())
    assert validation["summary"]["within_tolerance"] is True


def test_validation_locates_perturbed_note(generated: Path) -> None:
    score = parse_score(generated / "scores" / "virtual_v3.json")
    sequence = PerformanceSolver().solve(score)
    perturbed = copy.deepcopy(sequence)
    record = perturbed["parts"][0]["notes"][0]
    record["start_sec"] = round(record["start_sec"] + 0.01, 9)
    report = validate_performance(score, perturbed, tolerance=ValidationTolerance(timing_ms=2.0))
    assert report["summary"]["within_tolerance"] is False
    assert report["sequence_hash_valid"] is False
    violation = next(item for item in report["violations"] if item["note_id"] == record["note_id"])
    assert "start_time_out_of_tolerance" in violation["fields"]
    note_report = next(item for item in report["notes"] if item["note_id"] == record["note_id"])
    assert note_report["deviations"]["start_time_ms"] >= 10.0
    assert report["maximum_deviation_note"]["note_id"] == record["note_id"]


def test_validation_locates_pitch_range_violation(generated: Path) -> None:
    score = parse_score(generated / "scores" / "range_violation_v3.json")
    sequence = PerformanceSolver().solve(score)
    report = validate_performance(score, sequence)
    assert report["summary"]["within_tolerance"] is False
    violation = next(item for item in report["violations"] if item["note_id"] == "P2-N003")
    assert "pitch_out_of_range" in violation["fields"]
    note_report = next(item for item in report["notes"] if item["note_id"] == "P2-N003")
    assert note_report["deviations"]["pitch_range"] == 8
    assert report["maximum_deviation_note"]["note_id"] == "P2-N003"


def test_solver_output_is_byte_deterministic(generated: Path) -> None:
    score = parse_score(generated / "scores" / "virtual_v3.json")
    first = PerformanceSolver().solve(score)
    second = PerformanceSolver().solve(score)
    assert canonical_json_bytes(first) == canonical_json_bytes(second)
    assert first["content_hash"] == second["content_hash"]


def test_solver_cooperative_cancellation(generated: Path) -> None:
    score = parse_score(generated / "scores" / "virtual_v3.json")
    checks = 0

    def cancel_check() -> bool:
        nonlocal checks
        checks += 1
        return checks > 3

    with pytest.raises(EngineError) as caught:
        PerformanceSolver().solve(score, cancel_check=cancel_check)
    assert caught.value.code == E_CANCELLED

