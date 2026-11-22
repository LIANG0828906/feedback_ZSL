import copy

import pytest

from scoreforge.errors import DurationError, RefError, SchemaError
from scoreforge.model import load_project
from scoreforge.validation import validate_project


def test_valid_project_passes(project_data):
    validate_project(project_data)  # 不抛异常


def test_missing_field_returns_e_schema(project_data):
    data = copy.deepcopy(project_data)
    del data["parts"][0]["measures"][0]["notes"][0]["pitch"]
    with pytest.raises(SchemaError) as exc:
        validate_project(data)
    assert exc.value.code == "E_SCHEMA"
    assert exc.value.path == "$.parts[0].measures[0].notes[0]"


def test_missing_top_level_field(project_data):
    data = copy.deepcopy(project_data)
    del data["tempo_map"]
    with pytest.raises(SchemaError):
        validate_project(data)


def test_dangling_anchor_returns_e_ref(project_data):
    data = copy.deepcopy(project_data)
    data["expression_events"][0]["anchor_note_id"] = "NOPE"
    with pytest.raises(RefError) as exc:
        validate_project(data)
    assert exc.value.code == "E_REF"
    assert "anchor_note_id" in exc.value.path


def test_unknown_part_returns_e_ref(project_data):
    data = copy.deepcopy(project_data)
    data["expression_events"][0]["part_id"] = "PX"
    with pytest.raises(RefError):
        validate_project(data)


def test_beat_overflow_returns_e_ref(project_data):
    data = copy.deepcopy(project_data)
    data["expression_events"][0]["beat_offset_ticks"] = 10 ** 9
    with pytest.raises(RefError) as exc:
        validate_project(data)
    assert "beat_offset_ticks" in exc.value.path


@pytest.mark.parametrize("bad", [0, -120, 12.5, True, "480"])
def test_illegal_duration_returns_e_dur(project_data, bad):
    data = copy.deepcopy(project_data)
    data["parts"][0]["measures"][0]["notes"][0]["duration_ticks"] = bad
    with pytest.raises(DurationError) as exc:
        validate_project(data)
    assert exc.value.code == "E_DUR"
    assert "duration_ticks" in exc.value.path


def test_notes_overflow_measure_returns_e_dur(project_data):
    data = copy.deepcopy(project_data)
    note = data["parts"][0]["measures"][0]["notes"][0]
    note["duration_ticks"] += 10 ** 6
    with pytest.raises(DurationError):
        validate_project(data)


def test_truth_dangling_reference_returns_e_ref(project_data):
    data = copy.deepcopy(project_data)
    data["truth"]["event_timestamps"][0]["event_id"] = "E9999"
    with pytest.raises(RefError):
        validate_project(data)
