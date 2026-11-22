import copy

import pytest

from syncdemo.errors import SchemaError, UnitRefError
from syncdemo.schema import load_sample, validate_sample


def test_valid_sample_loads(sample):
    assert sample.units and sample.inputs and sample.clients
    ticks = [e["tick"] for e in sample.inputs]
    assert ticks == list(range(1, len(ticks) + 1))


def _broken(sample, mutate):
    broken = copy.deepcopy(sample)
    mutate(broken)
    return broken


def test_duplicate_player_rejected(sample):
    def mutate(s):
        s.units[1]["player"] = s.units[0]["player"]
    with pytest.raises(SchemaError) as exc:
        validate_sample(_broken(sample, mutate))
    assert exc.value.code == "E_SCHEMA"
    assert exc.value.field == "player"


def test_non_finite_attribute_rejected(sample):
    def mutate(s):
        s.units[0]["hp"] = float("nan")
    with pytest.raises(SchemaError) as exc:
        validate_sample(_broken(sample, mutate))
    assert exc.value.code == "E_SCHEMA"
    assert exc.value.field == "hp"


def test_missing_field_rejected(sample):
    def mutate(s):
        del s.units[0]["resource"]
    with pytest.raises(SchemaError) as exc:
        validate_sample(_broken(sample, mutate))
    assert exc.value.code == "E_SCHEMA"
    assert exc.value.field == "resource"


def test_tick_gap_rejected(sample):
    def mutate(s):
        s.inputs[5]["tick"] += 10
    with pytest.raises(SchemaError) as exc:
        validate_sample(_broken(sample, mutate))
    assert exc.value.code == "E_SCHEMA"
    assert exc.value.field == "tick"


def test_negative_latency_rejected(sample):
    def mutate(s):
        s.clients[0]["latency"][0]["tier"] = -1
    with pytest.raises(SchemaError) as exc:
        validate_sample(_broken(sample, mutate))
    assert exc.value.code == "E_SCHEMA"
    assert exc.value.field == "latency"


def test_unknown_unit_reference(sample):
    def mutate(s):
        s.inputs[0]["intents"].append({"unit": "ghost", "type": "gather"})
    with pytest.raises(UnitRefError) as exc:
        validate_sample(_broken(sample, mutate))
    assert exc.value.code == "E_UNIT"
    assert exc.value.unit == "ghost"


def test_unknown_skill_target(sample):
    def mutate(s):
        s.inputs[0]["intents"].append(
            {"unit": s.units[0]["id"], "type": "skill", "target": "ghost"})
    with pytest.raises(UnitRefError) as exc:
        validate_sample(_broken(sample, mutate))
    assert exc.value.code == "E_UNIT"
    assert exc.value.unit == "ghost"


def test_unknown_truth_unit(sample):
    def mutate(s):
        s.truth["key_states"][0]["units"]["ghost"] = {}
    with pytest.raises(UnitRefError) as exc:
        validate_sample(_broken(sample, mutate))
    assert exc.value.code == "E_UNIT"
    assert exc.value.unit == "ghost"


def test_missing_file_rejected(tmp_path):
    with pytest.raises(SchemaError) as exc:
        load_sample(tmp_path)
    assert exc.value.code == "E_SCHEMA"
    assert exc.value.field == "manifest.json"
