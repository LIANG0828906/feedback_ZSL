from __future__ import annotations

from dataclasses import dataclass
from enum import StrEnum
from threading import RLock
from typing import Any

from .errors import BalanceError, ErrorCode
from .models import ComboResult


class CandidateState(StrEnum):
    PENDING = "PENDING"
    EVALUATING = "EVALUATING"
    SUBMITTED = "SUBMITTED"
    FAILED = "FAILED"


class ComboState(StrEnum):
    PENDING = "PENDING"
    SIMULATING = "SIMULATING"
    SUBMITTED = "SUBMITTED"
    FAILED = "FAILED"


CANDIDATE_TRANSITIONS: dict[CandidateState, frozenset[CandidateState]] = {
    CandidateState.PENDING: frozenset({CandidateState.EVALUATING}),
    CandidateState.EVALUATING: frozenset({CandidateState.SUBMITTED, CandidateState.FAILED}),
    CandidateState.SUBMITTED: frozenset(),
    CandidateState.FAILED: frozenset(),
}

COMBO_TRANSITIONS: dict[ComboState, frozenset[ComboState]] = {
    ComboState.PENDING: frozenset({ComboState.SIMULATING}),
    ComboState.SIMULATING: frozenset({ComboState.SUBMITTED, ComboState.FAILED}),
    ComboState.SUBMITTED: frozenset(),
    ComboState.FAILED: frozenset(),
}


@dataclass(frozen=True)
class StateSnapshot:
    candidates: dict[str, str]
    combos: dict[str, str]
    submitted_candidates: int
    submitted_combos: int
    frozen_combo_hashes: dict[str, str]

    def to_dict(self) -> dict[str, Any]:
        return {
            "candidates": dict(sorted(self.candidates.items())),
            "combos": dict(sorted(self.combos.items())),
            "submitted_candidates": self.submitted_candidates,
            "submitted_combos": self.submitted_combos,
            "frozen_combo_hashes": dict(sorted(self.frozen_combo_hashes.items())),
        }


class StateMachine:
    """Two-level candidate/combo state machine with immutable submissions."""

    def __init__(self) -> None:
        self._lock = RLock()
        self._candidate_state: dict[str, CandidateState] = {}
        self._combo_state: dict[str, ComboState] = {}
        self._combo_results: dict[str, ComboResult] = {}

    @staticmethod
    def combo_key(candidate_id: str, scenario_id: str) -> str:
        return f"{candidate_id}::{scenario_id}"

    def register(self, candidate_ids: list[str], combo_keys: list[str]) -> None:
        with self._lock:
            for candidate_id in candidate_ids:
                self._candidate_state.setdefault(candidate_id, CandidateState.PENDING)
            for key in combo_keys:
                self._combo_state.setdefault(key, ComboState.PENDING)

    def transition_candidate(self, candidate_id: str, target: CandidateState) -> None:
        with self._lock:
            current = self._candidate_state.get(candidate_id)
            if current is None:
                raise BalanceError(ErrorCode.STATE, "unknown candidate", candidate_id, "state")
            if target not in CANDIDATE_TRANSITIONS[current]:
                raise BalanceError(
                    ErrorCode.STATE,
                    f"illegal candidate transition {current.value} -> {target.value}",
                    candidate_id,
                    "state",
                    detail={"current": current.value, "target": target.value},
                )
            self._candidate_state[candidate_id] = target

    def transition_combo(self, key: str, target: ComboState) -> None:
        with self._lock:
            current = self._combo_state.get(key)
            if current is None:
                raise BalanceError(ErrorCode.STATE, "unknown combo", key, "state")
            if target not in COMBO_TRANSITIONS[current]:
                raise BalanceError(
                    ErrorCode.STATE,
                    f"illegal combo transition {current.value} -> {target.value}",
                    key,
                    "state",
                    detail={"current": current.value, "target": target.value},
                )
            self._combo_state[key] = target

    def submit_combo(self, result: ComboResult) -> None:
        key = self.combo_key(result.candidate_id, result.scenario_id)
        with self._lock:
            self.transition_combo(key, ComboState.SUBMITTED)
            self._combo_results[key] = result

    def fail_combo(self, key: str) -> None:
        with self._lock:
            current = self._combo_state.get(key)
            if current == ComboState.FAILED:
                return
            self.transition_combo(key, ComboState.FAILED)

    def submit_candidate(self, candidate_id: str) -> None:
        with self._lock:
            self.transition_candidate(candidate_id, CandidateState.SUBMITTED)

    def fail_candidate(self, candidate_id: str) -> None:
        with self._lock:
            current = self._candidate_state.get(candidate_id)
            if current == CandidateState.FAILED:
                return
            self.transition_candidate(candidate_id, CandidateState.FAILED)

    def restore_submitted_combo(self, result: ComboResult) -> None:
        key = self.combo_key(result.candidate_id, result.scenario_id)
        with self._lock:
            if self._combo_state.get(key) == ComboState.SUBMITTED:
                if self._combo_results[key].content_hash() != result.content_hash():
                    raise BalanceError(ErrorCode.STATE, "cannot overwrite frozen combo", key, "result")
                return
            self.transition_combo(key, ComboState.SIMULATING)
            self.transition_combo(key, ComboState.SUBMITTED)
            self._combo_results[key] = result

    def ensure_evaluating(self, candidate_id: str) -> None:
        with self._lock:
            if self._candidate_state[candidate_id] == CandidateState.PENDING:
                self.transition_candidate(candidate_id, CandidateState.EVALUATING)

    def restore_submitted_candidate(self, candidate_id: str) -> None:
        with self._lock:
            self.ensure_evaluating(candidate_id)
            self.transition_candidate(candidate_id, CandidateState.SUBMITTED)

    def get_combo_result(self, key: str) -> ComboResult | None:
        with self._lock:
            return self._combo_results.get(key)

    def candidate_state(self, candidate_id: str) -> CandidateState:
        with self._lock:
            return self._candidate_state[candidate_id]

    def combo_state(self, key: str) -> ComboState:
        with self._lock:
            return self._combo_state[key]

    @property
    def submitted_candidate_count(self) -> int:
        with self._lock:
            return sum(1 for state in self._candidate_state.values() if state == CandidateState.SUBMITTED)

    @property
    def submitted_combo_count(self) -> int:
        with self._lock:
            return sum(1 for state in self._combo_state.values() if state == ComboState.SUBMITTED)

    def assert_combo_mutable(self, candidate_id: str, scenario_id: str) -> None:
        key = self.combo_key(candidate_id, scenario_id)
        with self._lock:
            if self._combo_state.get(key) == ComboState.SUBMITTED:
                raise BalanceError(
                    ErrorCode.STATE,
                    "submitted combo is frozen; adjustments may only target unsubmitted combos",
                    key,
                    "result",
                )

    def freeze_snapshot(self) -> StateSnapshot:
        with self._lock:
            return StateSnapshot(
                candidates={key: value.value for key, value in self._candidate_state.items()},
                combos={key: value.value for key, value in self._combo_state.items()},
                submitted_candidates=self.submitted_candidate_count,
                submitted_combos=self.submitted_combo_count,
                frozen_combo_hashes={
                    key: result.content_hash() for key, result in self._combo_results.items()
                },
            )
