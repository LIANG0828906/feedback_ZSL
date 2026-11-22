from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable

from .util import content_hash


@dataclass(frozen=True)
class FormulaArg:
    kind: str
    field: str = ""
    side: str = ""
    value: int = 0
    formula_id: str = ""

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {"kind": self.kind}
        if self.kind == "skill":
            payload["field"] = self.field
        elif self.kind == "attr":
            payload["side"] = self.side
            payload["field"] = self.field
        elif self.kind == "const":
            payload["value"] = self.value
        elif self.kind == "formula":
            payload["id"] = self.formula_id
        return payload


@dataclass(frozen=True)
class FormulaNode:
    id: str
    name: str
    op: str
    args: tuple[FormulaArg, ...]

    def formula_dependencies(self) -> tuple[str, ...]:
        return tuple(arg.formula_id for arg in self.args if arg.kind == "formula")

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "name": self.name,
            "op": self.op,
            "args": [arg.to_dict() for arg in self.args],
        }


@dataclass(frozen=True)
class Skill:
    id: str
    name: str
    class_id: str
    base_damage: int
    growth_coeff: int
    cooldown: int
    resource_cost: int
    hit_min_bp: int
    hit_max_bp: int
    formula_id: str
    priority: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "name": self.name,
            "class_id": self.class_id,
            "base_damage": self.base_damage,
            "growth_coeff": self.growth_coeff,
            "cooldown": self.cooldown,
            "resource_cost": self.resource_cost,
            "hit_min_bp": self.hit_min_bp,
            "hit_max_bp": self.hit_max_bp,
            "formula_id": self.formula_id,
            "priority": self.priority,
        }


@dataclass(frozen=True)
class CharacterClass:
    id: str
    name: str
    role: str
    skill_ids: tuple[str, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "name": self.name,
            "role": self.role,
            "skills": list(self.skill_ids),
        }


@dataclass(frozen=True)
class Loadout:
    id: str
    name: str
    class_id: str
    attributes: dict[str, int]

    @property
    def hp(self) -> int:
        return self.attributes["hp"]

    @property
    def attack(self) -> int:
        return self.attributes["attack"]

    @property
    def defense(self) -> int:
        return self.attributes["defense"]

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "name": self.name,
            "class_id": self.class_id,
            "attributes": dict(sorted(self.attributes.items())),
        }


@dataclass(frozen=True)
class ValidationBudget:
    parallel_slots: int
    max_matches_per_round: int
    memory_buffer_kb: int
    max_scenario_seconds: int

    def to_dict(self) -> dict[str, int]:
        return {
            "parallel_slots": self.parallel_slots,
            "max_matches_per_round": self.max_matches_per_round,
            "memory_buffer_kb": self.memory_buffer_kb,
            "max_scenario_seconds": self.max_scenario_seconds,
        }


@dataclass(frozen=True)
class Scenario:
    id: str
    name: str
    mode: str
    primary_loadout_id: str
    party_loadout_ids: tuple[str, ...]
    opponent_loadout_ids: tuple[str, ...]
    max_rounds: int
    seed: int
    estimated_ms_per_match: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "name": self.name,
            "mode": self.mode,
            "primary_loadout_id": self.primary_loadout_id,
            "party_loadout_ids": list(self.party_loadout_ids),
            "opponent_loadout_ids": list(self.opponent_loadout_ids),
            "max_rounds": self.max_rounds,
            "seed": self.seed,
            "estimated_ms_per_match": self.estimated_ms_per_match,
        }


@dataclass(frozen=True)
class Project:
    schema_version: int
    project_id: str
    name: str
    generator_seed: int
    classes: dict[str, CharacterClass]
    skills: dict[str, Skill]
    formulas: dict[str, FormulaNode]
    loadouts: dict[str, Loadout]
    scenarios: tuple[Scenario, ...]
    validation_budget: ValidationBudget
    truth: dict[str, Any]
    source_path: str = ""
    content_hash_value: str = ""

    def engineering_payload(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "project_id": self.project_id,
            "name": self.name,
            "generator_seed": self.generator_seed,
            "classes": [self.classes[key].to_dict() for key in sorted(self.classes)],
            "skills": [self.skills[key].to_dict() for key in sorted(self.skills)],
            "formulas": [self.formulas[key].to_dict() for key in sorted(self.formulas)],
            "loadouts": [self.loadouts[key].to_dict() for key in sorted(self.loadouts)],
            "scenarios": [scenario.to_dict() for scenario in self.scenarios],
            "validation_budget": self.validation_budget.to_dict(),
            "truth": self.truth,
        }

    def content_hash(self) -> str:
        if self.content_hash_value:
            return self.content_hash_value
        return content_hash(self.engineering_payload())

    def scenario_map(self) -> dict[str, Scenario]:
        return {scenario.id: scenario for scenario in self.scenarios}

    def class_skills(self, class_id: str) -> tuple[Skill, ...]:
        character_class = self.classes[class_id]
        return tuple(self.skills[skill_id] for skill_id in character_class.skill_ids)


@dataclass(frozen=True)
class CandidateModification:
    target_kind: str
    target_id: str
    field: str
    op: str
    value: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "target_kind": self.target_kind,
            "target_id": self.target_id,
            "field": self.field,
            "op": self.op,
            "value": self.value,
        }


@dataclass(frozen=True)
class Candidate:
    id: str
    name: str
    priority: int
    modifications: tuple[CandidateModification, ...]
    note: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "name": self.name,
            "priority": self.priority,
            "modifications": [mod.to_dict() for mod in self.modifications],
            "note": self.note,
        }


@dataclass(frozen=True)
class CandidateSet:
    project_id: str
    candidates: tuple[Candidate, ...]
    source_path: str = ""

    def content_hash(self) -> str:
        return content_hash(
            {
                "project_id": self.project_id,
                "candidates": [candidate.to_dict() for candidate in self.candidates],
            }
        )

    def by_id(self) -> dict[str, Candidate]:
        return {candidate.id: candidate for candidate in self.candidates}

    def to_dict(self) -> dict[str, Any]:
        return {
            "project_id": self.project_id,
            "candidates": [candidate.to_dict() for candidate in self.candidates],
        }


@dataclass(frozen=True)
class CombatAction:
    round_index: int
    actor_id: str
    target_id: str
    skill_id: str
    damage_min: int
    damage_max: int
    damage: int
    hit_factor_bp: int
    resource_before: int
    resource_after: int
    cooldown_set: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "actor_id": self.actor_id,
            "target_id": self.target_id,
            "skill_id": self.skill_id,
            "damage_min": self.damage_min,
            "damage_max": self.damage_max,
            "damage": self.damage,
            "hit_factor_bp": self.hit_factor_bp,
            "resource_before": self.resource_before,
            "resource_after": self.resource_after,
            "cooldown_set": self.cooldown_set,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any], round_index: int) -> "CombatAction":
        return cls(
            round_index=round_index,
            actor_id=data["actor_id"],
            target_id=data["target_id"],
            skill_id=data["skill_id"],
            damage_min=int(data["damage_min"]),
            damage_max=int(data["damage_max"]),
            damage=int(data["damage"]),
            hit_factor_bp=int(data["hit_factor_bp"]),
            resource_before=int(data["resource_before"]),
            resource_after=int(data["resource_after"]),
            cooldown_set=int(data["cooldown_set"]),
        )


@dataclass(frozen=True)
class MatchRound:
    round_index: int
    damage_a_to_b: int
    damage_b_to_a: int
    hp_a: int
    hp_b: int
    actions: tuple[CombatAction, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "round": self.round_index,
            "damage_a_to_b": self.damage_a_to_b,
            "damage_b_to_a": self.damage_b_to_a,
            "hp_a": self.hp_a,
            "hp_b": self.hp_b,
            "actions": [action.to_dict() for action in self.actions],
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "MatchRound":
        round_index = int(data["round"])
        return cls(
            round_index=round_index,
            damage_a_to_b=int(data["damage_a_to_b"]),
            damage_b_to_a=int(data["damage_b_to_a"]),
            hp_a=int(data["hp_a"]),
            hp_b=int(data["hp_b"]),
            actions=tuple(CombatAction.from_dict(item, round_index) for item in data["actions"]),
        )


@dataclass(frozen=True)
class MatchResult:
    sample_index: int
    match_seed: int
    winner: str
    rounds: tuple[MatchRound, ...]
    hp_a_remaining: int
    hp_b_remaining: int

    @property
    def round_damage_a(self) -> tuple[int, ...]:
        return tuple(item.damage_a_to_b for item in self.rounds)

    @property
    def round_damage_b(self) -> tuple[int, ...]:
        return tuple(item.damage_b_to_a for item in self.rounds)

    def to_dict(self) -> dict[str, Any]:
        return {
            "sample_index": self.sample_index,
            "match_seed": self.match_seed,
            "winner": self.winner,
            "rounds": [item.to_dict() for item in self.rounds],
            "hp_a_remaining": self.hp_a_remaining,
            "hp_b_remaining": self.hp_b_remaining,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "MatchResult":
        return cls(
            sample_index=int(data["sample_index"]),
            match_seed=int(data["match_seed"]),
            winner=str(data["winner"]),
            rounds=tuple(MatchRound.from_dict(item) for item in data["rounds"]),
            hp_a_remaining=int(data["hp_a_remaining"]),
            hp_b_remaining=int(data["hp_b_remaining"]),
        )


@dataclass(frozen=True)
class BudgetDecision:
    sequence: int
    action: str
    reason: str
    combo_count: int
    budget_remaining: int
    detail: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "sequence": self.sequence,
            "action": self.action,
            "reason": self.reason,
            "combo_count": self.combo_count,
            "budget_remaining": self.budget_remaining,
            "detail": self.detail,
        }


@dataclass(frozen=True)
class ComboPlan:
    order: int
    candidate_id: str
    scenario_id: str
    requested_samples: int
    samples: int
    memory_cap: int
    time_cap: int
    reduction_reason: str
    estimated_result_bytes: int

    @property
    def key(self) -> str:
        return f"{self.candidate_id}::{self.scenario_id}"

    def to_dict(self) -> dict[str, Any]:
        return {
            "order": self.order,
            "candidate_id": self.candidate_id,
            "scenario_id": self.scenario_id,
            "requested_samples": self.requested_samples,
            "samples": self.samples,
            "memory_cap": self.memory_cap,
            "time_cap": self.time_cap,
            "reduction_reason": self.reduction_reason,
            "estimated_result_bytes": self.estimated_result_bytes,
        }


@dataclass(frozen=True)
class BudgetPlan:
    target_samples: int
    min_samples: int
    total_sample_budget: int | None
    combos: tuple[ComboPlan, ...]
    decisions: tuple[BudgetDecision, ...]
    per_thread_buffer_kb: int

    def by_key(self) -> dict[str, ComboPlan]:
        return {combo.key: combo for combo in self.combos}

    def to_dict(self) -> dict[str, Any]:
        return {
            "target_samples": self.target_samples,
            "min_samples": self.min_samples,
            "total_sample_budget": self.total_sample_budget,
            "combos": [combo.to_dict() for combo in self.combos],
            "decisions": [decision.to_dict() for decision in self.decisions],
            "per_thread_buffer_kb": self.per_thread_buffer_kb,
        }


@dataclass(frozen=True)
class ComboResult:
    candidate_id: str
    scenario_id: str
    samples: int
    requested_samples: int
    reduction_reason: str
    wins_a: int
    wins_b: int
    draws: int
    win_rate_bp: int
    ci_low_bp: int
    ci_high_bp: int
    ci_width_bp: int
    damage_min_total: int
    damage_max_total: int
    damage_mean_total: int
    round_curve_a: tuple[int, ...]
    round_curve_b: tuple[int, ...]
    matches: tuple[MatchResult, ...]
    content_hash_value: str = ""

    def unsigned_payload(self) -> dict[str, Any]:
        return {
            "candidate_id": self.candidate_id,
            "scenario_id": self.scenario_id,
            "samples": self.samples,
            "requested_samples": self.requested_samples,
            "reduction_reason": self.reduction_reason,
            "wins_a": self.wins_a,
            "wins_b": self.wins_b,
            "draws": self.draws,
            "win_rate_bp": self.win_rate_bp,
            "ci_low_bp": self.ci_low_bp,
            "ci_high_bp": self.ci_high_bp,
            "ci_width_bp": self.ci_width_bp,
            "damage_min_total": self.damage_min_total,
            "damage_max_total": self.damage_max_total,
            "damage_mean_total": self.damage_mean_total,
            "round_curve_a": list(self.round_curve_a),
            "round_curve_b": list(self.round_curve_b),
            "matches": [match.to_dict() for match in self.matches],
        }

    def to_dict(self) -> dict[str, Any]:
        payload = self.unsigned_payload()
        payload["content_hash"] = self.content_hash()
        return payload

    def content_hash(self) -> str:
        if self.content_hash_value:
            return self.content_hash_value
        return content_hash(self.unsigned_payload())

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ComboResult":
        return cls(
            candidate_id=str(data["candidate_id"]),
            scenario_id=str(data["scenario_id"]),
            samples=int(data["samples"]),
            requested_samples=int(data["requested_samples"]),
            reduction_reason=str(data["reduction_reason"]),
            wins_a=int(data["wins_a"]),
            wins_b=int(data["wins_b"]),
            draws=int(data["draws"]),
            win_rate_bp=int(data["win_rate_bp"]),
            ci_low_bp=int(data["ci_low_bp"]),
            ci_high_bp=int(data["ci_high_bp"]),
            ci_width_bp=int(data["ci_width_bp"]),
            damage_min_total=int(data["damage_min_total"]),
            damage_max_total=int(data["damage_max_total"]),
            damage_mean_total=int(data["damage_mean_total"]),
            round_curve_a=tuple(int(item) for item in data["round_curve_a"]),
            round_curve_b=tuple(int(item) for item in data["round_curve_b"]),
            matches=tuple(MatchResult.from_dict(item) for item in data["matches"]),
            content_hash_value=str(data.get("content_hash", "")),
        )


@dataclass(frozen=True)
class CandidateSummary:
    candidate_id: str
    name: str
    state: str
    submitted_combos: int
    total_combos: int
    win_rate_bp: int
    damage_min_total: int
    damage_max_total: int
    damage_mean_total: int
    samples: int


@dataclass(frozen=True)
class ScheduleStats:
    configured_parallel_slots: int
    actual_peak_slots: int
    dispatch_rounds: int
    total_combos: int
    committed_combos: int
    queued_peak: int
    blocked_waves: int
    restored_combos: int
    simulated_combos: int
    per_thread_buffer_kb: int

    def to_dict(self) -> dict[str, int]:
        return {
            "configured_parallel_slots": self.configured_parallel_slots,
            "actual_peak_slots": self.actual_peak_slots,
            "dispatch_rounds": self.dispatch_rounds,
            "total_combos": self.total_combos,
            "committed_combos": self.committed_combos,
            "queued_peak": self.queued_peak,
            "blocked_waves": self.blocked_waves,
            "restored_combos": self.restored_combos,
            "simulated_combos": self.simulated_combos,
            "per_thread_buffer_kb": self.per_thread_buffer_kb,
        }


@dataclass
class RunResult:
    project_id: str
    mode: str
    candidates: list[CandidateSummary]
    combos: list[ComboResult]
    plan: BudgetPlan
    schedule: ScheduleStats
    state_snapshot: dict[str, Any]
    checkpoint_fingerprint: str = ""
    interrupted: bool = False

    def combo_map(self) -> dict[tuple[str, str], ComboResult]:
        return {(combo.candidate_id, combo.scenario_id): combo for combo in self.combos}

    def to_dict(self) -> dict[str, Any]:
        return {
            "project_id": self.project_id,
            "mode": self.mode,
            "candidates": [
                {
                    "candidate_id": item.candidate_id,
                    "name": item.name,
                    "state": item.state,
                    "submitted_combos": item.submitted_combos,
                    "total_combos": item.total_combos,
                    "win_rate_bp": item.win_rate_bp,
                    "damage_min_total": item.damage_min_total,
                    "damage_max_total": item.damage_max_total,
                    "damage_mean_total": item.damage_mean_total,
                    "samples": item.samples,
                }
                for item in self.candidates
            ],
            "combos": [combo.to_dict() for combo in self.combos],
            "plan": self.plan.to_dict(),
            "schedule": self.schedule.to_dict(),
            "state_snapshot": self.state_snapshot,
            "checkpoint_fingerprint": self.checkpoint_fingerprint,
            "interrupted": self.interrupted,
        }


def flatten_results(results: Iterable[ComboResult]) -> dict[tuple[str, str], ComboResult]:
    return {(result.candidate_id, result.scenario_id): result for result in results}
