from __future__ import annotations

from dataclasses import dataclass

from .combat import RESULT_OVERHEAD_BYTES, estimate_result_bytes
from .errors import BalanceError, ErrorCode
from .models import (
    BudgetDecision,
    BudgetPlan,
    Candidate,
    ComboPlan,
    Project,
    Scenario,
)
from .util import content_hash


MAX_REQUEST_SAMPLES = 4_096


@dataclass(frozen=True)
class SimulationRequest:
    target_samples: int = 40
    min_samples: int = 2
    total_sample_budget: int | None = None
    interrupt_after_commits: int | None = None

    def validate(self, combo_count: int) -> None:
        if isinstance(self.target_samples, bool) or not isinstance(self.target_samples, int):
            raise BalanceError(ErrorCode.PARA, "target_samples must be an integer", "request", "target_samples")
        if not 1 <= self.target_samples <= MAX_REQUEST_SAMPLES:
            raise BalanceError(
                ErrorCode.PARA,
                f"target_samples must be in [1, {MAX_REQUEST_SAMPLES}]",
                "request",
                "target_samples",
            )
        if isinstance(self.min_samples, bool) or not isinstance(self.min_samples, int):
            raise BalanceError(ErrorCode.PARA, "min_samples must be an integer", "request", "min_samples")
        if not 1 <= self.min_samples <= self.target_samples:
            raise BalanceError(
                ErrorCode.PARA,
                "min_samples must be in [1, target_samples]",
                "request",
                "min_samples",
            )
        if self.total_sample_budget is not None:
            if isinstance(self.total_sample_budget, bool) or not isinstance(self.total_sample_budget, int):
                raise BalanceError(
                    ErrorCode.PARA,
                    "total_sample_budget must be an integer or null",
                    "request",
                    "total_sample_budget",
                )
            minimum_required = combo_count * self.min_samples
            if self.total_sample_budget < minimum_required:
                raise BalanceError(
                    ErrorCode.PARA,
                    f"total_sample_budget cannot provide min_samples to all combos ({minimum_required})",
                    "request",
                    "total_sample_budget",
                    detail={"required": minimum_required, "provided": self.total_sample_budget},
                )
        if self.interrupt_after_commits is not None:
            if (
                isinstance(self.interrupt_after_commits, bool)
                or not isinstance(self.interrupt_after_commits, int)
                or self.interrupt_after_commits < 1
            ):
                raise BalanceError(
                    ErrorCode.PARA,
                    "interrupt_after_commits must be a positive integer or null",
                    "request",
                    "interrupt_after_commits",
                )

    def to_dict(self) -> dict[str, int | None]:
        return {
            "target_samples": self.target_samples,
            "min_samples": self.min_samples,
            "total_sample_budget": self.total_sample_budget,
            "interrupt_after_commits": self.interrupt_after_commits,
        }


def ordered_combinations(
    project: Project,
    candidates: tuple[Candidate, ...] | list[Candidate],
) -> list[tuple[Candidate, Scenario]]:
    indexed = list(enumerate(candidates))
    indexed.sort(key=lambda item: (-item[1].priority, item[0], item[1].id))
    combos: list[tuple[Candidate, Scenario]] = []
    for _, candidate in indexed:
        for scenario in project.scenarios:
            combos.append((candidate, scenario))
    return combos


def _cap_reasons(scenario: Scenario, memory_cap: int, time_cap: int) -> str:
    reasons: list[str] = []
    if memory_cap < time_cap:
        reasons.append("MEMORY_BUFFER")
    elif time_cap < memory_cap:
        reasons.append("SCENARIO_TIME")
    else:
        reasons.append("MEMORY_BUFFER+SCENARIO_TIME")
    if time_cap <= 0:
        reasons.append("ZERO_TIME_BUDGET")
    return "+".join(reasons)


def build_budget_plan(
    project: Project,
    candidates: tuple[Candidate, ...] | list[Candidate],
    request: SimulationRequest,
) -> BudgetPlan:
    combos = ordered_combinations(project, candidates)
    request.validate(len(combos))
    budget = project.validation_budget
    per_thread_buffer_kb = max(1, budget.memory_buffer_kb // budget.parallel_slots)
    buffer_bytes = per_thread_buffer_kb * 1024
    plans: list[ComboPlan] = []
    decisions: list[BudgetDecision] = []
    sequence = 1

    for order, (candidate, scenario) in enumerate(combos):
        estimated = estimate_result_bytes(scenario, request.target_samples)
        per_sample = max(1, (estimated - RESULT_OVERHEAD_BYTES) // max(1, request.target_samples))
        memory_cap = max(1, (buffer_bytes - RESULT_OVERHEAD_BYTES) // per_sample)
        time_cap = max(1, budget.max_scenario_seconds * 1000 // scenario.estimated_ms_per_match)
        resource_cap = min(memory_cap, time_cap)
        reduction_reason = "" if request.target_samples <= resource_cap else _cap_reasons(
            scenario, memory_cap, time_cap
        )
        plans.append(
            ComboPlan(
                order=order,
                candidate_id=candidate.id,
                scenario_id=scenario.id,
                requested_samples=request.target_samples,
                samples=0,
                memory_cap=memory_cap,
                time_cap=time_cap,
                reduction_reason=reduction_reason,
                estimated_result_bytes=estimate_result_bytes(scenario, min(request.target_samples, resource_cap)),
            )
        )

    allocations = [min(request.min_samples, max(1, min(plan.memory_cap, plan.time_cap))) for plan in plans]
    total_allocated = sum(allocations)
    if request.total_sample_budget is not None:
        total_capacity = request.total_sample_budget
        used = total_allocated
    else:
        total_capacity = sum(min(request.target_samples, plan.memory_cap, plan.time_cap) for plan in plans)
        used = total_allocated
    decisions.append(
        BudgetDecision(
            sequence=sequence,
            action="INITIALIZE_MINIMUM_COVERAGE",
            reason="GUARANTEE_ONE_OR_MORE_PER_COMBO",
            combo_count=len(plans),
            budget_remaining=max(0, total_capacity - used),
            detail={"allocated_samples": total_allocated},
        )
    )
    sequence += 1
    round_index = 0
    while True:
        added = 0
        round_index += 1
        for index, plan in enumerate(plans):
            hard_cap = max(1, min(plan.memory_cap, plan.time_cap))
            if allocations[index] >= min(request.target_samples, hard_cap):
                continue
            if request.total_sample_budget is not None and used >= total_capacity:
                continue
            allocations[index] += 1
            used += 1
            added += 1
        if added == 0:
            break
        decisions.append(
            BudgetDecision(
                sequence=sequence,
                action="EXPAND_SAMPLING",
                reason="CONFIDENCE_TARGET_NOT_MET",
                combo_count=len(plans),
                budget_remaining=max(0, total_capacity - used),
                detail={"samples_added": added, "allocation_round": round_index},
            )
        )
        sequence += 1

    for index, plan in enumerate(plans):
        hard_cap = max(1, min(plan.memory_cap, plan.time_cap))
        allocation = allocations[index]
        reason = ""
        if allocation < request.target_samples:
            reasons: list[str] = []
            if hard_cap < request.target_samples:
                reasons.append(plan.reduction_reason or "RESOURCE_CAP")
            if request.total_sample_budget is not None and used >= total_capacity:
                reasons.append("TOTAL_SAMPLE_BUDGET")
            if not reasons:
                reasons.append("CONFIDENCE_TARGET_NOT_MET")
            reason = "+".join(dict.fromkeys(reasons))
        if reason:
            decisions.append(
                BudgetDecision(
                    sequence=sequence,
                    action="REDUCE_MATCHES",
                    reason=reason,
                    combo_count=1,
                    budget_remaining=max(0, total_capacity - used),
                    detail={
                        "candidate_id": plan.candidate_id,
                        "scenario_id": plan.scenario_id,
                        "requested_samples": request.target_samples,
                        "actual_samples": allocation,
                        "memory_cap": plan.memory_cap,
                        "time_cap": plan.time_cap,
                    },
                )
            )
            sequence += 1
        plans[index] = ComboPlan(
            order=plan.order,
            candidate_id=plan.candidate_id,
            scenario_id=plan.scenario_id,
            requested_samples=plan.requested_samples,
            samples=allocation,
            memory_cap=plan.memory_cap,
            time_cap=plan.time_cap,
            reduction_reason=reason,
            estimated_result_bytes=estimate_result_bytes(plan_scenario(project, plan.scenario_id), allocation),
        )

    schedule_decision = BudgetDecision(
        sequence=sequence,
        action="SCHEDULE_QUEUE",
        reason="RESOURCE_SLOTS_DO_NOT_DROP_COMBINATIONS",
        combo_count=len(plans),
        budget_remaining=max(0, total_capacity - used),
        detail={
            "parallel_slots": budget.parallel_slots,
            "max_matches_per_round": budget.max_matches_per_round,
            "per_thread_buffer_kb": per_thread_buffer_kb,
            "queued_until_slot_available": max(0, len(plans) - budget.parallel_slots),
        },
    )
    decisions.append(schedule_decision)
    return BudgetPlan(
        target_samples=request.target_samples,
        min_samples=request.min_samples,
        total_sample_budget=request.total_sample_budget,
        combos=tuple(plans),
        decisions=tuple(decisions),
        per_thread_buffer_kb=per_thread_buffer_kb,
    )


def plan_scenario(project: Project, scenario_id: str) -> Scenario:
    for scenario in project.scenarios:
        if scenario.id == scenario_id:
            return scenario
    raise BalanceError(ErrorCode.REF, f"missing scenario {scenario_id!r}", scenario_id, "scenario_id")


def decision_hash(plan: BudgetPlan) -> str:
    return content_hash([decision.to_dict() for decision in plan.decisions])


def replay_decision_sequence(
    project: Project,
    candidates: tuple[Candidate, ...] | list[Candidate],
    request: SimulationRequest,
) -> tuple[BudgetDecision, ...]:
    """Offline replay entry point; it must be a pure function of the same inputs."""

    return build_budget_plan(project, candidates, request).decisions
