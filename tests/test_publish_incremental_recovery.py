from __future__ import annotations

import json
from fractions import Fraction
from pathlib import Path

import pytest

from score_engine.conversion import parse_score
from score_engine.errors import E_RECOVERY_INVALID, E_VERSION_STALE, EngineError
from score_engine.incremental import IncrementalPerformanceEngine, apply_edits
from score_engine.model import NoteEdit, SolverParameters
from score_engine.publish import VersionedPublisher
from score_engine.recovery import RecoveryManager, SimulatedCrash
from score_engine.solver import PerformanceSolver, sequence_bytes


def test_publisher_atomic_snapshot_and_stale_errors(generated: Path, tmp_path: Path) -> None:
    score = parse_score(generated / "scores" / "virtual_v3.json")
    sequence = PerformanceSolver().solve(score)
    publisher = VersionedPublisher(tmp_path / "manifests")
    first = publisher.publish(sequence, version=1)
    assert len(first.notes()) == score.note_count()
    assert first.content_hash == sequence["content_hash"]
    assert publisher.notes_at(expected_version=1, expected_hash=first.content_hash) == first.notes()
    with pytest.raises(EngineError) as stale_hash:
        publisher.get_snapshot(expected_hash="not-the-hash")
    assert stale_hash.value.code == E_VERSION_STALE
    with pytest.raises(EngineError) as stale_version:
        publisher.get_snapshot(expected_version=9)
    assert stale_version.value.code == E_VERSION_STALE
    with pytest.raises(EngineError):
        publisher.publish(sequence, version=1)
    second = publisher.publish(sequence, version=2)
    assert second.version == 2
    assert first.notes() == second.notes()
    assert len(publisher.timeline()) == 2
    assert (tmp_path / "manifests" / "latest-manifest.json").exists()


def test_incremental_velocity_reuses_other_notes(generated: Path) -> None:
    score = parse_score(generated / "scores" / "virtual_v3.json")
    publisher = VersionedPublisher()
    engine = IncrementalPerformanceEngine(publisher=publisher)
    initial = engine.solve_initial(score, parameters=SolverParameters(), version=1)
    old_notes = {
        note["note_id"]: note
        for part in initial.sequence["parts"]
        for note in part["notes"]
    }
    target = score.parts[0].notes[0]
    edit = NoteEdit(
        "note_velocity",
        part_id=score.parts[0].id,
        note_id=target.id,
        value=110,
        old_value=target.velocity,
    )
    edited = apply_edits(score, [edit])
    result = engine.apply(edited, [edit])
    assert result.affected_note_ids == (target.id,)
    assert result.reused_count == score.note_count() - 1
    assert result.reasons[target.id] == "velocity_value_changed"
    new_notes = {
        note["note_id"]: note
        for part in result.sequence["parts"]
        for note in part["notes"]
    }
    for note_id, old_note in old_notes.items():
        if note_id != target.id:
            assert new_notes[note_id] == old_note
            assert new_notes[note_id]["performance_hash"] == old_note["performance_hash"]
    fresh = PerformanceSolver().solve(edited)
    assert sequence_bytes(fresh) == sequence_bytes(result.sequence)


def test_incremental_expression_tempo_pedal_dependencies(generated: Path) -> None:
    score = parse_score(generated / "scores" / "virtual_v3.json")
    engine = IncrementalPerformanceEngine()
    engine.solve_initial(score, parameters=SolverParameters(), version=1)

    anchor = score.expression[1]
    expression_edit = NoteEdit(
        "expression_anchor",
        index=1,
        value=Fraction(1, 10),
        old_value=anchor.value,
    )
    expression_score = apply_edits(score, [expression_edit])
    expression_result = engine.apply(expression_score, [expression_edit])
    assert 0 < expression_result.affected_count < score.note_count()
    assert expression_result.reused_count > 0

    tempo = score.tempo_segments[1]
    tempo_edit = NoteEdit("tempo_segment", index=1, value=Fraction(80), old_value=tempo)
    tempo_score = apply_edits(score, [tempo_edit])
    tempo_result = engine.apply(tempo_score, [tempo_edit])
    assert 0 < tempo_result.affected_count < score.note_count()
    assert all(
        reason == "tempo_actual_time_changed"
        for reason in tempo_result.reasons.values()
    )

    part = score.parts[0]
    pedal = part.pedals[0]
    pedal_edit = NoteEdit(
        "pedal_span",
        part_id=part.id,
        index=0,
        value=Fraction(3, 5),
        old_value=pedal,
    )
    pedal_score = apply_edits(score, [pedal_edit])
    pedal_result = engine.apply(pedal_score, [pedal_edit])
    assert 0 < pedal_result.affected_count < len(part.notes)
    assert pedal_result.reused_count > 0
    assert all(reason == "pedal_interval_overlap_changed" for reason in pedal_result.reasons.values())


def test_unknown_edit_forces_conservative_full_resolve(generated: Path) -> None:
    score = parse_score(generated / "scores" / "virtual_v3.json")
    engine = IncrementalPerformanceEngine()
    engine.solve_initial(score, parameters=SolverParameters(), version=1)
    edit = NoteEdit("unknown")
    edited = apply_edits(score, [edit])
    result = engine.apply(edited, [edit])
    assert result.affected_count == score.note_count()
    assert result.reused_count == 0
    assert set(result.reasons.values()) == {"unknown_edit_conservative_full_resolve"}


def test_recovery_resumes_after_forced_termination(generated: Path, tmp_path: Path) -> None:
    score = parse_score(generated / "scores" / "virtual_v3.json")
    manager = RecoveryManager(
        tmp_path / "recovery",
        score,
        parameter_version=7,
        sequence_version=1,
    )
    with pytest.raises(SimulatedCrash):
        manager.resume(crash_after_new_parts=2)
    assert len(list((tmp_path / "recovery").glob("part-*.json"))) == 2
    result = manager.resume()
    assert result.resumed_part_ids == ("P1", "P2")
    assert result.newly_solved_part_ids == ("P3",)
    assert len(result.recovered_note_ids) == 20
    assert len(result.newly_solved_note_ids) == 12
    assert not (set(result.recovered_note_ids) & set(result.newly_solved_note_ids))
    fresh = PerformanceSolver().solve(score)
    assert sequence_bytes(fresh) == sequence_bytes(result.sequence)


def test_recovery_detects_broken_chain_and_version(generated: Path, tmp_path: Path) -> None:
    score = parse_score(generated / "scores" / "virtual_v3.json")
    manager = RecoveryManager(
        tmp_path / "recovery",
        score,
        parameter_version=7,
        sequence_version=1,
    )
    manager.resume()
    path = tmp_path / "recovery" / "part-000001.json"
    document = json.loads(path.read_text(encoding="utf-8"))
    document["chain_hash"] = "0" * 64
    path.write_text(json.dumps(document, ensure_ascii=False, sort_keys=True), encoding="utf-8")
    with pytest.raises(EngineError) as caught:
        manager.resume()
    assert caught.value.code == E_RECOVERY_INVALID
    assert caught.value.location is not None
    assert caught.value.location.checkpoint == str(path)
    assert caught.value.location.field == "chain_hash"

    stale_manager = RecoveryManager(
        tmp_path / "other-recovery",
        score,
        parameter_version=7,
        sequence_version=2,
    )
    stale_manager.resume()
    with pytest.raises(EngineError) as version_error:
        RecoveryManager(
            tmp_path / "other-recovery",
            score,
            parameter_version=7,
            sequence_version=3,
        ).resume()
    assert version_error.value.code == E_RECOVERY_INVALID
    assert version_error.value.location is not None
    assert version_error.value.location.field == "sequence_version"


def test_recovery_detects_missing_field(generated: Path, tmp_path: Path) -> None:
    score = parse_score(generated / "scores" / "virtual_v3.json")
    manager = RecoveryManager(
        tmp_path / "recovery",
        score,
        parameter_version=7,
        sequence_version=1,
    )
    manager.resume()
    path = tmp_path / "recovery" / "part-000002.json"
    document = json.loads(path.read_text(encoding="utf-8"))
    del document["part_id"]
    path.write_text(json.dumps(document, ensure_ascii=False, sort_keys=True), encoding="utf-8")
    with pytest.raises(EngineError) as caught:
        manager.resume()
    assert caught.value.code == E_RECOVERY_INVALID
    assert caught.value.location is not None
    assert caught.value.location.field == "part_id"

