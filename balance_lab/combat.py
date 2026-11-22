from __future__ import annotations

import time
from dataclasses import dataclass

from .errors import BalanceError, ErrorCode
from .models import (
    Candidate,
    CombatAction,
    ComboPlan,
    ComboResult,
    FormulaArg,
    Loadout,
    MatchResult,
    MatchRound,
    Project,
    Scenario,
    Skill,
)
from .util import derive_u64, match_seed, rate_bp, wilson_interval
from .validation import build_effective_values


DEFAULT_ACTIONS_PER_ROUND = 8
RESULT_OVERHEAD_BYTES = 1_024
ACTION_ESTIMATE_BYTES = 220


def estimate_result_bytes(scenario: Scenario, samples: int) -> int:
    side_size = max(len(scenario.party_loadout_ids), len(scenario.opponent_loadout_ids))
    actions_per_round = max(1, len(scenario.party_loadout_ids) + len(scenario.opponent_loadout_ids))
    bytes_per_action = ACTION_ESTIMATE_BYTES + side_size * 12
    round_bytes = 180 + actions_per_round * bytes_per_action
    # Sample-level metadata and deterministic JSON separators are included.
    return RESULT_OVERHEAD_BYTES + samples * (120 + scenario.max_rounds * round_bytes)


@dataclass(frozen=True)
class CandidateRuntime:
    project: Project
    candidate: Candidate
    skill_values: dict[str, dict[str, int]]
    loadout_values: dict[str, dict[str, int]]

    @classmethod
    def from_candidate(cls, project: Project, candidate: Candidate) -> "CandidateRuntime":
        effective = build_effective_values(project, candidate)
        return cls(
            project=project,
            candidate=candidate,
            skill_values=effective["skills"],
            loadout_values=effective["loadouts"],
        )


class FormulaEvaluator:
    """Integer interval evaluator for a formula dependency DAG."""

    def __init__(
        self,
        project: Project,
        skill_id: str,
        skill_values: dict[str, dict[str, int]],
        attacker_attrs: dict[str, int],
        defender_attrs: dict[str, int],
    ) -> None:
        self.project = project
        self.skill_id = skill_id
        self.skill_values = skill_values
        self.attacker_attrs = attacker_attrs
        self.defender_attrs = defender_attrs
        self._memo: dict[str, tuple[int, int]] = {}

    def evaluate(self, formula_id: str) -> tuple[int, int]:
        if formula_id in self._memo:
            return self._memo[formula_id]
        formula = self.project.formulas.get(formula_id)
        if formula is None:
            raise BalanceError(
                ErrorCode.REF,
                f"formula {formula_id!r} is missing at evaluation time",
                self.skill_id,
                "formula_id",
            )
        intervals = [self._eval_arg(arg) for arg in formula.args]
        if formula.op == "ADD":
            result = (
                sum(item[0] for item in intervals),
                sum(item[1] for item in intervals),
            )
        elif formula.op == "SUB":
            result = (
                max(0, intervals[0][0] - intervals[1][1]),
                max(0, intervals[0][1] - intervals[1][0]),
            )
        elif formula.op == "MAX":
            result = (
                max(intervals[0][0], intervals[1][0]),
                max(intervals[0][1], intervals[1][1]),
            )
        elif formula.op == "MIN":
            result = (
                min(intervals[0][0], intervals[1][0]),
                min(intervals[0][1], intervals[1][1]),
            )
        elif formula.op == "MUL_BP":
            products = [
                (a * b) // 10_000
                for a in intervals[0]
                for b in intervals[1]
            ]
            result = (min(products), max(products))
        elif formula.op == "DIV_BP":
            quotients = [
                (a * 10_000) // max(1, b)
                for a in intervals[0]
                for b in intervals[1]
            ]
            result = (min(quotients), max(quotients))
        else:  # Validation prevents this path.
            raise BalanceError(
                ErrorCode.SCHEMA,
                f"unsupported formula op {formula.op!r}",
                formula.id,
                "op",
            )
        self._memo[formula_id] = result
        return result

    def _eval_arg(self, arg: FormulaArg) -> tuple[int, int]:
        if arg.kind == "const":
            return arg.value, arg.value
        if arg.kind == "formula":
            return self.evaluate(arg.formula_id)
        if arg.kind == "skill":
            value = self.skill_values[self.skill_id][arg.field]
            return value, value
        if arg.kind == "attr":
            attrs = self.attacker_attrs if arg.side == "attacker" else self.defender_attrs
            value = attrs[arg.field]
            return value, value
        raise BalanceError(ErrorCode.SCHEMA, f"unsupported arg kind {arg.kind!r}", self.skill_id, "args")


@dataclass
class _Combatant:
    side: str
    index: int
    loadout_id: str
    display_id: str
    loadout_attrs: dict[str, int]
    skill_ids: tuple[str, ...]
    skill_table: dict[str, dict[str, int]]
    hp: int
    resource: int
    cooldowns: dict[str, int]

    @property
    def alive(self) -> bool:
        return self.hp > 0

    @property
    def actor_id(self) -> str:
        return f"{self.side}:{self.loadout_id}:{self.index}"


def _make_combatant(
    *,
    side: str,
    index: int,
    loadout: Loadout,
    project: Project,
    attrs: dict[str, int],
    skill_table: dict[str, dict[str, int]],
) -> _Combatant:
    character_class = project.classes[loadout.class_id]
    skills = tuple(
        sorted(
            character_class.skill_ids,
            key=lambda skill_id: (-project.skills[skill_id].priority, skill_id),
        )
    )
    return _Combatant(
        side=side,
        index=index,
        loadout_id=loadout.id,
        display_id=f"{side}:{loadout.id}#{index}",
        loadout_attrs=dict(attrs),
        skill_ids=skills,
        skill_table=skill_table,
        hp=attrs["hp"],
        resource=attrs["resource_max"],
        cooldowns={skill_id: 0 for skill_id in skills},
    )


def _side_hp(combatants: list[_Combatant]) -> int:
    return sum(max(0, item.hp) for item in combatants)


def _first_alive(combatants: list[_Combatant]) -> _Combatant | None:
    return next((item for item in combatants if item.alive), None)


def _pick_skill(combatant: _Combatant) -> tuple[Skill, dict[str, int]] | None:
    for skill_id in combatant.skill_ids:
        values = combatant.skill_table[skill_id]
        if combatant.cooldowns[skill_id] == 0 and combatant.resource >= values["resource_cost"]:
            return _skill_stub(combatant, skill_id), values
    return None


def _skill_stub(combatant: _Combatant, skill_id: str) -> Skill:
    # Only identity fields are used here; the project object resolves metadata.
    return Skill(
        id=skill_id,
        name=skill_id,
        class_id="",
        base_damage=combatant.skill_table[skill_id]["base_damage"],
        growth_coeff=combatant.skill_table[skill_id]["growth_coeff"],
        cooldown=combatant.skill_table[skill_id]["cooldown"],
        resource_cost=combatant.skill_table[skill_id]["resource_cost"],
        hit_min_bp=combatant.skill_table[skill_id]["hit_min_bp"],
        hit_max_bp=combatant.skill_table[skill_id]["hit_max_bp"],
        formula_id="",
        priority=0,
    )


def simulate_match(
    project: Project,
    runtime: CandidateRuntime,
    scenario: Scenario,
    *,
    sample_index: int,
    max_actual_seconds: float | None = None,
) -> MatchResult:
    started = time.monotonic()
    seed = match_seed(scenario.seed, sample_index)
    baseline_skill_table = {
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
    side_a: list[_Combatant] = []
    side_b: list[_Combatant] = []
    for index, loadout_id in enumerate(scenario.party_loadout_ids):
        loadout = project.loadouts[loadout_id]
        side_a.append(
            _make_combatant(
                side="A",
                index=index,
                loadout=loadout,
                project=project,
                attrs=runtime.loadout_values[loadout_id],
                skill_table=runtime.skill_values,
            )
        )
    for index, loadout_id in enumerate(scenario.opponent_loadout_ids):
        loadout = project.loadouts[loadout_id]
        side_b.append(
            _make_combatant(
                side="B",
                index=index,
                loadout=loadout,
                project=project,
                attrs=loadout.attributes,
                skill_table=baseline_skill_table,
            )
        )

    initial_hp_a = _side_hp(side_a)
    initial_hp_b = _side_hp(side_b)
    rounds: list[MatchRound] = []
    winner = "DRAW"

    for round_index in range(1, scenario.max_rounds + 1):
        actions: list[CombatAction] = []
        damage_a_to_b = 0
        damage_b_to_a = 0

        for combatant in side_a + side_b:
            if not combatant.alive:
                continue
            attrs = combatant.loadout_attrs
            combatant.resource = min(
                attrs["resource_max"],
                combatant.resource + attrs["resource_regen"],
            )

        for side, attackers, defenders in (
            ("A", side_a, side_b),
            ("B", side_b, side_a),
        ):
            for attacker in attackers:
                if not attacker.alive:
                    continue
                target = _first_alive(defenders)
                if target is None:
                    break
                picked = _pick_skill(attacker)
                if picked is None:
                    continue
                skill, values = picked
                formula_id = project.skills[skill.id].formula_id
                evaluator = FormulaEvaluator(
                    project,
                    skill.id,
                    attacker.skill_table,
                    attacker.loadout_attrs,
                    target.loadout_attrs,
                )
                raw_min, raw_max = evaluator.evaluate(formula_id)
                draw = derive_u64(seed, round_index, f"{side}{attacker.index}")
                raw_span = max(0, raw_max - raw_min)
                raw_value = raw_min + (draw & 0xFFFFFFFF) % (raw_span + 1)
                hit_span = values["hit_max_bp"] - values["hit_min_bp"]
                hit_factor = values["hit_min_bp"] + ((draw >> 32) % (hit_span + 1))
                damage = (raw_value * hit_factor) // 10_000
                damage_min = (raw_min * values["hit_min_bp"]) // 10_000
                damage_max = (raw_max * values["hit_max_bp"]) // 10_000
                resource_before = attacker.resource
                attacker.resource = max(0, attacker.resource - values["resource_cost"])
                attacker.cooldowns[skill.id] = values["cooldown"]
                target.hp = max(0, target.hp - damage)
                action = CombatAction(
                    round_index=round_index,
                    actor_id=attacker.actor_id,
                    target_id=target.actor_id,
                    skill_id=skill.id,
                    damage_min=damage_min,
                    damage_max=damage_max,
                    damage=damage,
                    hit_factor_bp=hit_factor,
                    resource_before=resource_before,
                    resource_after=attacker.resource,
                    cooldown_set=values["cooldown"],
                )
                actions.append(action)
                if side == "A":
                    damage_a_to_b += damage
                else:
                    damage_b_to_a += damage

        for combatant in side_a + side_b:
            for skill_id in combatant.cooldowns:
                if combatant.cooldowns[skill_id] > 0:
                    combatant.cooldowns[skill_id] -= 1

        hp_a = _side_hp(side_a)
        hp_b = _side_hp(side_b)
        rounds.append(
            MatchRound(
                round_index=round_index,
                damage_a_to_b=damage_a_to_b,
                damage_b_to_a=damage_b_to_a,
                hp_a=hp_a,
                hp_b=hp_b,
                actions=tuple(actions),
            )
        )
        if _first_alive(side_a) is None or _first_alive(side_b) is None:
            alive_a = _first_alive(side_a) is not None
            alive_b = _first_alive(side_b) is not None
            winner = "A" if alive_a and not alive_b else "B" if alive_b and not alive_a else "DRAW"
            break
        if max_actual_seconds is not None and time.monotonic() - started > max_actual_seconds:
            # Deterministic sample counts are declared in the budget plan.  The
            # guard protects the UI from a pathological formula while preserving
            # replay identity via the already-fixed sample prefix.
            break
    else:
        hp_a = _side_hp(side_a)
        hp_b = _side_hp(side_b)
        if hp_a * initial_hp_b > hp_b * initial_hp_a:
            winner = "A"
        elif hp_b * initial_hp_a > hp_a * initial_hp_b:
            winner = "B"
        else:
            winner = "DRAW"

    return MatchResult(
        sample_index=sample_index,
        match_seed=seed,
        winner=winner,
        rounds=tuple(rounds),
        hp_a_remaining=_side_hp(side_a),
        hp_b_remaining=_side_hp(side_b),
    )


def _mean_curve(matches: list[MatchResult], side: str) -> tuple[int, ...]:
    if not matches:
        return ()
    max_rounds = max(len(match.rounds) for match in matches)
    totals = [0] * max_rounds
    for match in matches:
        for index, round_result in enumerate(match.rounds):
            totals[index] += (
                round_result.damage_a_to_b if side == "A" else round_result.damage_b_to_a
            )
    count = len(matches)
    return tuple((value + count // 2) // count for value in totals)


def simulate_combo(
    project: Project,
    runtime: CandidateRuntime,
    scenario: Scenario,
    plan: ComboPlan,
    *,
    per_thread_buffer_kb: int,
    max_actual_seconds: float | None = None,
) -> ComboResult:
    if plan.samples <= 0:
        raise BalanceError(
            ErrorCode.PARA,
            "combo sample allocation must be positive",
            plan.candidate_id,
            "samples",
        )
    matches = [
        simulate_match(
            project,
            runtime,
            scenario,
            sample_index=sample_index,
            max_actual_seconds=max_actual_seconds,
        )
        for sample_index in range(plan.samples)
    ]
    wins_a = sum(1 for match in matches if match.winner == "A")
    wins_b = sum(1 for match in matches if match.winner == "B")
    draws = len(matches) - wins_a - wins_b
    low, high, width = wilson_interval(wins_a, len(matches))

    damage_min_total = 0
    damage_max_total = 0
    damage_mean_total = 0
    for match in matches:
        damage_min_total += sum(
            action.damage_min for round_result in match.rounds for action in round_result.actions
            if action.actor_id.startswith("A:")
        )
        damage_max_total += sum(
            action.damage_max for round_result in match.rounds for action in round_result.actions
            if action.actor_id.startswith("A:")
        )
        damage_mean_total += sum(round_result.damage_a_to_b for round_result in match.rounds)
    count = len(matches)
    result = ComboResult(
        candidate_id=plan.candidate_id,
        scenario_id=plan.scenario_id,
        samples=plan.samples,
        requested_samples=plan.requested_samples,
        reduction_reason=plan.reduction_reason,
        wins_a=wins_a,
        wins_b=wins_b,
        draws=draws,
        win_rate_bp=rate_bp(wins_a, count),
        ci_low_bp=low,
        ci_high_bp=high,
        ci_width_bp=width,
        damage_min_total=(damage_min_total + count // 2) // count,
        damage_max_total=(damage_max_total + count // 2) // count,
        damage_mean_total=(damage_mean_total + count // 2) // count,
        round_curve_a=_mean_curve(matches, "A"),
        round_curve_b=_mean_curve(matches, "B"),
        matches=tuple(matches),
    )
    actual_bytes = len(str(result.to_dict()).encode("utf-8"))
    if actual_bytes > per_thread_buffer_kb * 1024:
        raise BalanceError(
            ErrorCode.PARA,
            f"combo result needs {actual_bytes} bytes, per-thread cap is {per_thread_buffer_kb * 1024}",
            plan.candidate_id,
            "memory_buffer_kb",
            detail={"scenario_id": plan.scenario_id},
        )
    return result
