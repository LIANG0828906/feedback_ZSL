from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Iterable

from .errors import BalanceError, ErrorCode
from .models import (
    Candidate,
    CandidateModification,
    CandidateSet,
    CharacterClass,
    FormulaArg,
    FormulaNode,
    Loadout,
    Project,
    Scenario,
    Skill,
    ValidationBudget,
)
from .util import apply_scale_bp


MAX_PARALLEL_SLOTS = 8
MAX_MEMORY_BUFFER_KB = 262_144
MAX_FIXED_INT = 10**15
SKILL_FIELDS = {
    "base_damage",
    "growth_coeff",
    "cooldown",
    "resource_cost",
    "hit_min_bp",
    "hit_max_bp",
}
LOADOUT_FIELDS = {
    "hp",
    "attack",
    "defense",
    "resource_max",
    "resource_regen",
    "speed",
}
FORMULA_OPS = {
    "ADD": (2, 16),
    "SUB": (2, 2),
    "MAX": (2, 2),
    "MIN": (2, 2),
    "MUL_BP": (2, 2),
    "DIV_BP": (2, 2),
}


def _fail(code: ErrorCode, object_id: str, field: str, message: str, **detail: Any) -> None:
    raise BalanceError(code, message, object_id=object_id, field=field, detail=detail or None)


def _expect_dict(value: Any, object_id: str, field: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        _fail(ErrorCode.SCHEMA, object_id, field, "expected an object")
    return value


def _required(data: dict[str, Any], key: str, object_id: str) -> Any:
    if key not in data:
        _fail(ErrorCode.SCHEMA, object_id, key, "required field is missing")
    return data[key]


def _string(data: dict[str, Any], key: str, object_id: str, *, allow_empty: bool = False) -> str:
    value = _required(data, key, object_id)
    if not isinstance(value, str):
        _fail(ErrorCode.SCHEMA, object_id, key, "expected a string")
    if not allow_empty and not value.strip():
        _fail(ErrorCode.SCHEMA, object_id, key, "must not be empty")
    return value


def _int(
    data: dict[str, Any],
    key: str,
    object_id: str,
    *,
    minimum: int | None = None,
    maximum: int | None = None,
) -> int:
    value = _required(data, key, object_id)
    if isinstance(value, bool) or not isinstance(value, int):
        _fail(ErrorCode.SCHEMA, object_id, key, "expected a fixed-point integer, not float/bool/string")
    if minimum is not None and value < minimum:
        _fail(ErrorCode.SCHEMA, object_id, key, f"must be >= {minimum}")
    if maximum is not None and value > maximum:
        _fail(ErrorCode.SCHEMA, object_id, key, f"must be <= {maximum}")
    if abs(value) > MAX_FIXED_INT:
        _fail(ErrorCode.SCHEMA, object_id, key, f"fixed-point integer exceeds safe bound {MAX_FIXED_INT}")
    return value


def _list(data: dict[str, Any], key: str, object_id: str) -> list[Any]:
    value = _required(data, key, object_id)
    if not isinstance(value, list):
        _fail(ErrorCode.SCHEMA, object_id, key, "expected an array")
    return value


def _string_list(data: dict[str, Any], key: str, object_id: str) -> tuple[str, ...]:
    values = _list(data, key, object_id)
    result: list[str] = []
    for index, value in enumerate(values):
        if not isinstance(value, str) or not value:
            _fail(ErrorCode.SCHEMA, object_id, f"{key}[{index}]", "expected a non-empty string")
        result.append(value)
    return tuple(result)


def _object_id(raw: dict[str, Any], fallback: str) -> str:
    value = raw.get("id")
    return value if isinstance(value, str) and value else fallback


def load_project(path: str | Path) -> Project:
    source = Path(path)
    try:
        raw = json.loads(source.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise BalanceError(ErrorCode.SCHEMA, "project file does not exist", str(source), "path") from exc
    except json.JSONDecodeError as exc:
        raise BalanceError(
            ErrorCode.SCHEMA,
            f"invalid JSON: {exc.msg}",
            str(source),
            f"line:{exc.lineno}:column:{exc.colno}",
        ) from exc
    return parse_project(raw, source_path=str(source))


def parse_project(raw: Any, source_path: str = "") -> Project:
    data = _expect_dict(raw, "$", "root")
    schema_version = _int(data, "schema_version", "$", minimum=1, maximum=1)
    project_id = _string(data, "project_id", "$")
    name = _string(data, "name", "$")
    generator_seed = _int(data, "generator_seed", project_id, minimum=0)

    classes_raw = _list(data, "classes", project_id)
    skills_raw = _list(data, "skills", project_id)
    formulas_raw = _list(data, "formulas", project_id)
    loadouts_raw = _list(data, "loadouts", project_id)
    scenarios_raw = _list(data, "scenarios", project_id)
    budget_raw = _expect_dict(_required(data, "validation_budget", project_id), project_id, "validation_budget")
    truth = _expect_dict(_required(data, "truth", project_id), project_id, "truth")

    character_classes: dict[str, CharacterClass] = {}
    for index, item in enumerate(classes_raw):
        class_raw = _expect_dict(item, f"classes[{index}]", "item")
        object_id = _object_id(class_raw, f"classes[{index}]")
        character_class = CharacterClass(
            id=object_id,
            name=_string(class_raw, "name", object_id),
            role=_string(class_raw, "role", object_id),
            skill_ids=_string_list(class_raw, "skills", object_id),
        )
        if character_class.id in character_classes:
            _fail(ErrorCode.SCHEMA, character_class.id, "id", "duplicate class id")
        character_classes[character_class.id] = character_class

    skills: dict[str, Skill] = {}
    for index, item in enumerate(skills_raw):
        skill_raw = _expect_dict(item, f"skills[{index}]", "item")
        object_id = _object_id(skill_raw, f"skills[{index}]")
        skill = Skill(
            id=object_id,
            name=_string(skill_raw, "name", object_id),
            class_id=_string(skill_raw, "class_id", object_id),
            base_damage=_int(skill_raw, "base_damage", object_id, minimum=1),
            growth_coeff=_int(skill_raw, "growth_coeff", object_id, minimum=0, maximum=100_000),
            cooldown=_int(skill_raw, "cooldown", object_id, minimum=0, maximum=100),
            resource_cost=_int(skill_raw, "resource_cost", object_id, minimum=0),
            hit_min_bp=_int(skill_raw, "hit_min_bp", object_id, minimum=1, maximum=20_000),
            hit_max_bp=_int(skill_raw, "hit_max_bp", object_id, minimum=1, maximum=20_000),
            formula_id=_string(skill_raw, "formula_id", object_id),
            priority=_int(skill_raw, "priority", object_id, minimum=0, maximum=1000),
        )
        if skill.hit_min_bp > skill.hit_max_bp:
            _fail(ErrorCode.SCHEMA, object_id, "hit_min_bp", "must be <= hit_max_bp")
        if skill.class_id not in character_classes:
            _fail(ErrorCode.REF, object_id, "class_id", f"unknown class {skill.class_id!r}")
        if skill.id in skills:
            _fail(ErrorCode.SCHEMA, skill.id, "id", "duplicate skill id")
        skills[skill.id] = skill

    for character_class in character_classes.values():
        if not character_class.skill_ids:
            _fail(ErrorCode.SCHEMA, character_class.id, "skills", "class must declare at least one skill")
        for skill_id in character_class.skill_ids:
            if skill_id not in skills:
                _fail(ErrorCode.REF, character_class.id, "skills", f"references missing skill {skill_id!r}")
            if skills[skill_id].class_id != character_class.id:
                _fail(
                    ErrorCode.REF,
                    character_class.id,
                    "skills",
                    f"skill {skill_id!r} belongs to class {skills[skill_id].class_id!r}",
                )

    formulas: dict[str, FormulaNode] = {}
    for index, item in enumerate(formulas_raw):
        formula_raw = _expect_dict(item, f"formulas[{index}]", "item")
        object_id = _object_id(formula_raw, f"formulas[{index}]")
        op = _string(formula_raw, "op", object_id).upper()
        if op not in FORMULA_OPS:
            _fail(ErrorCode.SCHEMA, object_id, "op", f"unsupported formula op {op!r}")
        args_raw = _list(formula_raw, "args", object_id)
        min_args, max_args = FORMULA_OPS[op]
        if not min_args <= len(args_raw) <= max_args:
            _fail(
                ErrorCode.SCHEMA,
                object_id,
                "args",
                f"op {op} expects {min_args}..{max_args} arguments, got {len(args_raw)}",
            )
        parsed_args: list[FormulaArg] = []
        for arg_index, arg_item in enumerate(args_raw):
            arg_raw = _expect_dict(arg_item, object_id, f"args[{arg_index}]")
            kind = _string(arg_raw, "kind", object_id)
            if kind == "skill":
                field_name = _string(arg_raw, "field", object_id)
                if field_name not in {"base_damage", "growth_coeff", "cooldown", "resource_cost"}:
                    _fail(
                        ErrorCode.SCHEMA,
                        object_id,
                        f"args[{arg_index}].field",
                        f"unsupported skill formula field {field_name!r}",
                    )
                parsed_args.append(FormulaArg(kind=kind, field=field_name))
            elif kind == "attr":
                side = _string(arg_raw, "side", object_id)
                if side not in {"attacker", "defender"}:
                    _fail(ErrorCode.SCHEMA, object_id, f"args[{arg_index}].side", "expected attacker/defender")
                field_name = _string(arg_raw, "field", object_id)
                if field_name not in LOADOUT_FIELDS:
                    _fail(
                        ErrorCode.SCHEMA,
                        object_id,
                        f"args[{arg_index}].field",
                        f"unsupported attribute {field_name!r}",
                    )
                parsed_args.append(
                    FormulaArg(
                        kind=kind,
                        field=field_name,
                        side=side,
                    )
                )
            elif kind == "const":
                parsed_args.append(
                    FormulaArg(kind=kind, value=_int(arg_raw, "value", object_id))
                )
            elif kind == "formula":
                parsed_args.append(
                    FormulaArg(kind=kind, formula_id=_string(arg_raw, "id", object_id))
                )
            else:
                _fail(ErrorCode.SCHEMA, object_id, f"args[{arg_index}].kind", f"unsupported arg kind {kind!r}")
        formula = FormulaNode(
            id=object_id,
            name=_string(formula_raw, "name", object_id),
            op=op,
            args=tuple(parsed_args),
        )
        if formula.id in formulas:
            _fail(ErrorCode.SCHEMA, formula.id, "id", "duplicate formula id")
        formulas[formula.id] = formula

    for formula in formulas.values():
        for arg in formula.args:
            if arg.kind == "formula" and arg.formula_id not in formulas:
                _fail(ErrorCode.REF, formula.id, "args", f"references missing formula {arg.formula_id!r}")

    _assert_formula_dag(formulas)

    for skill in skills.values():
        if skill.formula_id not in formulas:
            _fail(ErrorCode.REF, skill.id, "formula_id", f"references missing formula {skill.formula_id!r}")

    loadouts: dict[str, Loadout] = {}
    for index, item in enumerate(loadouts_raw):
        loadout_raw = _expect_dict(item, f"loadouts[{index}]", "item")
        object_id = _object_id(loadout_raw, f"loadouts[{index}]")
        class_id = _string(loadout_raw, "class_id", object_id)
        if class_id not in character_classes:
            _fail(ErrorCode.REF, object_id, "class_id", f"unknown class {class_id!r}")
        attrs_raw = _expect_dict(_required(loadout_raw, "attributes", object_id), object_id, "attributes")
        attributes: dict[str, int] = {}
        required_attrs = {
            "hp": (1, MAX_FIXED_INT),
            "attack": (1, MAX_FIXED_INT),
            "defense": (0, MAX_FIXED_INT),
            "resource_max": (0, MAX_FIXED_INT),
            "resource_regen": (0, MAX_FIXED_INT),
            "speed": (1, MAX_FIXED_INT),
        }
        for attr_name, (minimum, maximum) in required_attrs.items():
            attributes[attr_name] = _int(
                attrs_raw, attr_name, object_id, minimum=minimum, maximum=maximum
            )
        extra_attrs = set(attrs_raw) - set(required_attrs)
        if extra_attrs:
            first = sorted(extra_attrs)[0]
            _fail(ErrorCode.SCHEMA, object_id, f"attributes.{first}", "unknown attribute")
        loadout = Loadout(
            id=object_id,
            name=_string(loadout_raw, "name", object_id),
            class_id=class_id,
            attributes=attributes,
        )
        if loadout.id in loadouts:
            _fail(ErrorCode.SCHEMA, loadout.id, "id", "duplicate loadout id")
        loadouts[loadout.id] = loadout

    _validate_budget(budget_raw)
    budget = ValidationBudget(
        parallel_slots=_int(
            budget_raw, "parallel_slots", project_id, minimum=1, maximum=MAX_PARALLEL_SLOTS
        ),
        max_matches_per_round=_int(
            budget_raw, "max_matches_per_round", project_id, minimum=1, maximum=10_000
        ),
        memory_buffer_kb=_int(
            budget_raw, "memory_buffer_kb", project_id, minimum=16, maximum=MAX_MEMORY_BUFFER_KB
        ),
        max_scenario_seconds=_int(
            budget_raw, "max_scenario_seconds", project_id, minimum=1, maximum=3_600
        ),
    )

    scenarios: list[Scenario] = []
    scenario_ids: set[str] = set()
    primary_owner: dict[str, str] = {}
    for index, item in enumerate(scenarios_raw):
        scenario_raw = _expect_dict(item, f"scenarios[{index}]", "item")
        object_id = _object_id(scenario_raw, f"scenarios[{index}]")
        mode = _string(scenario_raw, "mode", object_id)
        if mode not in {"duel", "squad"}:
            _fail(ErrorCode.SCHEMA, object_id, "mode", "expected duel or squad")
        primary = _string(scenario_raw, "primary_loadout_id", object_id)
        party = _string_list(scenario_raw, "party_loadout_ids", object_id)
        opponents = _string_list(scenario_raw, "opponent_loadout_ids", object_id)
        if not party or party[0] != primary:
            _fail(ErrorCode.SCHEMA, object_id, "party_loadout_ids", "first party member must be primary")
        for loadout_id, field in [(primary, "primary_loadout_id")] + [
            (item_id, "party_loadout_ids") for item_id in party
        ] + [(item_id, "opponent_loadout_ids") for item_id in opponents]:
            if loadout_id not in loadouts:
                _fail(ErrorCode.REF, object_id, field, f"references missing loadout {loadout_id!r}")
        if mode == "duel" and (len(party) != 1 or len(opponents) != 1):
            _fail(ErrorCode.SCHEMA, object_id, "mode", "duel requires exactly one loadout per side")
        if mode == "squad" and (len(party) < 2 or len(opponents) < 2):
            _fail(ErrorCode.SCHEMA, object_id, "mode", "squad requires at least two loadouts per side")
        scenario = Scenario(
            id=object_id,
            name=_string(scenario_raw, "name", object_id),
            mode=mode,
            primary_loadout_id=primary,
            party_loadout_ids=party,
            opponent_loadout_ids=opponents,
            max_rounds=_int(scenario_raw, "max_rounds", object_id, minimum=1, maximum=200),
            seed=_int(scenario_raw, "seed", object_id, minimum=0),
            estimated_ms_per_match=_int(
                scenario_raw, "estimated_ms_per_match", object_id, minimum=1, maximum=60_000
            ),
        )
        if scenario.id in scenario_ids:
            _fail(ErrorCode.SCHEMA, scenario.id, "id", "duplicate scenario id")
        if primary in primary_owner:
            _fail(
                ErrorCode.SCHEMA,
                scenario.id,
                "primary_loadout_id",
                f"loadout {primary!r} is already declared by {primary_owner[primary]!r}",
            )
        scenario_ids.add(scenario.id)
        primary_owner[primary] = scenario.id
        scenarios.append(scenario)
    if not scenarios:
        _fail(ErrorCode.SCHEMA, project_id, "scenarios", "at least one scenario is required")
    missing_primaries = sorted(set(loadouts) - set(primary_owner))
    if missing_primaries:
        _fail(
            ErrorCode.SCHEMA,
            project_id,
            "scenarios",
            "each loadout must own exactly one primary scenario",
            missing_loadouts=missing_primaries,
        )

    _validate_truth(truth, project_id, skills, scenarios)

    project = Project(
        schema_version=schema_version,
        project_id=project_id,
        name=name,
        generator_seed=generator_seed,
        classes=character_classes,
        skills=skills,
        formulas=formulas,
        loadouts=loadouts,
        scenarios=tuple(scenarios),
        validation_budget=budget,
        truth=truth,
        source_path=source_path,
    )
    return Project(**{**project.__dict__, "content_hash_value": project.content_hash()})


def _validate_budget(budget_raw: dict[str, Any]) -> None:
    object_id = "validation_budget"
    parallel_slots = _int(budget_raw, "parallel_slots", object_id)
    max_matches = _int(budget_raw, "max_matches_per_round", object_id)
    memory = _int(budget_raw, "memory_buffer_kb", object_id)
    seconds = _int(budget_raw, "max_scenario_seconds", object_id)
    if not 1 <= parallel_slots <= MAX_PARALLEL_SLOTS:
        _fail(
            ErrorCode.PARA,
            object_id,
            "parallel_slots",
            f"must be in [1, {MAX_PARALLEL_SLOTS}], got {parallel_slots}",
        )
    if not 1 <= max_matches <= 10_000:
        _fail(ErrorCode.PARA, object_id, "max_matches_per_round", "must be in [1, 10000]")
    if not 16 <= memory <= MAX_MEMORY_BUFFER_KB:
        _fail(
            ErrorCode.PARA,
            object_id,
            "memory_buffer_kb",
            f"must be in [16, {MAX_MEMORY_BUFFER_KB}]",
        )
    if not 1 <= seconds <= 3_600:
        _fail(ErrorCode.PARA, object_id, "max_scenario_seconds", "must be in [1, 3600]")


def _assert_formula_dag(formulas: dict[str, FormulaNode]) -> None:
    state: dict[str, int] = {}
    stack: list[str] = []

    def visit(node_id: str) -> None:
        marker = state.get(node_id, 0)
        if marker == 2:
            return
        if marker == 1:
            if node_id in stack:
                start = stack.index(node_id)
                cycle = stack[start:] + [node_id]
            else:
                cycle = [node_id, node_id]
            _fail(
                ErrorCode.CYCLE,
                node_id,
                "args",
                "formula dependency graph contains a cycle",
                cycle=cycle,
            )
        state[node_id] = 1
        stack.append(node_id)
        for dependency in formulas[node_id].formula_dependencies():
            visit(dependency)
        stack.pop()
        state[node_id] = 2

    for formula_id in sorted(formulas):
        visit(formula_id)


def _validate_truth(
    truth: dict[str, Any],
    project_id: str,
    skills: dict[str, Skill],
    scenarios: list[Scenario],
) -> None:
    formula_cases = _list(truth, "formula_cases", project_id)
    win_rates = _list(truth, "win_rates", project_id)
    thresholds = _list(truth, "thresholds", project_id)
    scenario_ids = {scenario.id for scenario in scenarios}
    for index, item in enumerate(formula_cases):
        case = _expect_dict(item, f"truth.formula_cases[{index}]", "item")
        object_id = _object_id(case, f"truth.formula_cases[{index}]")
        skill_id = _string(case, "skill_id", object_id)
        if skill_id not in skills:
            _fail(ErrorCode.REF, object_id, "skill_id", f"unknown skill {skill_id!r}")
        _int(case, "expected_min", object_id, minimum=0)
        _int(case, "expected_max", object_id, minimum=0)
        _int(case, "expected_mid", object_id, minimum=0)
    for index, item in enumerate(win_rates):
        row = _expect_dict(item, f"truth.win_rates[{index}]", "item")
        object_id = _object_id(row, f"truth.win_rates[{index}]")
        scenario_id = _string(row, "scenario_id", object_id)
        if scenario_id not in scenario_ids:
            _fail(ErrorCode.REF, object_id, "scenario_id", f"unknown scenario {scenario_id!r}")
        low = _int(row, "expected_low_bp", object_id, minimum=0, maximum=10_000)
        high = _int(row, "expected_high_bp", object_id, minimum=0, maximum=10_000)
        if low > high:
            _fail(ErrorCode.SCHEMA, object_id, "expected_low_bp", "must be <= expected_high_bp")
    for index, item in enumerate(thresholds):
        row = _expect_dict(item, f"truth.thresholds[{index}]", "item")
        object_id = _object_id(row, f"truth.thresholds[{index}]")
        scenario_id = _string(row, "scenario_id", object_id)
        if scenario_id not in scenario_ids:
            _fail(ErrorCode.REF, object_id, "scenario_id", f"unknown scenario {scenario_id!r}")
        attr = _string(row, "attribute", object_id)
        if attr not in LOADOUT_FIELDS:
            _fail(ErrorCode.SCHEMA, object_id, "attribute", f"unknown attribute {attr!r}")
        _int(row, "threshold", object_id, minimum=0)


def load_candidate_set(path: str | Path, project: Project | None = None) -> CandidateSet:
    source = Path(path)
    try:
        raw = json.loads(source.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise BalanceError(ErrorCode.SCHEMA, "candidate file does not exist", str(source), "path") from exc
    except json.JSONDecodeError as exc:
        raise BalanceError(ErrorCode.SCHEMA, f"invalid JSON: {exc.msg}", str(source), "json") from exc
    return parse_candidate_set(raw, project=project, source_path=str(source))


def parse_candidate_set(
    raw: Any,
    project: Project | None = None,
    source_path: str = "",
) -> CandidateSet:
    data = _expect_dict(raw, "$", "candidate_set")
    project_id = _string(data, "project_id", "candidate_set")
    if project is not None and project.project_id != project_id:
        _fail(
            ErrorCode.REF,
            project_id,
            "project_id",
            f"candidate set targets {project_id!r}, project is {project.project_id!r}",
        )
    items = _list(data, "candidates", project_id)
    candidates: list[Candidate] = []
    seen: set[str] = set()
    for index, item in enumerate(items):
        candidate_raw = _expect_dict(item, f"candidates[{index}]", "item")
        object_id = _object_id(candidate_raw, f"candidates[{index}]")
        mods_raw = _list(candidate_raw, "modifications", object_id)
        modifications: list[CandidateModification] = []
        for mod_index, mod_item in enumerate(mods_raw):
            mod_raw = _expect_dict(mod_item, object_id, f"modifications[{mod_index}]")
            target_kind = _string(mod_raw, "target_kind", object_id)
            if target_kind not in {"skill", "loadout"}:
                _fail(ErrorCode.SCHEMA, object_id, f"modifications[{mod_index}].target_kind", "expected skill/loadout")
            target_id = _string(mod_raw, "target_id", object_id)
            field_name = _string(mod_raw, "field", object_id)
            op = _string(mod_raw, "op", object_id)
            if op not in {"scale_bp", "add"}:
                _fail(ErrorCode.SCHEMA, object_id, f"modifications[{mod_index}].op", "expected scale_bp/add")
            value = _int(mod_raw, "value", object_id)
            modifications.append(
                CandidateModification(
                    target_kind=target_kind,
                    target_id=target_id,
                    field=field_name,
                    op=op,
                    value=value,
                )
            )
        candidate = Candidate(
            id=object_id,
            name=_string(candidate_raw, "name", object_id),
            priority=_int(candidate_raw, "priority", object_id, minimum=0, maximum=1_000_000),
            modifications=tuple(modifications),
            note=_string(candidate_raw, "note", object_id, allow_empty=True),
        )
        if candidate.id in seen:
            _fail(ErrorCode.SCHEMA, candidate.id, "id", "duplicate candidate id")
        seen.add(candidate.id)
        candidates.append(candidate)
    if not candidates:
        _fail(ErrorCode.SCHEMA, project_id, "candidates", "at least one candidate is required")
    candidate_set = CandidateSet(project_id=project_id, candidates=tuple(candidates), source_path=source_path)
    if project is not None:
        validate_candidates(project, candidate_set.candidates)
    return candidate_set


def validate_candidates(project: Project, candidates: Iterable[Candidate]) -> None:
    for candidate in candidates:
        for mod in candidate.modifications:
            if mod.target_kind == "skill":
                if mod.target_id not in project.skills:
                    _fail(ErrorCode.REF, candidate.id, "modifications", f"unknown skill {mod.target_id!r}")
                if mod.field not in SKILL_FIELDS:
                    _fail(
                        ErrorCode.SCHEMA,
                        candidate.id,
                        "modifications",
                        f"skill field {mod.field!r} is not adjustable",
                    )
                if mod.field == "cooldown" and mod.op == "scale_bp":
                    _fail(ErrorCode.PARA, candidate.id, "modifications", "cooldown only supports add")
                if mod.op == "scale_bp" and not 1_000 <= mod.value <= 30_000:
                    _fail(ErrorCode.PARA, candidate.id, "modifications", "scale_bp factor must be in [1000, 30000]")
                if mod.op == "add" and not -1_000_000_000 <= mod.value <= 1_000_000_000:
                    _fail(ErrorCode.PARA, candidate.id, "modifications", "add delta is outside safe range")
            else:
                if mod.target_id not in project.loadouts:
                    _fail(ErrorCode.REF, candidate.id, "modifications", f"unknown loadout {mod.target_id!r}")
                if mod.field not in LOADOUT_FIELDS:
                    _fail(
                        ErrorCode.SCHEMA,
                        candidate.id,
                        "modifications",
                        f"loadout field {mod.field!r} is not adjustable",
                    )
                if mod.op == "scale_bp" and not 1_000 <= mod.value <= 30_000:
                    _fail(ErrorCode.PARA, candidate.id, "modifications", "scale_bp factor must be in [1000, 30000]")
                if mod.op == "add" and not -1_000_000_000 <= mod.value <= 1_000_000_000:
                    _fail(ErrorCode.PARA, candidate.id, "modifications", "add delta is outside safe range")
        effective = build_effective_values(project, candidate)
        for target_id, attrs in effective["loadouts"].items():
            if attrs["hp"] <= 0 or attrs["attack"] <= 0 or attrs["speed"] <= 0:
                _fail(ErrorCode.PARA, candidate.id, "modifications", f"loadout {target_id!r} becomes invalid")
        for target_id, attrs in effective["skills"].items():
            if attrs["base_damage"] <= 0 or attrs["cooldown"] < 0 or attrs["resource_cost"] < 0:
                _fail(ErrorCode.PARA, candidate.id, "modifications", f"skill {target_id!r} becomes invalid")
            if attrs["hit_min_bp"] > attrs["hit_max_bp"]:
                _fail(ErrorCode.PARA, candidate.id, "modifications", f"skill {target_id!r} hit range is inverted")


def build_effective_values(project: Project, candidate: Candidate) -> dict[str, dict[str, dict[str, int]]]:
    """Build immutable candidate-adjusted numeric tables.

    The project itself is never mutated; each candidate receives an isolated
    value table used by all of its simulations.
    """

    skills = {
        skill_id: {
            "base_damage": skill.base_damage,
            "growth_coeff": skill.growth_coeff,
            "cooldown": skill.cooldown,
            "resource_cost": skill.resource_cost,
            "hit_min_bp": skill.hit_min_bp,
            "hit_max_bp": skill.hit_max_bp,
        }
        for skill_id, skill in project.skills.items()
    }
    loadouts = {
        loadout_id: dict(loadout.attributes)
        for loadout_id, loadout in project.loadouts.items()
    }
    for mod in candidate.modifications:
        table = skills if mod.target_kind == "skill" else loadouts
        value = table[mod.target_id][mod.field]
        table[mod.target_id][mod.field] = (
            apply_scale_bp(value, mod.value) if mod.op == "scale_bp" else value + mod.value
        )
    return {"skills": skills, "loadouts": loadouts}
