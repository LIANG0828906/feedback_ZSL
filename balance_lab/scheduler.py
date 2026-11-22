from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from .budget import SimulationRequest, build_budget_plan
from .checkpoint import ALGORITHM_VERSION, CheckpointStore, SnapshotContext, SnapshotLoad
from .combat import CandidateRuntime, simulate_combo
from .errors import BalanceError, ErrorCode
from .models import (
    BudgetPlan,
    Candidate,
    CandidateSet,
    CandidateSummary,
    ComboPlan,
    ComboResult,
    Project,
    RunResult,
    ScheduleStats,
)
from .state import CandidateState, ComboState, StateMachine
from .util import content_hash, rate_bp
from .validation import validate_candidates


@dataclass(frozen=True)
class RunProgress:
    phase: str
    candidate_id: str
    scenario_id: str
    submitted_candidates: int
    total_candidates: int
    submitted_combos: int
    total_combos: int
    restored_combos: int


ProgressCallback = Callable[[RunProgress], None]


class _SlotTracker:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self.active = 0
        self.peak = 0

    def enter(self) -> None:
        with self._lock:
            self.active += 1
            self.peak = max(self.peak, self.active)

    def leave(self) -> None:
        with self._lock:
            self.active -= 1


def _empty_snapshot() -> SnapshotLoad:
    return SnapshotLoad(valid=True, invalidated=False, reason="not_requested")


def _snapshot_is_consistent(
    load: SnapshotLoad,
    candidate_set: CandidateSet,
    plan: BudgetPlan,
) -> bool:
    expected_combos = {combo.key for combo in plan.combos}
    actual_combos = {f"{item.candidate_id}::{item.scenario_id}" for item in load.combo_results}
    if actual_combos - expected_combos:
        return False
    candidate_ids = {candidate.id for candidate in candidate_set.candidates}
    if load.candidate_commits - candidate_ids:
        return False
    by_candidate: dict[str, set[str]] = {candidate_id: set() for candidate_id in candidate_ids}
    for result in load.combo_results:
        by_candidate.setdefault(result.candidate_id, set()).add(result.scenario_id)
    for candidate_id in load.candidate_commits:
        expected = {
            combo.scenario_id for combo in plan.combos if combo.candidate_id == candidate_id
        }
        if not expected.issubset(by_candidate.get(candidate_id, set())):
            return False
    return True


def _candidate_summary(
    candidate: Candidate,
    state: StateMachine,
    results: list[ComboResult],
    total_combos: int,
) -> CandidateSummary:
    samples = sum(result.samples for result in results)
    wins = sum(result.wins_a for result in results)
    damage_min = (
        sum(result.damage_min_total for result in results) // len(results) if results else 0
    )
    damage_max = (
        sum(result.damage_max_total for result in results) // len(results) if results else 0
    )
    damage_mean = (
        sum(result.damage_mean_total for result in results) // len(results) if results else 0
    )
    state_value = state.candidate_state(candidate.id)
    return CandidateSummary(
        candidate_id=candidate.id,
        name=candidate.name,
        state=state_value.value,
        submitted_combos=sum(
            1
            for combo in results
            if combo.candidate_id == candidate.id
        ),
        total_combos=total_combos,
        win_rate_bp=rate_bp(wins, samples),
        damage_min_total=damage_min,
        damage_max_total=damage_max,
        damage_mean_total=damage_mean,
        samples=samples,
    )


def run_validation(
    project: Project,
    candidate_set: CandidateSet,
    request: SimulationRequest,
    *,
    checkpoint_path: str | Path | None = None,
    mode: str = "realtime",
    progress: ProgressCallback | None = None,
    pause_event: threading.Event | None = None,
    parallel_slots_override: int | None = None,
) -> RunResult:
    if mode not in {"realtime", "resume", "full"}:
        raise BalanceError(ErrorCode.PARA, f"unknown run mode {mode!r}", "request", "mode")
    if candidate_set.project_id != project.project_id:
        raise BalanceError(
            ErrorCode.REF,
            "candidate set does not belong to this project",
            candidate_set.project_id,
            "project_id",
        )
    validate_candidates(project, candidate_set.candidates)
    if parallel_slots_override is not None:
        if (
            isinstance(parallel_slots_override, bool)
            or not isinstance(parallel_slots_override, int)
            or not 1 <= parallel_slots_override <= project.validation_budget.parallel_slots
        ):
            raise BalanceError(
                ErrorCode.PARA,
                f"parallel_slots_override must be in [1, {project.validation_budget.parallel_slots}]",
                "request",
                "parallel_slots_override",
            )
    execution_slots = parallel_slots_override or project.validation_budget.parallel_slots
    plan = build_budget_plan(project, candidate_set.candidates, request)
    context = SnapshotContext.create(project, candidate_set, request)
    load = _empty_snapshot()
    store: CheckpointStore | None = None
    if mode != "full":
        if checkpoint_path is None:
            if mode == "resume":
                raise BalanceError(ErrorCode.PARA, "resume mode requires checkpoint_path", "request", "checkpoint_path")
        else:
            store = CheckpointStore(checkpoint_path)
            load = store.load(context)
            if load.valid and not _snapshot_is_consistent(load, candidate_set, plan):
                store.clear()
                load = SnapshotLoad(
                    valid=False,
                    invalidated=True,
                    reason="inconsistent candidate/combo commit ordering",
                )

    state = StateMachine()
    combo_keys = [combo.key for combo in plan.combos]
    state.register([candidate.id for candidate in candidate_set.candidates], combo_keys)
    restored_keys: set[str] = set()
    for result in load.combo_results:
        state.restore_submitted_combo(result)
        restored_keys.add(f"{result.candidate_id}::{result.scenario_id}")
    for candidate_id in load.candidate_commits:
        state.restore_submitted_candidate(candidate_id)
    for candidate in candidate_set.candidates:
        if state.candidate_state(candidate.id) != CandidateState.SUBMITTED:
            state.ensure_evaluating(candidate.id)

    result_by_key: dict[str, ComboResult] = {
        f"{result.candidate_id}::{result.scenario_id}": result for result in load.combo_results
    }
    pending = [combo for combo in plan.combos if combo.key not in restored_keys]
    committed_candidates = set(load.candidate_commits)
    queue = list(pending)
    initial_queue_depth = len(queue)
    queued_peak = initial_queue_depth
    dispatch_rounds = 0
    blocked_waves = 0
    interrupted = False
    simulated_combos = 0
    slot_tracker = _SlotTracker()
    candidate_by_id = candidate_set.by_id()

    def notify(phase: str, combo: ComboPlan) -> None:
        if progress is None:
            return
        progress(
            RunProgress(
                phase=phase,
                candidate_id=combo.candidate_id,
                scenario_id=combo.scenario_id,
                submitted_candidates=state.submitted_candidate_count,
                total_candidates=len(candidate_set.candidates),
                submitted_combos=state.submitted_combo_count,
                total_combos=len(plan.combos),
                restored_combos=len(restored_keys),
            )
        )

    def maybe_commit_candidate(candidate_id: str) -> None:
        if candidate_id in committed_candidates:
            return
        expected = [combo for combo in plan.combos if combo.candidate_id == candidate_id]
        if not all(state.combo_state(combo.key) == ComboState.SUBMITTED for combo in expected):
            return
        candidate_results = [
            result_by_key[combo.key] for combo in expected if combo.key in result_by_key
        ]
        summary = _candidate_summary(
            candidate_by_id[candidate_id], state, candidate_results, len(expected)
        )
        if store is not None:
            store.commit_candidate(
                context,
                candidate_id,
                {
                    "name": summary.name,
                    "state": CandidateState.SUBMITTED.value,
                    "submitted_combos": len(expected),
                    "total_combos": len(expected),
                    "win_rate_bp": summary.win_rate_bp,
                    "samples": summary.samples,
                },
            )
        state.submit_candidate(candidate_id)
        committed_candidates.add(candidate_id)

    try:
        while queue:
            if pause_event is not None and pause_event.is_set():
                interrupted = True
                break
            if request.interrupt_after_commits is not None and len(restored_keys) + len(
                result_by_key
            ) - len(restored_keys) >= request.interrupt_after_commits:
                interrupted = True
                break
            wave = queue[: project.validation_budget.max_matches_per_round]
            queue = queue[len(wave) :]
            dispatch_rounds += 1
            if queue:
                blocked_waves += 1
            queued_peak = max(queued_peak, len(queue))
            for combo in wave:
                state.ensure_evaluating(combo.candidate_id)
                state.transition_combo(combo.key, ComboState.SIMULATING)
                notify("STARTED", combo)

            def execute(combo: ComboPlan) -> ComboResult:
                slot_tracker.enter()
                try:
                    runtime = CandidateRuntime.from_candidate(
                        project, candidate_by_id[combo.candidate_id]
                    )
                    scenario = next(
                        item for item in project.scenarios if item.id == combo.scenario_id
                    )
                    return simulate_combo(
                        project,
                        runtime,
                        scenario,
                        combo,
                        per_thread_buffer_kb=plan.per_thread_buffer_kb,
                        max_actual_seconds=float(project.validation_budget.max_scenario_seconds),
                    )
                finally:
                    slot_tracker.leave()

            with ThreadPoolExecutor(
                max_workers=execution_slots,
                thread_name_prefix="balance-sim",
            ) as executor:
                futures = [executor.submit(execute, combo) for combo in wave]
                for combo, future in zip(wave, futures):
                    try:
                        result = future.result()
                    except BalanceError as exc:
                        state.fail_combo(combo.key)
                        state.fail_candidate(combo.candidate_id)
                        raise exc
                    except Exception as exc:
                        state.fail_combo(combo.key)
                        state.fail_candidate(combo.candidate_id)
                        raise BalanceError(
                            ErrorCode.STATE,
                            f"simulation task failed: {exc}",
                            combo.candidate_id,
                            "simulation",
                        ) from exc
                    if store is not None:
                        store.commit_combo(context, result)
                    state.submit_combo(result)
                    result_by_key[combo.key] = result
                    simulated_combos += 1
                    notify("COMMITTED", combo)
                    maybe_commit_candidate(combo.candidate_id)

            if request.interrupt_after_commits is not None and len(result_by_key) >= request.interrupt_after_commits:
                interrupted = state.submitted_combo_count < len(plan.combos)
                break
            if pause_event is not None and pause_event.is_set():
                interrupted = state.submitted_combo_count < len(plan.combos)
                break

        # Candidates whose combos were fully restored need their candidate
        # commit finalized without re-running any simulation.
        for candidate in candidate_set.candidates:
            if state.submitted_combo_count == len(plan.combos):
                maybe_commit_candidate(candidate.id)
            elif state.candidate_state(candidate.id) != CandidateState.SUBMITTED:
                expected = [combo for combo in plan.combos if combo.candidate_id == candidate.id]
                if expected and all(
                    state.combo_state(combo.key) == ComboState.SUBMITTED for combo in expected
                ):
                    maybe_commit_candidate(candidate.id)
    finally:
        if store is not None:
            store.close()

    ordered_results = [
        result_by_key[combo.key] for combo in plan.combos if combo.key in result_by_key
    ]
    summaries: list[CandidateSummary] = []
    for candidate in candidate_set.candidates:
        results = [item for item in ordered_results if item.candidate_id == candidate.id]
        expected_count = sum(1 for combo in plan.combos if combo.candidate_id == candidate.id)
        summary = _candidate_summary(candidate, state, results, expected_count)
        summaries.append(summary)
    schedule = ScheduleStats(
        configured_parallel_slots=project.validation_budget.parallel_slots,
        actual_peak_slots=slot_tracker.peak,
        dispatch_rounds=dispatch_rounds,
        total_combos=len(plan.combos),
        committed_combos=state.submitted_combo_count,
        queued_peak=queued_peak,
        blocked_waves=blocked_waves,
        restored_combos=len(restored_keys),
        simulated_combos=simulated_combos,
        per_thread_buffer_kb=plan.per_thread_buffer_kb,
    )
    fingerprint = (
        f"{context.fingerprint()}:{state.submitted_combo_count}:{state.submitted_candidate_count}"
    )
    result = RunResult(
        project_id=project.project_id,
        mode=mode,
        candidates=summaries,
        combos=ordered_results,
        plan=plan,
        schedule=schedule,
        state_snapshot={
            **state.freeze_snapshot().to_dict(),
            "checkpoint_load": load.to_dict(),
            "algorithm_version": ALGORITHM_VERSION,
            "snapshot_context": context.to_dict(),
        },
        checkpoint_fingerprint=fingerprint,
        interrupted=interrupted,
    )
    return result


def candidate_set_from_candidates(project: Project, candidates: list[Candidate]) -> CandidateSet:
    return CandidateSet(project_id=project.project_id, candidates=tuple(candidates))


def result_signature(result: RunResult) -> str:
    return content_hash(
        {
            "mode": "ignored",
            "project_id": result.project_id,
            "combos": [combo.to_dict() for combo in result.combos],
            "plan": result.plan.to_dict(),
        }
    )
