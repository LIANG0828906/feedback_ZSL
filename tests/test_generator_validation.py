from __future__ import annotations

import json
from pathlib import Path

import pytest

from balance_lab.errors import BalanceError, ErrorCode
from balance_lab.generator import apply_project_injection, generate_all
from balance_lab.validation import parse_project

DATA = Path(__file__).resolve().parents[1] / "data" / "samples"


def test_generator_is_byte_deterministic(tmp_path: Path) -> None:
    first = tmp_path / "first"
    second = tmp_path / "second"
    manifest_one = generate_all(first)
    manifest_two = generate_all(second)
    assert manifest_one["manifest_hash"] == manifest_two["manifest_hash"]
    for relative in [
        "alpha.json",
        "alpha.candidates.json",
        "beta.json",
        "beta.candidates.json",
        "gamma.json",
        "gamma.candidates.json",
        "injection_cases.json",
    ]:
        assert (first / relative).read_bytes() == (second / relative).read_bytes()


def test_sample_project_structure(alpha) -> None:
    project, candidate_set = alpha
    assert project.project_id == "project_alpha"
    assert len(project.classes) == 3
    assert len(project.skills) == 9
    assert len(project.formulas) == 30
    assert len(project.loadouts) == 6
    assert len(project.scenarios) == 6
    assert len(candidate_set.candidates) == 7
    assert project.content_hash()
    assert all(skill.formula_id in project.formulas for skill in project.skills.values())
    assert any(scenario.mode == "duel" for scenario in project.scenarios)
    assert any(scenario.mode == "squad" for scenario in project.scenarios)
    primary_ids = [scenario.primary_loadout_id for scenario in project.scenarios]
    assert len(primary_ids) == len(set(primary_ids)) == len(project.loadouts)


@pytest.mark.parametrize("index", range(7))
def test_project_error_code_injections(index: int, injection_specs: dict) -> None:
    case = injection_specs["project_cases"][index]
    raw = json.loads((DATA / case["project"]).read_text(encoding="utf-8"))
    mutated = apply_project_injection(raw, case["operation"])
    with pytest.raises(BalanceError) as caught:
        parse_project(mutated)
    error = caught.value
    assert error.code.value == case["expected_code"]
    assert error.object_id == case["expected_object_id"]
    assert error.field == case["expected_field"]


def test_candidate_reference_error(alpha) -> None:
    project, _ = alpha
    raw = json.loads((DATA / "alpha.candidates.json").read_text(encoding="utf-8"))
    raw["candidates"][1]["modifications"][0]["target_id"] = "skill.absent"
    from balance_lab.validation import parse_candidate_set

    with pytest.raises(BalanceError) as caught:
        parse_candidate_set(raw, project)
    assert caught.value.code == ErrorCode.REF
    assert caught.value.object_id == "candidate.burst_plus"


def test_invalid_budget_is_e_para(alpha) -> None:
    from balance_lab.budget import SimulationRequest

    project, candidates = alpha
    with pytest.raises(BalanceError) as caught:
        SimulationRequest(target_samples=0, min_samples=1).validate(
            len(candidates.candidates) * len(project.scenarios)
        )
    assert caught.value.code == ErrorCode.PARA
    assert caught.value.field == "target_samples"


def test_load_rejects_wrong_candidate_project(alpha) -> None:
    project, _ = alpha
    raw = json.loads((DATA / "beta.candidates.json").read_text(encoding="utf-8"))
    from balance_lab.validation import parse_candidate_set

    with pytest.raises(BalanceError) as caught:
        parse_candidate_set(raw, project)
    assert caught.value.code == ErrorCode.REF
    assert caught.value.field == "project_id"

