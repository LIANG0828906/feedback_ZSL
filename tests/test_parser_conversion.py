from __future__ import annotations

import copy
from fractions import Fraction
from pathlib import Path

import pytest

from score_engine.conversion import export_score, parse_score
from score_engine.errors import E_SCORE_INVALID, E_VERSION_LOSS, EngineError

from conftest import load_json


def test_parse_three_versions_and_derived_provenance(generated: Path) -> None:
    v1 = parse_score(generated / "scores" / "classic_v1.json")
    assert v1.source_version == 1
    assert all(note.velocity == 80 for note in v1.parts[0].notes)
    assert any("default_v1_velocity_80" == value for value in v1.provenance.values())
    assert any(pedal.id.startswith("P1-PEDAL") for pedal in v1.parts[0].pedals)
    assert v1.start_offset == Fraction(3, 4)

    v2 = parse_score(generated / "scores" / "expressive_v2.json")
    assert v2.source_version == 2
    assert len(v2.parts) == 2
    assert len(v2.time_signatures) == 3
    assert any(note.articulation == "staccato" for note in v2.parts[0].notes)
    assert v2.parts[0].pedals[0].end - v2.parts[0].pedals[0].start == 8

    v3 = parse_score(generated / "scores" / "virtual_v3.json")
    assert v3.source_version == 3
    assert len(v3.parts) == 3
    assert len(v3.tempo_segments) == 3
    assert v3.expression
    assert v3.parts[2].max_pitch == 60


def test_invalid_version_is_located(generated: Path) -> None:
    payload = load_json(generated / "scores" / "virtual_v3.json")
    payload["format_version"] = "9.0"
    with pytest.raises(EngineError) as caught:
        parse_score(payload)
    assert caught.value.code == E_SCORE_INVALID
    assert caught.value.location is not None
    assert caught.value.location.note_id == "-"
    assert caught.value.location.field == "format_version"


def test_missing_note_field_is_located(generated: Path) -> None:
    payload = load_json(generated / "scores" / "virtual_v3.json")
    del payload["parts"][0]["notes"][0]["pitch"]
    with pytest.raises(EngineError) as caught:
        parse_score(payload)
    assert caught.value.code == E_SCORE_INVALID
    assert caught.value.location is not None
    assert caught.value.location.note_id == "P1-N001"
    assert caught.value.location.field == "pitch"


def test_duration_meter_contradiction_is_located(generated: Path) -> None:
    payload = load_json(generated / "scores" / "virtual_v3.json")
    payload["parts"][1]["notes"][2]["duration"] = "1/7"
    with pytest.raises(EngineError) as caught:
        parse_score(payload)
    assert caught.value.code == E_SCORE_INVALID
    assert caught.value.location is not None
    assert caught.value.location.note_id == "P2-N003"
    assert caught.value.location.field == "duration"


def test_missing_v3_expression_is_located(generated: Path) -> None:
    payload = load_json(generated / "scores" / "virtual_v3.json")
    del payload["expression"]
    with pytest.raises(EngineError) as caught:
        parse_score(payload)
    assert caught.value.code == E_SCORE_INVALID
    assert caught.value.location is not None
    assert caught.value.location.field == "expression"


def test_old_version_missing_velocity_and_pedals_are_derived(generated: Path) -> None:
    payload = load_json(generated / "scores" / "expressive_v2.json")
    del payload["tracks"][0]["notes"][0]["velocity"]
    del payload["tracks"][1]["pedal_spans"]
    score = parse_score(payload)
    assert score.parts[0].notes[0].velocity == 80
    assert "default_v2_velocity_80" in score.provenance.values()
    assert score.parts[1].pedals == ()
    assert any("v2_absent_pedal_spans_none" == value for value in score.provenance.values())


def test_downgrade_loss_is_explicit_and_can_be_allowed(generated: Path) -> None:
    score = parse_score(generated / "scores" / "virtual_v3.json")
    with pytest.raises(EngineError) as caught:
        export_score(score, 2)
    assert caught.value.code == E_VERSION_LOSS
    assert caught.value.details
    fields = {detail["field"] for detail in caught.value.details}
    assert {"expression", "part.grouping"} <= fields
    assert all("note_id" in detail for detail in caught.value.details)

    result = export_score(score, 2, allow_loss=True)
    assert result.loss_count > 0
    assert result.payload["format_version"] == "2.0"
    assert "expression" not in result.payload
    v1_losses = export_score(score, 1, allow_loss=True).losses
    assert "time_signatures" in {loss["field"] for loss in v1_losses}


def test_v3_v2_v3_preserves_representable_note_values(generated: Path) -> None:
    original = parse_score(generated / "scores" / "virtual_v3.json")
    v2_result = export_score(original, 2, allow_loss=True)
    reparsed = parse_score(v2_result.payload)
    assert reparsed.start_offset == original.start_offset
    assert len(reparsed.parts) == len(original.parts)
    for original_part in original.parts:
        part = next(candidate for candidate in reparsed.parts if candidate.id == original_part.id)
        for original_note in original_part.notes:
            note = next(candidate for candidate in part.notes if candidate.id == original_note.id)
            assert note.pitch == original_note.pitch
            assert note.start == original_note.start
            assert note.duration == original_note.duration
            assert note.velocity == original_note.velocity
            assert note.articulation == original_note.articulation
        for original_pedal, pedal in zip(original_part.pedals, part.pedals):
            assert pedal.start == original_pedal.start
            assert pedal.end == original_pedal.end
            assert pedal.value == original_pedal.value


def test_v3_v1_roundtrip_preserves_timing(generated: Path) -> None:
    original = parse_score(generated / "scores" / "virtual_v3.json")
    v1_result = export_score(original, 1, allow_loss=True)
    reparsed = parse_score(v1_result.payload)
    assert reparsed.start_offset == original.start_offset
    assert len(reparsed.parts) == 1
    for original_note in original.parts[0].notes:
        note = next(candidate for candidate in reparsed.parts[0].notes if candidate.id == original_note.id)
        assert note.pitch == original_note.pitch
        assert note.start == original_note.start
        assert note.duration == original_note.duration


def test_v1_upgrade_is_lossless() -> None:
    payload = {
        "format_version": 1,
        "title": "v1",
        "time_signature": {"numerator": 4, "denominator": 4},
        "tempo_bpm": "120/1",
        "start_offset_seconds": "1/2",
        "duration_unit": "quarter",
        "notes": [
            {"id": "N1", "pitch": 60, "start": "0", "duration": "1"},
            {"id": "N2", "pitch": 64, "start": "1", "duration": "3/2"},
        ],
    }
    score = parse_score(payload)
    assert score.start_offset == 1
    upgraded = export_score(score, 3)
    assert upgraded.losses == []
    reparsed = parse_score(upgraded.payload)
    assert [note.start for note in reparsed.parts[0].notes] == [Fraction(1), Fraction(2)]
    assert [note.duration for note in reparsed.parts[0].notes] == [Fraction(1), Fraction(3, 2)]

