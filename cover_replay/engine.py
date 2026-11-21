"""Deterministic per-physics-step replay engine."""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

from .config import ReplayConfig
from .errors import (
    BaselineVersionError,
    BusinessError,
    CrackUnstableError,
    CreditConflictError,
    RecoveryInvalidError,
)
from .model import CoverSpec, ReplayRecord, ShotSpec
from .physics import (
    CoverState,
    apply_damage,
    direct_damage,
    neighbors,
    spread_damage,
)
from .scheduler import DeterministicBlockScheduler, ScheduleOutcome
from .snapshot import SnapshotStore
from .util import (
    as_decimal,
    canonical_json,
    from_units,
    load_json,
    sha256_obj,
    to_units,
)
from .version import RESULT_SCHEMA_VERSION


@dataclass(frozen=True, slots=True)
class Withdrawal:
    step: int
    block_id: int
    amount: str
    reason: str = "manual"

    def to_dict(self) -> dict[str, Any]:
        return {
            "step": self.step,
            "block_id": self.block_id,
            "amount": self.amount,
            "reason": self.reason,
        }


@dataclass(frozen=True, slots=True)
class PrimaryImpact:
    shot: ShotSpec
    calculated_damage: int


@dataclass(frozen=True, slots=True)
class ReplayRun:
    match_id: str
    model_version: str
    baseline_version: str
    strategy: str
    physics_steps: int
    fractional_digits: int
    frames: tuple[dict[str, Any], ...]
    final_state_hash: str | None
    complete: bool
    interrupted_step: int | None
    conflicts: tuple[CreditConflictError, ...] = field(default_factory=tuple)
    latest_snapshot_step: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": RESULT_SCHEMA_VERSION,
            "match_id": self.match_id,
            "model_version": self.model_version,
            "baseline_version": self.baseline_version,
            "strategy": self.strategy,
            "physics_steps": self.physics_steps,
            "fractional_digits": self.fractional_digits,
            "complete": self.complete,
            "interrupted_step": self.interrupted_step,
            "final_state_hash": self.final_state_hash,
            "conflicts": [conflict.to_dict() for conflict in self.conflicts],
            "frames": list(self.frames),
        }

    def to_bytes(self) -> bytes:
        return canonical_json(self.to_dict()).encode("utf-8")


class ReplayEngine:
    def __init__(
        self,
        record: ReplayRecord,
        config: ReplayConfig,
        *,
        snapshot_dir: str | Path | None = None,
        resume: bool = False,
        withdrawals: tuple[Withdrawal, ...] = (),
        allow_conflicts: bool = False,
    ) -> None:
        if record.model_version and record.model_version != config.model_version:
            raise BaselineVersionError(
                baseline_version=record.baseline_version or config.baseline_version,
                expected_baseline_version=config.baseline_version,
                model_version=record.model_version,
                expected_model_version=config.model_version,
            )
        if (
            record.baseline_version
            and record.baseline_version != config.baseline_version
        ):
            raise BaselineVersionError(
                baseline_version=record.baseline_version,
                expected_baseline_version=config.baseline_version,
                model_version=record.model_version or config.model_version,
                expected_model_version=config.model_version,
            )
        self.record = record
        self.config = config
        self.allow_conflicts = allow_conflicts
        self.withdrawals = tuple(
            sorted(
                withdrawals,
                key=lambda item: (item.step, item.block_id, item.reason, item.amount),
            )
        )
        self._validate_withdrawals()
        self.strategy = self._resolve_strategy()
        self.covers_by_id = record.covers_by_id
        self.covers = tuple(sorted(record.covers, key=lambda cover: cover.block_id))
        self.states: dict[int, CoverState] = {
            cover.block_id: CoverState(
                block_id=cover.block_id,
                durability_max=cover.durability_max,
            )
            for cover in self.covers
        }
        self.client_remaining: dict[int, int] = {
            cover.block_id: cover.durability_max for cover in self.covers
        }
        self.frames: list[dict[str, Any]] = []
        self.conflicts: list[CreditConflictError] = []
        self.next_step = 1
        self.scheduler = DeterministicBlockScheduler(config)
        self._withdrawals_by_step: dict[int, list[Withdrawal]] = {}
        for withdrawal in self.withdrawals:
            self._withdrawals_by_step.setdefault(withdrawal.step, []).append(withdrawal)
        self.input_digest = sha256_obj(
            {
                "record": record.raw,
                "config": config.to_dict(),
                "allow_conflicts": self.allow_conflicts,
                "withdrawals": [
                    {
                        "step": item.step,
                        "block_id": item.block_id,
                        "amount_units": to_units(
                            as_decimal(item.amount, "withdrawal.amount"),
                            config.fractional_digits,
                            "withdrawal.amount",
                        ),
                        "reason": item.reason,
                    }
                    for item in self.withdrawals
                ],
            }
        )
        self.snapshots = SnapshotStore(
            snapshot_dir,
            input_digest=self.input_digest,
            config=config,
        )
        if not resume:
            self.snapshots.ensure_fresh_target()
        if resume:
            loaded = self.snapshots.load_latest()
            if loaded is None:
                raise RecoveryInvalidError(
                    1,
                    "no committed snapshots are available for recovery",
                )
            self._restore(loaded.record)

    # ------------------------------------------------------------------ setup
    def _validate_withdrawals(self) -> None:
        for withdrawal in self.withdrawals:
            if withdrawal.step < 1 or withdrawal.step > self.config.physics_steps:
                raise BusinessError(
                    "E_RECORD_INVALID",
                    "withdrawal step is outside the replay",
                    field="withdrawal.step",
                    step=withdrawal.step,
                )
            if withdrawal.block_id not in self.record.covers_by_id:
                raise BusinessError(
                    "E_RECORD_INVALID",
                    "withdrawal references an unknown cover block",
                    field="withdrawal.block_id",
                    step=withdrawal.step,
                    block_id=withdrawal.block_id,
                )
            try:
                amount = to_units(
                    as_decimal(withdrawal.amount, "withdrawal.amount"),
                    self.config.fractional_digits,
                    "withdrawal.amount",
                )
            except (BusinessError, ValueError, TypeError):
                raise BusinessError(
                    "E_RECORD_INVALID",
                    "withdrawal amount must be a finite non-negative number",
                    field="withdrawal.amount",
                    step=withdrawal.step,
                ) from None
            if amount < 0:
                raise BusinessError(
                    "E_RECORD_INVALID",
                    "withdrawal amount must be non-negative",
                    field="withdrawal.amount",
                    step=withdrawal.step,
                )

    def _resolve_strategy(self) -> str:
        if self.config.strategy != "adaptive":
            return self.config.strategy
        cost = len(self.record.covers) * max(1, len(self.record.shots))
        return "full" if cost <= self.config.full_scan_cost_limit else "window"

    def _restore(self, snapshot: dict[str, Any]) -> None:
        state = snapshot["state"]
        self.next_step = int(state["step"]) + 1
        restored_states: dict[int, CoverState] = {}
        for raw_cover in state["covers"]:
            cover_state = CoverState.from_snapshot(raw_cover)
            if cover_state.block_id not in self.states:
                raise BusinessError(
                    "E_RECOVERY_INVALID",
                    "snapshot references an unknown cover block",
                    step=int(state["step"]),
                    block_id=cover_state.block_id,
                )
            restored_states[cover_state.block_id] = cover_state
        if set(restored_states) != set(self.states):
            raise BusinessError(
                "E_RECOVERY_INVALID",
                "snapshot cover set does not match the record",
                step=int(state["step"]),
            )
        self.states = restored_states
        self.client_remaining = {
            int(block_id): int(value)
            for block_id, value in snapshot["ledger"].items()
        }
        if set(self.client_remaining) != set(self.states):
            raise BusinessError(
                "E_RECOVERY_INVALID",
                "snapshot credit ledger cover set does not match the record",
                step=int(state["step"]),
            )
        self.frames = []
        for step in range(1, int(state["step"]) + 1):
            path = self.snapshots.path_for_step(step)
            try:
                payload = load_json(path)
            except (OSError, ValueError) as error:
                raise BusinessError(
                    "E_RECOVERY_INVALID",
                    "cannot reload committed snapshot frame",
                    step=step,
                    detail=str(error),
                ) from None
            self.frames.append(payload["frame"])
            self.scheduler.mark_submitted(step)
        self.conflicts = []
        for frame in self.frames:
            for credit in frame.get("credit_entries", []):
                if credit.get("status") != "conflict":
                    continue
                self.conflicts.append(
                    CreditConflictError(
                        step=int(frame["step"]),
                        block_id=int(credit["block_id"]),
                        shot_index=int(credit["shot_index"]),
                        predicted_remaining=str(credit["predicted_remaining"]),
                        authoritative_remaining=str(
                            credit["authoritative_remaining"]
                        ),
                        client_sequence=int(credit["client_sequence"]),
                        player_id=str(credit["player_id"]),
                    )
                )
        if self.conflicts and not self.allow_conflicts:
            raise self.conflicts[0]
        meta_strategy = snapshot.get("meta", {}).get("strategy")
        if meta_strategy and meta_strategy != self.strategy:
            raise BusinessError(
                "E_RECOVERY_INVALID",
                "snapshot settlement strategy does not match the replay",
                step=int(state["step"]),
                snapshot_strategy=meta_strategy,
                replay_strategy=self.strategy,
            )

    # --------------------------------------------------------------- planning
    def _shots_for_step(self) -> dict[int, tuple[ShotSpec, ...]]:
        if self.strategy == "full":
            ordered = sorted(
                self.record.shots,
                key=lambda shot: (
                    shot.declared_step,
                    shot.player_id,
                    shot.client_seq,
                    shot.shot_index,
                ),
            )
            grouped: dict[int, list[ShotSpec]] = {}
            for shot in ordered:
                grouped.setdefault(shot.declared_step, []).append(shot)
            return {step: tuple(shots) for step, shots in grouped.items()}
        grouped = {}
        for step in range(1, self.config.physics_steps + 1):
            shots = sorted(
                (
                    shot
                    for shot in self.record.shots
                    if shot.declared_step == step
                ),
                key=lambda shot: (shot.player_id, shot.client_seq, shot.shot_index),
            )
            if shots:
                grouped[step] = tuple(shots)
        return grouped

    # ------------------------------------------------------------------ steps
    def _cover_tasks(
        self,
        step_shots: tuple[ShotSpec, ...],
    ) -> dict[int, tuple[int, Callable[[], list[PrimaryImpact]]]]:
        by_cover: dict[int, list[ShotSpec]] = {}
        for shot in step_shots:
            by_cover.setdefault(shot.block_id, []).append(shot)
        tasks: dict[int, tuple[int, Callable[[], list[PrimaryImpact]]]] = {}
        for cover in self.covers:
            shots = tuple(by_cover.get(cover.block_id, ()))
            start_damage = self.states[cover.block_id].accumulated_damage

            def calculate(
                target_cover: CoverSpec = cover,
                target_shots: tuple[ShotSpec, ...] = shots,
                accumulated: int = start_damage,
            ) -> list[PrimaryImpact]:
                return [
                    PrimaryImpact(
                        shot=shot,
                        calculated_damage=direct_damage(
                            self.config, shot, target_cover, accumulated
                        ),
                    )
                    for shot in target_shots
                ]

            tasks[cover.block_id] = (cover.memory_units, calculate)
        return tasks

    def _precredit(
        self,
        step: int,
        step_shots: tuple[ShotSpec, ...],
        impacts: dict[int, int],
    ) -> dict[int, dict[str, Any]]:
        pending: dict[int, dict[str, Any]] = {}
        for shot in sorted(step_shots, key=lambda item: (item.arrival_seq, item.shot_index)):
            calculated = impacts[shot.shot_index]
            is_explicit = shot.predicted_damage is not None
            predicted = (
                shot.predicted_damage if is_explicit else calculated
            )
            assert predicted is not None
            before = self.client_remaining[shot.block_id]
            after = max(0, before - predicted)
            self.client_remaining[shot.block_id] = after
            pending[shot.shot_index] = {
                "before": before,
                "predicted": predicted,
                "after_precredit": after,
                "explicit": is_explicit,
            }
        return pending

    def _damage_events(
        self, step_shots: tuple[ShotSpec, ...], impacts: dict[int, int]
    ) -> list[dict[str, Any]]:
        ordered_shots = sorted(
            step_shots,
            key=lambda shot: (shot.player_id, shot.client_seq, shot.shot_index),
        )
        events: list[dict[str, Any]] = []
        for shot in ordered_shots:
            primary = impacts[shot.shot_index]
            if primary > 0:
                events.append(
                    {
                        "shot_index": shot.shot_index,
                        "kind": "primary",
                        "source_block_id": shot.block_id,
                        "target_block_id": shot.block_id,
                        "amount": primary,
                    }
                )
            source = self.covers_by_id[shot.block_id]
            secondary = spread_damage(self.config, primary)
            if secondary > 0:
                for target in sorted(
                    neighbors(source, self.covers), key=lambda cover: cover.block_id
                ):
                    events.append(
                        {
                            "shot_index": shot.shot_index,
                            "kind": "spread",
                            "source_block_id": shot.block_id,
                            "target_block_id": target.block_id,
                            "amount": secondary,
                        }
                    )
        return events

    def _apply_withdrawals(self, step: int) -> list[dict[str, Any]]:
        entries: list[dict[str, Any]] = []
        for withdrawal in sorted(
            self._withdrawals_by_step.get(step, []),
            key=lambda item: (item.block_id, item.reason, item.amount),
        ):
            amount = to_units(
                as_decimal(withdrawal.amount, "withdrawal.amount"),
                self.config.fractional_digits,
                "withdrawal.amount",
            )
            state = self.states[withdrawal.block_id]
            applied = min(amount, state.accumulated_damage)
            state.accumulated_damage -= applied
            if applied and state.cracked:
                state.cracked = False
                state.cracked_step = None
            self.client_remaining[withdrawal.block_id] = min(
                self.states[withdrawal.block_id].durability_max,
                self.client_remaining[withdrawal.block_id] + applied,
            )
            entries.append(
                {
                    "kind": "withdrawal",
                    "step": step,
                    "block_id": withdrawal.block_id,
                    "requested": from_units(
                        amount, self.config.fractional_digits
                    ),
                    "refunded": from_units(
                        applied, self.config.fractional_digits
                    ),
                    "reason": withdrawal.reason,
                }
            )
        return entries

    def _reconcile(
        self,
        step: int,
        step_shots: tuple[ShotSpec, ...],
        pending: dict[int, dict[str, Any]],
        primary_applied: dict[int, int],
    ) -> list[dict[str, Any]]:
        records: list[dict[str, Any]] = []
        for shot in sorted(step_shots, key=lambda item: (item.arrival_seq, item.shot_index)):
            credit = pending[shot.shot_index]
            actual = primary_applied.get(shot.shot_index, 0)
            predicted = int(credit["predicted"])
            if not credit["explicit"]:
                self.client_remaining[shot.block_id] = max(
                    0,
                    self.client_remaining[shot.block_id] + predicted - actual,
                )
                status = "effective" if actual > 0 else "refunded"
                if actual == 0:
                    self.client_remaining[shot.block_id] = min(
                        self.states[shot.block_id].durability_max,
                        self.client_remaining[shot.block_id] + predicted,
                    )
            elif actual == 0:
                status = "refunded"
                self.client_remaining[shot.block_id] = min(
                    self.states[shot.block_id].durability_max,
                    self.client_remaining[shot.block_id] + predicted,
                )
            elif actual == predicted:
                status = "effective"
            else:
                status = "conflict"
                conflict = CreditConflictError(
                    step=step,
                    block_id=shot.block_id,
                    shot_index=shot.shot_index,
                    predicted_remaining=from_units(
                        self.client_remaining[shot.block_id],
                        self.config.fractional_digits,
                    ),
                    authoritative_remaining=from_units(
                        self.states[shot.block_id].remaining,
                        self.config.fractional_digits,
                    ),
                    client_sequence=shot.client_seq,
                    player_id=shot.player_id,
                )
                self.conflicts.append(conflict)
            records.append(
                {
                    "shot_index": shot.shot_index,
                    "player_id": shot.player_id,
                    "client_sequence": shot.client_seq,
                    "block_id": shot.block_id,
                    "predicted_damage": from_units(
                        predicted, self.config.fractional_digits
                    ),
                    "authoritative_damage": from_units(
                        actual, self.config.fractional_digits
                    ),
                    "status": status,
                    "predicted_remaining": from_units(
                        self.client_remaining[shot.block_id],
                        self.config.fractional_digits,
                    ),
                    "authoritative_remaining": from_units(
                        self.states[shot.block_id].remaining,
                        self.config.fractional_digits,
                    ),
                }
            )
        if self.conflicts and not self.allow_conflicts:
            raise self.conflicts[0]
        return records

    def _mark_initial_cracks(self, step: int) -> list[dict[str, Any]]:
        events: list[dict[str, Any]] = []
        for cover in self.covers:
            state = self.states[cover.block_id]
            threshold = cover.segment_threshold * self.config.crack_threshold_segments
            if not state.cracked and state.accumulated_damage >= threshold:
                state.cracked = True
                state.cracked_step = step
                events.append(
                    {
                        "step": step,
                        "block_id": cover.block_id,
                        "cause": "threshold",
                        "damage": from_units(
                            state.accumulated_damage, self.config.fractional_digits
                        ),
                        "threshold": from_units(
                            threshold, self.config.fractional_digits
                        ),
                    }
                )
        return events

    def _propagate_cracks(
        self,
        step: int,
        initial_events: list[dict[str, Any]],
    ) -> tuple[list[dict[str, Any]], list[dict[str, int]]]:
        if not initial_events:
            return [], []
        shock = self.config.crack_propagation_units()
        if shock <= 0:
            return [], []
        current = {
            int(event["block_id"]): [int(event["block_id"])]
            for event in initial_events
        }
        events: list[dict[str, Any]] = []
        chain: list[dict[str, int]] = []
        iterations = 0
        while current:
            iterations += 1
            if iterations > self.config.max_crack_iterations:
                involved = sorted(
                    {
                        *(
                            block
                            for path in current.values()
                            for block in path
                        ),
                        *(event["block_id"] for event in initial_events),
                        *(edge["from_block_id"] for edge in chain),
                        *(edge["to_block_id"] for edge in chain),
                    }
                )
                raise CrackUnstableError(
                    step=step,
                    block_ids=involved,
                    propagation_chain=chain,
                    iterations=iterations - 1,
                )
            next_cracks: dict[int, list[int]] = {}
            for source_id in sorted(current):
                source = self.covers_by_id[source_id]
                path = current[source_id]
                for target in sorted(
                    neighbors(source, self.covers), key=lambda cover: cover.block_id
                ):
                    chain.append(
                        {"from_block_id": source_id, "to_block_id": target.block_id}
                    )
                    target_state = self.states[target.block_id]
                    effective = apply_damage(target_state, shock)
                    if effective <= 0:
                        continue
                    threshold = (
                        target.segment_threshold
                        * self.config.crack_threshold_segments
                    )
                    if (
                        not target_state.cracked
                        and target_state.accumulated_damage >= threshold
                    ):
                        target_state.cracked = True
                        target_state.cracked_step = step
                        next_path = path + [target.block_id]
                        next_cracks.setdefault(target.block_id, next_path)
                        events.append(
                            {
                                "step": step,
                                "block_id": target.block_id,
                                "cause": "crack_shock",
                                "source_block_id": source_id,
                                "damage": from_units(
                                    target_state.accumulated_damage,
                                    self.config.fractional_digits,
                                ),
                                "threshold": from_units(
                                    threshold, self.config.fractional_digits
                                ),
                            }
                        )
            current = next_cracks
        return events, chain

    def _state_payload(self, step: int) -> dict[str, Any]:
        return {
            "step": step,
            "covers": [
                {
                    "block_id": cover.block_id,
                    "accumulated_damage": from_units(
                        self.states[cover.block_id].accumulated_damage,
                        self.config.fractional_digits,
                    ),
                    "remaining": from_units(
                        self.states[cover.block_id].remaining,
                        self.config.fractional_digits,
                    ),
                    "cracked": self.states[cover.block_id].cracked,
                    "cracked_step": self.states[cover.block_id].cracked_step,
                }
                for cover in self.covers
            ],
            "credits": [
                {
                    "block_id": cover.block_id,
                    "predicted_remaining": from_units(
                        self.client_remaining[cover.block_id],
                        self.config.fractional_digits,
                    ),
                }
                for cover in self.covers
            ],
        }

    def _engine_state_payload(self, step: int) -> dict[str, Any]:
        return {
            "step": step,
            "covers": [
                self.states[cover.block_id].to_snapshot() for cover in self.covers
            ],
        }

    def advance(
        self,
        *,
        expected_step: int | None = None,
    ) -> dict[str, Any]:
        step = self.next_step if expected_step is None else expected_step
        if step <= self.next_step - 1:
            from .errors import BusyError

            raise BusyError(step)
        if step != self.next_step:
            raise BusinessError(
                "E_STEP_ORDER",
                "physical steps must be settled in order",
                expected_step=self.next_step,
                actual_step=step,
            )
        if step > self.config.physics_steps:
            raise BusinessError(
                "E_STEP_ORDER",
                "physical step exceeds configured replay length",
                step=step,
            )
        windows = self._shots_for_step()
        step_shots = windows.get(step, ())
        tasks = self._cover_tasks(step_shots)
        schedule: ScheduleOutcome[list[PrimaryImpact]] = self.scheduler.execute(
            step, tasks
        )
        impacts: dict[int, int] = {}
        for block_id in sorted(schedule.results):
            for impact in schedule.results[block_id]:
                impacts[impact.shot.shot_index] = impact.calculated_damage
        pending = self._precredit(step, step_shots, impacts)
        withdrawal_entries = self._apply_withdrawals(step)
        events = self._damage_events(step_shots, impacts)
        primary_applied: dict[int, int] = {}
        secondary_applied: dict[int, int] = {}
        for event in events:
            target_state = self.states[int(event["target_block_id"])]
            applied = apply_damage(target_state, int(event["amount"]))
            if event["kind"] == "primary":
                shot_index = int(event["shot_index"])
                primary_applied[shot_index] = (
                    primary_applied.get(shot_index, 0) + applied
                )
            else:
                target_id = int(event["target_block_id"])
                secondary_applied[target_id] = (
                    secondary_applied.get(target_id, 0) + applied
                )
        crack_events = self._mark_initial_cracks(step)
        propagated, propagation_chain = self._propagate_cracks(step, crack_events)
        crack_events.extend(propagated)
        credit_records = self._reconcile(
            step, step_shots, pending, primary_applied
        )
        state_payload = self._state_payload(step)
        state_hash = sha256_obj(state_payload)
        effective_shots = [
            {
                "shot_index": shot.shot_index,
                "player_id": shot.player_id,
                "client_sequence": shot.client_seq,
            }
            for shot in sorted(
                step_shots,
                key=lambda shot: (shot.player_id, shot.client_seq, shot.shot_index),
            )
            if primary_applied.get(shot.shot_index, 0) > 0
        ]
        frame = {
            "step": step,
            "state_hash": state_hash,
            "covers": state_payload["covers"],
            "credits": state_payload["credits"],
            "credit_entries": credit_records,
            "withdrawals": withdrawal_entries,
            "crack_events": crack_events,
            "crack_propagation_chain": propagation_chain,
            "active_shot_indices": [shot.shot_index for shot in step_shots],
            "effective_shots": effective_shots,
            "effective_client_sequences": [
                entry["client_sequence"] for entry in effective_shots
            ],
            "secondary_damage": [
                {
                    "block_id": block_id,
                    "amount": from_units(
                        amount, self.config.fractional_digits
                    ),
                }
                for block_id, amount in sorted(secondary_applied.items())
            ],
            "budget_usage": {
                "memory_budget": schedule.memory_budget,
                "peak_memory_used": schedule.peak_memory_units,
                "queue_watermark": schedule.queue_watermark,
                "worker_limit": schedule.max_workers,
                "wave_count": len(schedule.waves),
            },
            "scheduling": {
                "summary": schedule.summary(),
                "trace": list(schedule.trace),
            },
        }
        self.frames.append(frame)
        self.snapshots.write_step(
            step=step,
            engine_state=self._engine_state_payload(step),
            ledger=dict(self.client_remaining),
            frame=frame,
            meta={"strategy": self.strategy, "model_version": self.config.model_version},
        )
        self.next_step += 1
        return frame

    def run(
        self,
        *,
        stop_after_step: int | None = None,
        cancel_event: threading.Event | None = None,
    ) -> ReplayRun:
        while self.next_step <= self.config.physics_steps:
            if cancel_event is not None and cancel_event.is_set():
                return self._run_result(complete=False, interrupted_step=self.next_step - 1)
            self.advance()
            if stop_after_step is not None and self.next_step - 1 >= stop_after_step:
                return self._run_result(
                    complete=False, interrupted_step=self.next_step - 1
                )
        return self._run_result(complete=True, interrupted_step=None)

    def _run_result(
        self, *, complete: bool, interrupted_step: int | None
    ) -> ReplayRun:
        return ReplayRun(
            match_id=self.record.match_id,
            model_version=self.config.model_version,
            baseline_version=self.config.baseline_version,
            strategy=self.strategy,
            physics_steps=self.config.physics_steps,
            fractional_digits=self.config.fractional_digits,
            frames=tuple(self.frames),
            final_state_hash=(
                self.frames[-1]["state_hash"] if self.frames else None
            ),
            complete=complete,
            interrupted_step=interrupted_step,
            conflicts=tuple(self.conflicts),
            latest_snapshot_step=self.snapshots.last_step,
        )


def replay(
    record: ReplayRecord,
    config: ReplayConfig,
    **kwargs: Any,
) -> ReplayRun:
    stop_after_step = kwargs.pop("stop_after_step", None)
    cancel_event = kwargs.pop("cancel_event", None)
    return ReplayEngine(record, config, **kwargs).run(
        stop_after_step=stop_after_step,
        cancel_event=cancel_event,
    )
