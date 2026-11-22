from __future__ import annotations

import json
import random
from pathlib import Path
from typing import Any

from .combat import CandidateRuntime, FormulaEvaluator, simulate_match
from .models import Candidate, CandidateModification, CandidateSet, Project
from .util import content_hash, rate_bp, wilson_interval
from .validation import load_candidate_set, parse_project


GENERATOR_VERSION = "sample-generator-v1"
SCALES = {
    "damage": "milli-HP integer",
    "coefficient": "basis points (10000 = 1.0)",
    "hit_roll": "basis points (10000 = 1.0)",
    "resource": "milli-resource integer",
}


PROJECT_SPECS = (
    {
        "slug": "alpha",
        "project_id": "project_alpha",
        "name": "边境裂隙 · Alpha",
        "seed": 420143,
        "budget": {
            "parallel_slots": 3,
            "max_matches_per_round": 5,
            "memory_buffer_kb": 2048,
            "max_scenario_seconds": 8,
        },
    },
    {
        "slug": "beta",
        "project_id": "project_beta",
        "name": "灰烬竞技场 · Beta",
        "seed": 730021,
        "budget": {
            "parallel_slots": 2,
            "max_matches_per_round": 3,
            "memory_buffer_kb": 1536,
            "max_scenario_seconds": 6,
        },
    },
    {
        "slug": "gamma",
        "project_id": "project_gamma",
        "name": "风暴回廊 · Gamma",
        "seed": 991337,
        "budget": {
            "parallel_slots": 4,
            "max_matches_per_round": 7,
            "memory_buffer_kb": 3072,
            "max_scenario_seconds": 10,
        },
    },
)


CLASS_SPECS = (
    {
        "id": "class.iron",
        "name": "Iron Vanguard",
        "role": "frontline",
        "skills": (
            ("strike", "Shield Strike", 130_000, 1_200, 0, 0, (9_400, 10_600), 10),
            ("bash", "Shield Bash", 205_000, 1_600, 2, 20_000, (9_000, 11_000), 30),
            ("earth", "Earthsplitter", 315_000, 1_000, 4, 55_000, (8_600, 11_400), 50),
        ),
        "attributes": {
            "hp": 1_350_000,
            "attack": 1_500,
            "defense": 850,
            "resource_max": 140_000,
            "resource_regen": 26_000,
            "speed": 90,
        },
    },
    {
        "id": "class.ranger",
        "name": "Ash Ranger",
        "role": "ranged",
        "skills": (
            ("quick", "Quick Shot", 90_000, 1_400, 0, 0, (9_200, 10_800), 10),
            ("pierce", "Piercing Bolt", 190_000, 2_100, 2, 24_000, (9_100, 10_900), 30),
            ("rain", "Arrow Rain", 260_000, 1_000, 4, 60_000, (8_500, 11_500), 50),
        ),
        "attributes": {
            "hp": 1_080_000,
            "attack": 1_750,
            "defense": 620,
            "resource_max": 125_000,
            "resource_regen": 30_000,
            "speed": 115,
        },
    },
    {
        "id": "class.mystic",
        "name": "Storm Mystic",
        "role": "caster",
        "skills": (
            ("spark", "Arc Spark", 100_000, 1_700, 0, 0, (9_300, 10_700), 10),
            ("siphon", "Void Siphon", 170_000, 2_300, 2, 22_000, (8_900, 11_100), 30),
            ("cataclysm", "Cataclysm", 300_000, 1_300, 5, 70_000, (8_200, 11_800), 50),
        ),
        "attributes": {
            "hp": 1_180_000,
            "attack": 1_650,
            "defense": 700,
            "resource_max": 150_000,
            "resource_regen": 34_000,
            "speed": 105,
        },
    },
)


def _jitter(rng: random.Random, value: int, pct_bp: int = 450) -> int:
    factor = 10_000 + rng.randint(-pct_bp, pct_bp)
    return max(1, (value * factor + 5_000) // 10_000)


def _formula_nodes(skill_id: str) -> list[dict[str, Any]]:
    return [
        {
            "id": f"formula.{skill_id}.base",
            "name": f"{skill_id} base + growth",
            "op": "ADD",
            "args": [
                {"kind": "skill", "field": "base_damage"},
                {"kind": "formula", "id": f"formula.{skill_id}.growth"},
            ],
        },
        {
            "id": f"formula.{skill_id}.growth",
            "name": f"{skill_id} attack growth",
            "op": "MUL_BP",
            "args": [
                {"kind": "attr", "side": "attacker", "field": "attack"},
                {"kind": "skill", "field": "growth_coeff"},
            ],
        },
        {
            "id": f"formula.{skill_id}.damage",
            "name": f"{skill_id} damage range",
            "op": "MUL_BP",
            "args": [
                {"kind": "formula", "id": f"formula.{skill_id}.base"},
                {"kind": "formula", "id": "formula.attack_ratio"},
            ],
        },
    ]


def _shared_formulas() -> list[dict[str, Any]]:
    return [
        {
            "id": "formula.attack_ratio",
            "name": "attack / (attack + defense), 1.5x tuned",
            "op": "DIV_BP",
            "args": [
                {"kind": "formula", "id": "formula.attack_scaled"},
                {"kind": "formula", "id": "formula.attack_plus_defense"},
            ],
        },
        {
            "id": "formula.attack_scaled",
            "name": "attack x1.5",
            "op": "MUL_BP",
            "args": [
                {"kind": "attr", "side": "attacker", "field": "attack"},
                {"kind": "const", "value": 15_000},
            ],
        },
        {
            "id": "formula.attack_plus_defense",
            "name": "attack + defense denominator",
            "op": "ADD",
            "args": [
                {"kind": "attr", "side": "attacker", "field": "attack"},
                {"kind": "attr", "side": "defender", "field": "defense"},
            ],
        },
    ]


def _build_raw_project(spec: dict[str, Any]) -> dict[str, Any]:
    rng = random.Random(spec["seed"])
    classes: list[dict[str, Any]] = []
    skills: list[dict[str, Any]] = []
    formulas: list[dict[str, Any]] = _shared_formulas()
    loadouts: list[dict[str, Any]] = []
    class_loadouts: dict[str, str] = {}

    for class_spec in CLASS_SPECS:
        class_id = class_spec["id"]
        class_skills: list[str] = []
        for short_name, display_name, base, growth, cooldown, cost, hit_range, priority in class_spec[
            "skills"
        ]:
            skill_id = f"skill.{class_id.split('.')[-1]}.{short_name}"
            class_skills.append(skill_id)
            skills.append(
                {
                    "id": skill_id,
                    "name": display_name,
                    "class_id": class_id,
                    "base_damage": _jitter(rng, base),
                    "growth_coeff": _jitter(rng, growth),
                    "cooldown": cooldown,
                    "resource_cost": _jitter(rng, cost) if cost else 0,
                    "hit_min_bp": hit_range[0],
                    "hit_max_bp": hit_range[1],
                    "formula_id": f"formula.{skill_id}.damage",
                    "priority": priority,
                }
            )
            formulas.extend(_formula_nodes(skill_id))
        classes.append(
            {
                "id": class_id,
                "name": class_spec["name"],
                "role": class_spec["role"],
                "skills": class_skills,
            }
        )

        short_class = class_id.split(".")[-1]
        for side in ("a", "b"):
            attrs = {
                key: _jitter(rng, value)
                for key, value in class_spec["attributes"].items()
            }
            if side == "b":
                attrs["attack"] = _jitter(rng, attrs["attack"], 350)
                attrs["defense"] = _jitter(rng, attrs["defense"], 350)
            loadout_id = f"loadout.{short_class}_{side}"
            loadouts.append(
                {
                    "id": loadout_id,
                    "name": f"{class_spec['name']} {'Primary' if side == 'a' else 'Rival'}",
                    "class_id": class_id,
                    "attributes": attrs,
                }
            )
            if side == "a":
                class_loadouts[class_id] = loadout_id

    scenario_specs = [
        (
            "scene.duel.iron",
            "Duel · Iron vs Ranger",
            "duel",
            "loadout.iron_a",
            ("loadout.iron_a",),
            ("loadout.ranger_b",),
            20,
        ),
        (
            "scene.duel.ranger",
            "Duel · Ranger vs Mystic",
            "duel",
            "loadout.ranger_a",
            ("loadout.ranger_a",),
            ("loadout.mystic_b",),
            20,
        ),
        (
            "scene.duel.mystic",
            "Duel · Mystic vs Iron",
            "duel",
            "loadout.mystic_a",
            ("loadout.mystic_a",),
            ("loadout.iron_b",),
            22,
        ),
        (
            "scene.duel.rival_iron",
            "Duel · Rival Iron vs Ranger",
            "duel",
            "loadout.iron_b",
            ("loadout.iron_b",),
            ("loadout.ranger_a",),
            20,
        ),
        (
            "scene.squad.ranger",
            "Squad · Ranger / Mystic",
            "squad",
            "loadout.ranger_b",
            ("loadout.ranger_b", "loadout.mystic_b"),
            ("loadout.iron_a", "loadout.ranger_a"),
            24,
        ),
        (
            "scene.squad.mystic",
            "Squad · Mystic / Iron",
            "squad",
            "loadout.mystic_b",
            ("loadout.mystic_b", "loadout.iron_b"),
            ("loadout.ranger_a", "loadout.mystic_a"),
            24,
        ),
    ]
    scenarios = []
    for index, (scene_id, name, mode, primary, party, opponents, max_rounds) in enumerate(
        scenario_specs
    ):
        scenarios.append(
            {
                "id": scene_id,
                "name": name,
                "mode": mode,
                "primary_loadout_id": primary,
                "party_loadout_ids": list(party),
                "opponent_loadout_ids": list(opponents),
                "max_rounds": max_rounds,
                "seed": spec["seed"] * 10 + 101 + index,
                "estimated_ms_per_match": 4 if mode == "duel" else 7,
            }
        )

    return {
        "schema_version": 1,
        "project_id": spec["project_id"],
        "name": spec["name"],
        "generator_seed": spec["seed"],
        "classes": classes,
        "skills": skills,
        "formulas": formulas,
        "loadouts": loadouts,
        "scenarios": scenarios,
        "validation_budget": spec["budget"],
        "truth": {"formula_cases": [], "win_rates": [], "thresholds": []},
        "provenance": {
            "generator_version": GENERATOR_VERSION,
            "generator_seed": spec["seed"],
            "scales": SCALES,
        },
    }


def _build_candidates(project: Project) -> CandidateSet:
    skill_ids = sorted(project.skills)
    burst_skill_ids = [
        skill_id
        for skill_id in skill_ids
        if project.skills[skill_id].priority >= 30
    ]
    basic_skill_ids = [
        skill_id
        for skill_id in skill_ids
        if project.skills[skill_id].priority == 10
    ]
    all_loadouts = sorted(project.loadouts)
    candidates = [
        Candidate(
            id="candidate.baseline",
            name="基准方案",
            priority=120,
            modifications=(),
            note="未调整，作为真值校准与回归基线。",
        ),
        Candidate(
            id="candidate.burst_plus",
            name="爆发增强",
            priority=115,
            modifications=tuple(
                CandidateModification("skill", skill_id, "base_damage", "scale_bp", 10_750)
                for skill_id in burst_skill_ids
            )
            + tuple(
                CandidateModification("skill", skill_id, "resource_cost", "scale_bp", 10_500)
                for skill_id in burst_skill_ids
            ),
            note="爆发技能基础伤害 +7.5%，资源消耗 +5%。",
        ),
        Candidate(
            id="candidate.sustain_trim",
            name="续航削减",
            priority=110,
            modifications=tuple(
                CandidateModification("skill", skill_id, "growth_coeff", "scale_bp", 9_200)
                for skill_id in basic_skill_ids
            ),
            note="基础技能成长系数 -8%，观察长回合伤害曲线。",
        ),
        Candidate(
            id="candidate.attack_tune",
            name="攻击上调",
            priority=105,
            modifications=tuple(
                CandidateModification("loadout", loadout_id, "attack", "scale_bp", 10_500)
                for loadout_id in all_loadouts
            ),
            note="全部主用配装攻击 +5%。",
        ),
        Candidate(
            id="candidate.defense_tune",
            name="防御上调",
            priority=100,
            modifications=tuple(
                CandidateModification("loadout", loadout_id, "defense", "scale_bp", 10_800)
                for loadout_id in all_loadouts
            )
            + tuple(
                CandidateModification("loadout", loadout_id, "hp", "scale_bp", 9_800)
                for loadout_id in all_loadouts
            ),
            note="防御 +8%、生命 -2% 的偏防守配平。",
        ),
        Candidate(
            id="candidate.glass_cannon",
            name="玻璃大炮",
            priority=95,
            modifications=tuple(
                CandidateModification("loadout", loadout_id, "attack", "scale_bp", 11_000)
                for loadout_id in all_loadouts
            )
            + tuple(
                CandidateModification("loadout", loadout_id, "defense", "scale_bp", 8_800)
                for loadout_id in all_loadouts
            )
            + tuple(
                CandidateModification("loadout", loadout_id, "hp", "scale_bp", 9_200)
                for loadout_id in all_loadouts
            ),
            note="攻击 +10%，防御 -12%，生命 -8%，用于阈值失败诊断。",
        ),
        Candidate(
            id="candidate.resource_relief",
            name="资源减压",
            priority=90,
            modifications=tuple(
                CandidateModification("skill", skill_id, "resource_cost", "scale_bp", 8_500)
                for skill_id in burst_skill_ids
            )
            + tuple(
                CandidateModification("skill", skill_id, "cooldown", "add", -1)
                for skill_id in burst_skill_ids
                if project.skills[skill_id].cooldown >= 2 and project.skills[skill_id].priority == 30
            ),
            note="高阶技能资源 -15%，2 回合技能冷却 -1。",
        ),
    ]
    return CandidateSet(project_id=project.project_id, candidates=tuple(candidates))


def _formula_truth(project: Project, candidate: Candidate) -> list[dict[str, Any]]:
    runtime = CandidateRuntime.from_candidate(project, candidate)
    primary_by_class = {
        loadout.class_id: loadout_id for loadout_id, loadout in project.loadouts.items()
    }
    defender_by_primary: dict[str, str] = {}
    for scenario in project.scenarios:
        defender_by_primary[scenario.primary_loadout_id] = scenario.opponent_loadout_ids[0]
    cases: list[dict[str, Any]] = []
    for skill_id in sorted(project.skills):
        skill = project.skills[skill_id]
        loadout_id = primary_by_class[skill.class_id]
        defender_id = defender_by_primary[loadout_id]
        evaluator = FormulaEvaluator(
            project,
            skill_id,
            runtime.skill_values,
            runtime.loadout_values[loadout_id],
            project.loadouts[defender_id].attributes,
        )
        raw_min, raw_max = evaluator.evaluate(skill.formula_id)
        expected_min = raw_min * skill.hit_min_bp // 10_000
        expected_max = raw_max * skill.hit_max_bp // 10_000
        cases.append(
            {
                "id": f"truth.formula.{skill_id}",
                "skill_id": skill_id,
                "loadout_id": loadout_id,
                "defender_loadout_id": defender_id,
                "formula_id": skill.formula_id,
                "expected_min": expected_min,
                "expected_max": expected_max,
                "expected_mid": (expected_min + expected_max) // 2,
                "source": "integer_formula_interval",
            }
        )
    return cases


def _win_truth(project: Project, candidate: Candidate) -> list[dict[str, Any]]:
    runtime = CandidateRuntime.from_candidate(project, candidate)
    reference_samples = 256
    rows: list[dict[str, Any]] = []
    for scenario in project.scenarios:
        wins = 0
        for reference_index in range(reference_samples):
            match = simulate_match(
                project,
                runtime,
                scenario,
                sample_index=100_000 + reference_index,
            )
            if match.winner == "A":
                wins += 1
        rate = rate_bp(wins, reference_samples)
        low, high, _ = wilson_interval(wins, reference_samples)
        # Calibration band tolerates deterministic sample-prefix differences
        # between the truth run and the user's chosen target_samples.
        expected_low = max(0, min(low, rate - 450))
        expected_high = min(10_000, max(high, rate + 450))
        rows.append(
            {
                "id": f"truth.win.{scenario.id}",
                "scenario_id": scenario.id,
                "candidate_id": candidate.id,
                "expected_low_bp": expected_low,
                "expected_high_bp": expected_high,
                "calibrated_rate_bp": rate,
                "source": "calibrated_reference_band",
                "reference_samples": reference_samples,
            }
        )
    return rows


def _threshold_truth(project: Project) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for scenario in project.scenarios:
        loadout = project.loadouts[scenario.primary_loadout_id]
        rows.append(
            {
                "id": f"truth.threshold.attack.{scenario.id}",
                "scenario_id": scenario.id,
                "attribute": "attack",
                "threshold": int(loadout.attributes["attack"] * 9_500 // 10_000),
                "comparison": ">=",
                "description": "攻击不低于该场景基准配装攻击的 95%。",
                "source": "baseline_band",
            }
        )
        rows.append(
            {
                "id": f"truth.threshold.defense.{scenario.id}",
                "scenario_id": scenario.id,
                "attribute": "defense",
                "threshold": int(loadout.attributes["defense"] * 9_000 // 10_000),
                "comparison": ">=",
                "description": "防御不低于该场景基准配装防御的 90%。",
                "source": "baseline_band",
            }
        )
    return rows


def _augment_truth(raw: dict[str, Any], candidate_set: CandidateSet) -> dict[str, Any]:
    provisional = parse_project(raw)
    baseline = next(
        candidate for candidate in candidate_set.candidates if candidate.id == "candidate.baseline"
    )
    raw["truth"] = {
        "formula_cases": _formula_truth(provisional, baseline),
        "win_rates": _win_truth(provisional, baseline),
        "thresholds": _threshold_truth(provisional),
        "policy": {
            "damage_source": "exact integer interval from formula DAG and hit bounds",
            "win_rate_source": "256-seed baseline calibration band with deterministic prefix tolerance",
            "threshold_source": "baseline attribute policy bands",
        },
    }
    return raw


def _write_json(path: Path, payload: Any) -> str:
    text = json.dumps(payload, ensure_ascii=False, sort_keys=True, indent=2) + "\n"
    path.write_text(text, encoding="utf-8")
    return content_hash(payload)


def generate_all(output_dir: str | Path = "data/samples") -> dict[str, Any]:
    root = Path(output_dir)
    root.mkdir(parents=True, exist_ok=True)
    manifests: list[dict[str, Any]] = []
    demo_requests = {
        "alpha": {"target_samples": 64, "min_samples": 2, "total_sample_budget": 1_200},
        "beta": {"target_samples": 56, "min_samples": 2, "total_sample_budget": 900},
        "gamma": {"target_samples": 80, "min_samples": 2, "total_sample_budget": 1_800},
    }
    first_raw: dict[str, Any] | None = None
    for spec in PROJECT_SPECS:
        raw = _build_raw_project(spec)
        provisional = parse_project(raw)
        candidate_set = _build_candidates(provisional)
        raw = _augment_truth(raw, candidate_set)
        project = parse_project(raw)
        project_path = root / f"{spec['slug']}.json"
        candidate_path = root / f"{spec['slug']}.candidates.json"
        project_hash = _write_json(project_path, raw)
        _write_json(candidate_path, candidate_set.to_dict())
        loaded_candidates = load_candidate_set(candidate_path, project)
        manifests.append(
            {
                "slug": spec["slug"],
                "project_id": project.project_id,
                "name": project.name,
                "project_file": project_path.name,
                "candidate_file": candidate_path.name,
                "project_hash": project_hash,
                "project_model_hash": project.content_hash(),
                "candidate_set_hash": loaded_candidates.content_hash(),
                "seed": spec["seed"],
                "classes": len(project.classes),
                "skills": len(project.skills),
                "formulas": len(project.formulas),
                "loadouts": len(project.loadouts),
                "scenarios": len(project.scenarios),
                "candidates": len(project_candidates(loaded_candidates)),
                "budget": project.validation_budget.to_dict(),
                "demo_request": demo_requests[spec["slug"]],
                "truth_counts": {
                    "formula_cases": len(project.truth["formula_cases"]),
                    "win_rates": len(project.truth["win_rates"]),
                    "thresholds": len(project.truth["thresholds"]),
                },
            }
        )
        if first_raw is None:
            first_raw = raw

    injection_cases = _injection_cases()
    injection_path = root / "injection_cases.json"
    injection_hash = _write_json(injection_path, injection_cases)
    manifest = {
        "generator_version": GENERATOR_VERSION,
        "deterministic": True,
        "scales": SCALES,
        "projects": manifests,
        "injection_cases": injection_path.name,
        "injection_cases_hash": injection_hash,
        "manifest_hash": "",
    }
    manifest["manifest_hash"] = content_hash(
        {key: value for key, value in manifest.items() if key != "manifest_hash"}
    )
    _write_json(root / "manifest.json", manifest)
    return manifest


def project_candidates(candidate_set: CandidateSet) -> tuple[Candidate, ...]:
    return candidate_set.candidates


def _injection_cases() -> dict[str, Any]:
    return {
        "schema_version": 1,
        "projects": {
            "alpha": "alpha.json",
            "beta": "beta.json",
            "gamma": "gamma.json",
        },
        "project_cases": [
            {
                "id": "missing_required_field",
                "project": "alpha.json",
                "expected_code": "E_SCHEMA",
                "expected_object_id": "skill.iron.strike",
                "expected_field": "formula_id",
                "operation": {
                    "kind": "delete_field",
                    "collection": "skills",
                    "selector": {"id": "skill.iron.strike"},
                    "field": "formula_id",
                },
            },
            {
                "id": "formula_cycle",
                "project": "alpha.json",
                "expected_code": "E_CYCLE",
                "expected_object_id": "formula.injected.cycle_a",
                "expected_field": "args",
                "operation": {"kind": "add_formula_cycle"},
            },
            {
                "id": "missing_formula_reference",
                "project": "alpha.json",
                "expected_code": "E_REF",
                "expected_object_id": "formula.injected.bad_ref",
                "expected_field": "args",
                "operation": {"kind": "add_bad_formula_reference"},
            },
            {
                "id": "missing_class_skill_reference",
                "project": "alpha.json",
                "expected_code": "E_REF",
                "expected_object_id": "class.iron",
                "expected_field": "skills",
                "operation": {
                    "kind": "append_value",
                    "collection": "classes",
                    "selector": {"id": "class.iron"},
                    "field": "skills",
                    "value": "skill.injected.missing",
                },
            },
            {
                "id": "invalid_fixed_point",
                "project": "alpha.json",
                "expected_code": "E_SCHEMA",
                "expected_object_id": "skill.iron.bash",
                "expected_field": "growth_coeff",
                "operation": {
                    "kind": "set_field",
                    "collection": "skills",
                    "selector": {"id": "skill.iron.bash"},
                    "field": "growth_coeff",
                    "value": 12.5,
                },
            },
            {
                "id": "scenario_loadout_one_to_one",
                "project": "alpha.json",
                "expected_code": "E_SCHEMA",
                "expected_object_id": "scene.duel.ranger",
                "expected_field": "primary_loadout_id",
                "operation": {
                    "kind": "set_scenario_primary",
                    "selector": {"id": "scene.duel.ranger"},
                    "value": "loadout.iron_a",
                },
            },
            {
                "id": "budget_parameter_overflow",
                "project": "alpha.json",
                "expected_code": "E_PARA",
                "expected_object_id": "validation_budget",
                "expected_field": "parallel_slots",
                "operation": {
                    "kind": "set_field",
                    "collection": "root",
                    "selector": {},
                    "field": "validation_budget.parallel_slots",
                    "value": 99,
                },
            },
        ],
        "runtime_cases": [
            {
                "id": "illegal_combo_transition",
                "expected_code": "E_STATE",
                "operation": "submit_then_submit_again",
            },
            {
                "id": "frozen_combo_adjustment",
                "expected_code": "E_STATE",
                "operation": "adjust_submitted_combo",
            },
            {
                "id": "target_samples_out_of_range",
                "expected_code": "E_PARA",
                "operation": "request_target_samples_zero",
            },
            {
                "id": "three_path_mismatch",
                "expected_code": "E_DIFF",
                "operation": "mutate_one_round_then_compare",
            },
        ],
    }


def apply_project_injection(raw: dict[str, Any], operation: dict[str, Any]) -> dict[str, Any]:
    data = json.loads(json.dumps(raw))
    kind = operation["kind"]
    collection = operation.get("collection")
    selector = operation.get("selector", {})

    def find_item() -> dict[str, Any]:
        if collection == "root":
            return data
        for item in data[collection]:
            if all(item.get(key) == value for key, value in selector.items()):
                return item
        raise KeyError(f"selector not found: {collection} {selector}")

    if kind == "delete_field":
        find_item().pop(operation["field"], None)
    elif kind == "set_field":
        field = operation["field"]
        if "." in field:
            outer, inner = field.split(".", 1)
            target = data if collection == "root" else find_item()
            target[outer][inner] = operation["value"]
        else:
            find_item()[field] = operation["value"]
    elif kind == "append_value":
        find_item()[operation["field"]].append(operation["value"])
    elif kind == "add_formula_cycle":
        data["formulas"].extend(
            [
                {
                    "id": "formula.injected.cycle_a",
                    "name": "injected cycle A",
                    "op": "ADD",
                    "args": [
                        {"kind": "const", "value": 1},
                        {"kind": "formula", "id": "formula.injected.cycle_b"},
                    ],
                },
                {
                    "id": "formula.injected.cycle_b",
                    "name": "injected cycle B",
                    "op": "ADD",
                    "args": [
                        {"kind": "const", "value": 1},
                        {"kind": "formula", "id": "formula.injected.cycle_a"},
                    ],
                },
            ]
        )
    elif kind == "add_bad_formula_reference":
        data["formulas"].append(
            {
                "id": "formula.injected.bad_ref",
                "name": "injected missing reference",
                "op": "ADD",
                "args": [
                    {"kind": "const", "value": 1},
                    {"kind": "formula", "id": "formula.injected.absent"},
                ],
            }
        )
    elif kind == "set_scenario_primary":
        target = None
        for item in data["scenarios"]:
            if all(item.get(key) == value for key, value in selector.items()):
                target = item
                break
        if target is None:
            raise KeyError(f"selector not found: scenarios {selector}")
        old_primary = target["primary_loadout_id"]
        new_primary = operation["value"]
        target["primary_loadout_id"] = new_primary
        target["party_loadout_ids"] = [
            new_primary if item == old_primary else item
            for item in target["party_loadout_ids"]
        ]
    else:
        raise ValueError(f"unknown injection kind: {kind}")
    return data
