"""Deterministic fixed-step cover settlement and reconciliation."""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from typing import Any

from .errors import SimulationError
from .hashing import (
    chain_prefix_digest,
    compute_state_hash,
    next_chain_link,
    state_payload,
)
from .model import (
    GENESIS_HASH,
    InitialState,
    RevocationCommand,
    ShotRecord,
    from_micro,
)
from .physics import compute_damage, locate_hit

APPLIED = "APPLIED"
REFUNDED = "REFUNDED"
CONFLICT = "CONFLICT"

REVOKE_ACCEPTED = "ACCEPTED"


@dataclass
class RuntimeCover:
    definition: Any
    remaining: int
    damage: int = 0
    cracked: bool = False
    alive: bool = True
    crack_events: list[dict[str, Any]] = field(default_factory=list)

    @property
    def segment(self) -> int:
        return self.damage // self.definition.segment_threshold

    @classmethod
    def initial(cls, definition: Any) -> RuntimeCover:
        return cls(
            definition=definition,
            remaining=definition.max_durability,
            damage=0,
            cracked=False,
            alive=True,
            crack_events=[],
        )

    def state_dict(self) -> dict[str, Any]:
        return {
            "cover_id": self.definition.cover_id,
            "remaining": self.remaining,
            "damage": self.damage,
            "cracked": self.cracked,
            "alive": self.alive,
            "segment": self.segment,
            "crack_events": list(self.crack_events),
        }

    @classmethod
    def from_state_dict(cls, definition: Any, data: Mapping[str, Any]) -> RuntimeCover:
        return cls(
            definition=definition,
            remaining=int(data["remaining"]),
            damage=int(data["damage"]),
            cracked=bool(data["cracked"]),
            alive=bool(data["alive"]),
            crack_events=[dict(item) for item in data.get("crack_events", [])],
        )


@dataclass
class ShotOutcome:
    record: ShotRecord
    settlement_step: int
    status: str
    predicted: dict[str, int]
    authoritative: dict[str, int]
    conflicts: list[dict[str, Any]] = field(default_factory=list)
    errors: list[dict[str, Any]] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "record": self.record.to_dict(),
            "settlement_step": self.settlement_step,
            "status": self.status,
            "predicted": {
                cover_id: from_micro(value)
                for cover_id, value in sorted(self.predicted.items())
            },
            "authoritative": {
                cover_id: from_micro(value)
                for cover_id, value in sorted(self.authoritative.items())
            },
            "conflicts": list(self.conflicts),
            "errors": list(self.errors),
        }

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> ShotOutcome:
        return cls(
            record=ShotRecord.from_dict(data["record"]),
            settlement_step=int(data["settlement_step"]),
            status=str(data["status"]),
            predicted={
                str(cover_id): round(float(value) * 1_000_000)
                for cover_id, value in dict(data.get("predicted", {})).items()
            },
            authoritative={
                str(cover_id): round(float(value) * 1_000_000)
                for cover_id, value in dict(data.get("authoritative", {})).items()
            },
            conflicts=[dict(item) for item in data.get("conflicts", [])],
            errors=[dict(item) for item in data.get("errors", [])],
        )


@dataclass
class SimulationState:
    step: int
    covers: dict[str, RuntimeCover]
    pending: dict[str, int]
    buffer: list[ShotRecord]
    seen_clients: dict[tuple[str, int], str]
    settled_shots: dict[tuple[str, int], ShotOutcome]
    revocations: list[dict[str, Any]]
    rejections: list[dict[str, Any]]
    operational_reports: list[dict[str, Any]]
    duplicate_count: int
    hash_chain: list[dict[str, Any]]

    @classmethod
    def fresh(cls, initial: InitialState) -> SimulationState:
        return cls(
            step=0,
            covers={
                cover_id: RuntimeCover.initial(initial.covers[cover_id])
                for cover_id in initial.cover_ids
            },
            pending={},
            buffer=[],
            seen_clients={},
            settled_shots={},
            revocations=[],
            rejections=[],
            operational_reports=[],
            duplicate_count=0,
            hash_chain=[],
        )

    def buffer_watermark(self) -> dict[str, Any]:
        per_player: dict[str, int] = {}
        for shot in self.buffer:
            per_player[shot.player_id] = per_player.get(shot.player_id, 0) + 1
        earliest_due = (
            min(max(shot.target_step, shot.arrival_step) for shot in self.buffer)
            if self.buffer
            else None
        )
        return {
            "total": len(self.buffer),
            "per_player": {
                player_id: per_player[player_id] for player_id in sorted(per_player)
            },
            "earliest_due_step": earliest_due,
        }

    def to_dict(self) -> dict[str, Any]:
        return {
            "step": self.step,
            "covers": [
                self.covers[cover_id].state_dict()
                for cover_id in sorted(self.covers)
            ],
            "pending": [
                [cover_id, self.pending[cover_id]]
                for cover_id in sorted(self.pending)
            ],
            "buffer": [
                shot.to_dict()
                for shot in sorted(
                    self.buffer,
                    key=lambda item: (
                        item.arrival_step,
                        item.arrival_seq,
                        item.record_id,
                    ),
                )
            ],
            "seen_clients": [
                [player_id, client_seq, record_id]
                for (player_id, client_seq), record_id in sorted(
                    self.seen_clients.items()
                )
            ],
            "settled_shots": [
                outcome.to_dict()
                for _, outcome in sorted(self.settled_shots.items())
            ],
            "revocations": list(self.revocations),
            "rejections": list(self.rejections),
            "operational_reports": list(self.operational_reports),
            "duplicate_count": self.duplicate_count,
            "hash_chain": list(self.hash_chain),
        }

    @classmethod
    def from_dict(
        cls, initial: InitialState, data: Mapping[str, Any]
    ) -> SimulationState:
        try:
            covers: dict[str, RuntimeCover] = {}
            for item in data["covers"]:
                cover_id = str(item["cover_id"])
                if cover_id not in initial.covers:
                    raise ValueError(f"unknown cover {cover_id}")
                covers[cover_id] = RuntimeCover.from_state_dict(
                    initial.covers[cover_id], item
                )
            if set(covers) != set(initial.covers):
                raise ValueError("snapshot does not contain every cover")
            seen = {
                (str(player_id), int(seq)): str(record_id)
                for player_id, seq, record_id in data["seen_clients"]
            }
            settled = {
                outcome.record.key: outcome
                for outcome in (
                    ShotOutcome.from_dict(item) for item in data["settled_shots"]
                )
            }
            state = cls(
                step=int(data["step"]),
                covers=covers,
                pending={
                    str(cover_id): int(amount)
                    for cover_id, amount in data["pending"]
                },
                buffer=[ShotRecord.from_dict(item) for item in data["buffer"]],
                seen_clients=seen,
                settled_shots=settled,
                revocations=[dict(item) for item in data["revocations"]],
                rejections=[dict(item) for item in data["rejections"]],
                operational_reports=[
                    dict(item) for item in data["operational_reports"]
                ],
                duplicate_count=int(data["duplicate_count"]),
                hash_chain=[dict(item) for item in data["hash_chain"]],
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise SimulationError(
                "E_RECOVERY_INVALID",
                "snapshot state cannot be reconstructed",
                detail=str(exc),
                step=data.get("step") if isinstance(data, Mapping) else None,
            ) from exc
        for cover in state.covers.values():
            if cover.remaining < 0 or cover.damage < 0:
                raise SimulationError(
                    "E_RECOVERY_INVALID",
                    "snapshot contains negative cover values",
                    step=state.step,
                    cover_id=cover.definition.cover_id,
                )
            if cover.remaining + cover.damage != cover.definition.max_durability:
                raise SimulationError(
                    "E_RECOVERY_INVALID",
                    "snapshot cover conservation failed",
                    step=state.step,
                    cover_id=cover.definition.cover_id,
                )
        return state


class Simulation:
    """A single deterministic replay stream for one sample."""

    def __init__(
        self,
        initial: InitialState,
        shots: Sequence[ShotRecord],
        revocations: Sequence[RevocationCommand] = (),
        *,
        state: SimulationState | None = None,
        progress_fault_step: int | None = None,
    ) -> None:
        self.initial = initial
        self.shots = list(shots)
        self.revocations = list(revocations)
        self.progress_fault_step = progress_fault_step
        self.state = state if state is not None else SimulationState.fresh(initial)
        self.last_summary: dict[str, Any] | None = None
        for shot in self.shots:
            unknown = sorted(set(shot.prediction) - set(initial.covers))
            if unknown:
                raise SimulationError(
                    "E_INPUT_INVALID",
                    "shot prediction references an unknown cover",
                    shot_id=shot.record_id,
                    covers=unknown,
                )
        self._shots_by_arrival: dict[int, list[ShotRecord]] = {}
        for shot in self.shots:
            self._shots_by_arrival.setdefault(shot.arrival_step, []).append(shot)
        self._revocations_by_arrival: dict[int, list[RevocationCommand]] = {}
        for revocation in self.revocations:
            self._revocations_by_arrival.setdefault(
                revocation.arrival_step, []
            ).append(revocation)
        self._validate_state(initial)

    def _validate_state(self, initial: InitialState) -> None:
        if self.state.step < 0:
            raise SimulationError("E_RECOVERY_INVALID", "negative snapshot step")
        if self.state.hash_chain:
            expected_previous = GENESIS_HASH
            expected_step = 1
            for link in self.state.hash_chain:
                if int(link.get("step", -1)) != expected_step:
                    raise SimulationError(
                        "E_RECOVERY_INVALID",
                        "hash chain steps are not contiguous",
                        step=expected_step,
                    )
                if str(link.get("previous_hash")) != expected_previous:
                    raise SimulationError(
                        "E_RECOVERY_INVALID",
                        "hash chain predecessor mismatch",
                        step=expected_step,
                    )
                expected_previous = str(link.get("state_hash"))
                expected_step += 1
            if expected_step - 1 != self.state.step:
                raise SimulationError(
                    "E_RECOVERY_INVALID",
                    "hash chain length does not match snapshot step",
                    step=self.state.step,
                )

    @property
    def next_step(self) -> int:
        return int(self.state.step) + 1

    @property
    def complete(self) -> bool:
        return self.state.step >= self.initial.end_step

    def _pending_before(self) -> dict[str, int]:
        return {
            cover_id: self.state.pending[cover_id]
            for cover_id in sorted(self.state.pending)
        }

    def _subtract_pending(self, prediction: Mapping[str, int]) -> None:
        for cover_id, amount in prediction.items():
            current = self.state.pending.get(cover_id, 0) - int(amount)
            if current < 0:
                raise SimulationError(
                    "E_DAMAGE_IMBALANCE",
                    "pending prediction ledger underflow",
                    step=self.next_step,
                    cover_id=cover_id,
                )
            if current:
                self.state.pending[cover_id] = current
            else:
                self.state.pending.pop(cover_id, None)

    def _add_pending(self, prediction: Mapping[str, int]) -> None:
        for cover_id, amount in prediction.items():
            if amount <= 0:
                continue
            if cover_id not in self.state.covers:
                continue
            self.state.pending[cover_id] = (
                self.state.pending.get(cover_id, 0) + int(amount)
            )

    def _record_report(self, report: dict[str, Any]) -> None:
        self.state.operational_reports.append(report)

    def _accept_shot(self, shot: ShotRecord, step: int) -> None:
        late_by = step - shot.target_step
        if late_by > self.initial.compensation_window_steps:
            report = {
                "code": "E_OP_LATE",
                "step": step,
                "shot_id": shot.record_id,
                "player_id": shot.player_id,
                "client_seq": shot.client_seq,
                "lag_steps": late_by,
                "window_steps": self.initial.compensation_window_steps,
            }
            self.state.rejections.append(report)
            self._record_report(report)
            return
        self.state.seen_clients[shot.key] = shot.record_id
        self._add_pending(shot.prediction)
        self.state.buffer.append(shot)

    def _handle_shot_arrival(self, shot: ShotRecord, step: int) -> None:
        if shot.key in self.state.seen_clients:
            self.state.duplicate_count += 1
            report = {
                "code": "E_OP_DUPLICATE",
                "step": step,
                "shot_id": shot.record_id,
                "player_id": shot.player_id,
                "client_seq": shot.client_seq,
                "kept_record_id": self.state.seen_clients[shot.key],
            }
            self.state.rejections.append(report)
            self._record_report(report)
            return
        self._accept_shot(shot, step)

    def _process_revocation(
        self, command: RevocationCommand, step: int
    ) -> dict[str, Any]:
        before = self._pending_before()
        result: dict[str, Any]
        if command.key in self.state.settled_shots:
            outcome = self.state.settled_shots[command.key]
            result = {
                "revocation_id": command.revocation_id,
                "player_id": command.player_id,
                "client_seq": command.client_seq,
                "step": step,
                "status": "REJECTED",
                "reason_code": "E_REVOKE_CLOSED",
                "settled_at_step": outcome.settlement_step,
                "restore_amount": 0,
                "before_pending": before,
                "after_pending": before,
                "reason": command.reason,
            }
            self._record_report(
                {
                    "code": "E_REVOKE_CLOSED",
                    "step": step,
                    "revocation_id": command.revocation_id,
                    "player_id": command.player_id,
                    "client_seq": command.client_seq,
                    "settled_at_step": outcome.settlement_step,
                }
            )
        else:
            candidate: ShotRecord | None = None
            for shot in self.state.buffer:
                if shot.key == command.key:
                    candidate = shot
                    break
            if candidate is None:
                result = {
                    "revocation_id": command.revocation_id,
                    "player_id": command.player_id,
                    "client_seq": command.client_seq,
                    "step": step,
                    "status": "REJECTED",
                    "reason_code": "E_REVOKE_NOT_FOUND",
                    "restore_amount": 0,
                    "before_pending": before,
                    "after_pending": before,
                    "reason": command.reason,
                }
                self._record_report(
                    {
                        "code": "E_REVOKE_NOT_FOUND",
                        "step": step,
                        "revocation_id": command.revocation_id,
                        "player_id": command.player_id,
                        "client_seq": command.client_seq,
                    }
                )
            elif step - candidate.arrival_step > self.initial.compensation_window_steps:
                result = {
                    "revocation_id": command.revocation_id,
                    "player_id": command.player_id,
                    "client_seq": command.client_seq,
                    "step": step,
                    "status": "REJECTED",
                    "reason_code": "E_REVOKE_CLOSED",
                    "restore_amount": 0,
                    "before_pending": before,
                    "after_pending": before,
                    "reason": command.reason,
                }
                self._record_report(
                    {
                        "code": "E_REVOKE_CLOSED",
                        "step": step,
                        "revocation_id": command.revocation_id,
                        "player_id": command.player_id,
                        "client_seq": command.client_seq,
                        "lag_steps": step - candidate.arrival_step,
                    }
                )
            else:
                self.state.buffer = [
                    shot for shot in self.state.buffer if shot.key != command.key
                ]
                self._subtract_pending(candidate.prediction)
                after = self._pending_before()
                compensation = {
                    cover_id: int(amount)
                    for cover_id, amount in sorted(candidate.prediction.items())
                    if amount > 0
                }
                result = {
                    "revocation_id": command.revocation_id,
                    "player_id": command.player_id,
                    "client_seq": command.client_seq,
                    "step": step,
                    "status": REVOKE_ACCEPTED,
                    "reason_code": None,
                    "restore_amount": sum(compensation.values()),
                    "restore_by_cover": compensation,
                    "revoked_record_id": candidate.record_id,
                    "before_pending": before,
                    "after_pending": after,
                    "reason": command.reason,
                }
        self.state.revocations.append(result)
        return result

    def _apply_authoritative_damage(
        self, shot: ShotRecord, step: int
    ) -> tuple[dict[str, int], list[dict[str, Any]]]:
        result = compute_damage(self.initial, shot.projectile)
        actual: dict[str, int] = {}
        if result.direct_cover_id is None:
            return actual, []
        crack_events: list[dict[str, Any]] = []
        for cover_id in self.initial.cover_ids:
            raw = int(result.raw_damage.get(cover_id, 0))
            if raw <= 0:
                continue
            cover = self.state.covers[cover_id]
            applied = min(raw, cover.remaining)
            if applied <= 0:
                continue
            before_segment = cover.segment
            cover.damage += applied
            cover.remaining -= applied
            after_segment = cover.segment
            cover.cracked = cover.cracked or after_segment > 0
            cover.alive = cover.remaining > 0
            actual[cover_id] = applied
            if after_segment > before_segment:
                event = {
                    "step": step,
                    "cover_id": cover_id,
                    "source_shot_id": shot.record_id,
                    "source_player_id": shot.player_id,
                    "cause": result.causes.get(cover_id, "chain"),
                    "damage_units": applied,
                    "segment_before": before_segment,
                    "segment_after": after_segment,
                }
                cover.crack_events.append(event)
                crack_events.append(event)
        return actual, crack_events

    def _settle_shot(self, shot: ShotRecord, step: int) -> ShotOutcome:
        predicted = dict(shot.prediction)
        self._subtract_pending(predicted)
        actual, crack_events = self._apply_authoritative_damage(shot, step)
        hit_cover_id = locate_hit(self.initial, shot.projectile)
        conflicts: list[dict[str, Any]] = []
        errors: list[dict[str, Any]] = []
        all_covers = sorted(set(predicted) | set(actual))
        for cover_id in all_covers:
            predicted_value = int(predicted.get(cover_id, 0))
            actual_value = int(actual.get(cover_id, 0))
            if predicted_value != actual_value:
                conflict = {
                    "code": "E_DAMAGE_CONFLICT",
                    "step": step,
                    "cover_id": cover_id,
                    "predicted": from_micro(predicted_value),
                    "authoritative": from_micro(actual_value),
                    "predicted_micro": predicted_value,
                    "authoritative_micro": actual_value,
                    "shot_id": shot.record_id,
                    "player_id": shot.player_id,
                    "client_seq": shot.client_seq,
                }
                conflicts.append(conflict)
                errors.append(conflict)
        if hit_cover_id is None:
            status = REFUNDED
            no_hit = {
                "code": "E_NO_HIT",
                "step": step,
                "shot_id": shot.record_id,
                "player_id": shot.player_id,
                "client_seq": shot.client_seq,
                "projectile": shot.projectile.to_dict(),
            }
            errors.append(no_hit)
            self._record_report(no_hit)
        elif not actual:
            status = REFUNDED
            refund = {
                "code": "E_DAMAGE_REFUNDED",
                "step": step,
                "shot_id": shot.record_id,
                "player_id": shot.player_id,
                "client_seq": shot.client_seq,
                "cover_id": hit_cover_id,
                "message": "all affected covers were already at zero durability",
            }
            errors.append(refund)
            self._record_report(refund)
        elif conflicts:
            status = CONFLICT
            for conflict in conflicts:
                self._record_report(conflict)
        else:
            status = APPLIED
        outcome = ShotOutcome(
            record=shot,
            settlement_step=step,
            status=status,
            predicted=predicted,
            authoritative=actual,
            conflicts=conflicts,
            errors=errors,
        )
        self.state.settled_shots[shot.key] = outcome
        _ = crack_events
        return outcome

    def _step_operations(
        self,
        step: int,
        extra_revocations: Sequence[RevocationCommand],
    ) -> list[tuple[int, int, str, Any]]:
        operations: list[tuple[int, int, str, Any]] = []
        for shot in self._shots_by_arrival.get(step, []):
            operations.append(
                (shot.arrival_seq, 1, f"shot:{shot.record_id}", shot)
            )
        revocations = list(self._revocations_by_arrival.get(step, []))
        revocations.extend(extra_revocations)
        for command in revocations:
            operations.append(
                (
                    command.arrival_seq,
                    0,
                    f"revoke:{command.revocation_id}",
                    command,
                )
            )
        operations.sort(key=lambda item: (item[0], item[1], item[2]))
        return operations

    def _hash_revocations(self) -> list[dict[str, Any]]:
        stable: list[dict[str, Any]] = []
        for item in self.state.revocations:
            stable.append(
                {
                    "revocation_id": str(item.get("revocation_id", "")),
                    "player_id": str(item.get("player_id", "")),
                    "client_seq": int(item.get("client_seq", 0)),
                    "step": int(item.get("step", 0)),
                    "status": str(item.get("status", "")),
                    "reason_code": item.get("reason_code"),
                    "restore_amount": int(item.get("restore_amount", 0)),
                    "before_pending": [
                        [cover_id, int(value)]
                        for cover_id, value in sorted(
                            dict(item.get("before_pending", {})).items()
                        )
                    ],
                    "after_pending": [
                        [cover_id, int(value)]
                        for cover_id, value in sorted(
                            dict(item.get("after_pending", {})).items()
                        )
                    ],
                }
            )
        return stable

    def _ledger_check(
        self, step: int, step_errors: list[dict[str, Any]]
    ) -> None:
        calculated_pending: dict[str, int] = {}
        for shot in self.state.buffer:
            for cover_id, amount in shot.prediction.items():
                if amount > 0 and cover_id in self.state.covers:
                    calculated_pending[cover_id] = (
                        calculated_pending.get(cover_id, 0) + amount
                    )
        calculated_pending = {
            cover_id: amount
            for cover_id, amount in calculated_pending.items()
            if amount
        }
        for cover_id in self.initial.cover_ids:
            cover = self.state.covers[cover_id]
            if cover.remaining + cover.damage != cover.definition.max_durability:
                report = {
                    "code": "E_DAMAGE_IMBALANCE",
                    "step": step,
                    "cover_id": cover_id,
                    "remaining": cover.remaining,
                    "damage": cover.damage,
                    "initial": cover.definition.max_durability,
                }
                step_errors.append(report)
                self._record_report(report)
            pending_value = int(self.state.pending.get(cover_id, 0))
            calculated_value = int(calculated_pending.get(cover_id, 0))
            if pending_value != calculated_value:
                report = {
                    "code": "E_DAMAGE_IMBALANCE",
                    "step": step,
                    "cover_id": cover_id,
                    "pending": pending_value,
                    "expected_pending": calculated_value,
                }
                step_errors.append(report)
                self._record_report(report)
        total_remaining = sum(cover.remaining for cover in self.state.covers.values())
        total_damage = sum(cover.damage for cover in self.state.covers.values())
        if total_remaining + total_damage != self.initial.total_durability:
            report = {
                "code": "E_DAMAGE_IMBALANCE",
                "step": step,
                "cover_id": "ALL",
                "remaining_total": total_remaining,
                "damage_total": total_damage,
                "initial_total": self.initial.total_durability,
            }
            step_errors.append(report)
            self._record_report(report)

    def advance(
        self,
        step: int | None = None,
        *,
        extra_revocations: Sequence[RevocationCommand] = (),
    ) -> dict[str, Any]:
        expected = self.next_step
        target = expected if step is None else int(step)
        if target != expected:
            raise SimulationError(
                "E_PROGRESS_INVALID",
                "physics steps must advance by exactly one",
                expected_step=expected,
                requested_step=target,
            )
        if target > self.initial.end_step:
            raise SimulationError(
                "E_PROGRESS_INVALID",
                "step exceeds observation window",
                step=target,
                end_step=self.initial.end_step,
            )
        if self.progress_fault_step == target:
            raise SimulationError(
                "E_PROGRESS_INJECTED",
                "progress fault injected at step boundary",
                step=target,
            )

        step_errors: list[dict[str, Any]] = []
        step_revocations: list[dict[str, Any]] = []
        step_shots: list[ShotOutcome] = []
        step_rejections: list[dict[str, Any]] = []
        before_reports = len(self.state.operational_reports)

        for _, kind, _, payload in self._step_operations(
            target, extra_revocations
        ):
            if kind == 0:
                revocation_result = self._process_revocation(payload, target)
                step_revocations.append(revocation_result)
            else:
                self._handle_shot_arrival(payload, target)

        due = [
            shot
            for shot in self.state.buffer
            if max(shot.target_step, shot.arrival_step) <= target
        ]
        due.sort(
            key=lambda shot: (
                max(shot.target_step, shot.arrival_step),
                shot.player_id,
                shot.client_seq,
                shot.record_id,
            )
        )
        due_keys = {(shot.player_id, shot.client_seq): shot.record_id for shot in due}
        for shot in due:
            outcome = self._settle_shot(shot, target)
            step_shots.append(outcome)
        self.state.buffer = [
            shot
            for shot in self.state.buffer
            if (shot.player_id, shot.client_seq) not in due_keys
        ]

        self.state.step = target
        self._ledger_check(target, step_errors)
        step_rejections = [
            item
            for item in self.state.rejections
            if int(item.get("step", -1)) == target
        ]
        step_operational = self.state.operational_reports[before_reports:]
        previous_hash = (
            str(self.state.hash_chain[-1]["state_hash"])
            if self.state.hash_chain
            else GENESIS_HASH
        )
        cover_hashes = [
            {
                "cover_id": cover_id,
                "remaining": self.state.covers[cover_id].remaining,
                "cracked": self.state.covers[cover_id].cracked,
                "alive": self.state.covers[cover_id].alive,
            }
            for cover_id in self.initial.cover_ids
        ]
        payload, link = next_chain_link(
            step=target,
            covers=cover_hashes,
            pending=self.state.pending,
            revocations=self._hash_revocations(),
            previous_hash=previous_hash,
        )
        self.state.hash_chain.append(link)
        summary = {
            "step": target,
            "time_ms": target * self.initial.step_ms,
            "sample_rule": self.initial.sample_rule,
            "truth_source": self.initial.truth_source,
            "covers": [
                {
                    **self.state.covers[cover_id].state_dict(),
                    "max_durability": self.state.covers[cover_id].definition.max_durability,
                    "fraction": (
                        self.state.covers[cover_id].remaining
                        / self.state.covers[cover_id].definition.max_durability
                    ),
                }
                for cover_id in self.initial.cover_ids
            ],
            "pending": dict(sorted(self.state.pending.items())),
            "display_remaining": {
                cover_id: (
                    self.state.covers[cover_id].remaining
                    - int(self.state.pending.get(cover_id, 0))
                )
                for cover_id in self.initial.cover_ids
            },
            "ledger": [
                {
                    "cover_id": cover_id,
                    "authority_remaining": self.state.covers[cover_id].remaining,
                    "pending": int(self.state.pending.get(cover_id, 0)),
                    "display_remaining": (
                        self.state.covers[cover_id].remaining
                        - int(self.state.pending.get(cover_id, 0))
                    ),
                    "applied_damage": self.state.covers[cover_id].damage,
                    "conserved": (
                        self.state.covers[cover_id].remaining
                        + self.state.covers[cover_id].damage
                        == self.state.covers[cover_id].definition.max_durability
                        and (
                            self.state.covers[cover_id].remaining
                            - int(self.state.pending.get(cover_id, 0))
                            + int(self.state.pending.get(cover_id, 0))
                            == self.state.covers[cover_id].remaining
                        )
                    ),
                }
                for cover_id in self.initial.cover_ids
            ],
            "shots": [
                {
                    "record_id": outcome.record.record_id,
                    "player_id": outcome.record.player_id,
                    "client_seq": outcome.record.client_seq,
                    "status": outcome.status,
                    "predicted": {
                        cover_id: from_micro(value)
                        for cover_id, value in sorted(outcome.predicted.items())
                    },
                    "authoritative": {
                        cover_id: from_micro(value)
                        for cover_id, value in sorted(outcome.authoritative.items())
                    },
                }
                for outcome in step_shots
            ],
            "revocations": step_revocations,
            "rejections": step_rejections,
            "conflicts": [
                report
                for report in step_operational
                if report.get("code") == "E_DAMAGE_CONFLICT"
            ],
            "errors": step_operational,
            "applied_damage_total": sum(
                cover.damage for cover in self.state.covers.values()
            ),
            "remaining_total": sum(
                cover.remaining for cover in self.state.covers.values()
            ),
            "initial_total": self.initial.total_durability,
            "state_hash": link["state_hash"],
            "previous_hash": link["previous_hash"],
            "chain_prefix_digest": chain_prefix_digest(self.state.hash_chain),
            "buffer_watermark": self.state.buffer_watermark(),
            "duplicate_count": self.state.duplicate_count,
            "payload": payload,
        }
        self.last_summary = summary
        return summary

    def run_to(
        self,
        end_step: int | None = None,
        *,
        on_step: Any = None,
        cancel_check: Any = None,
        pause_check: Any = None,
    ) -> list[dict[str, Any]]:
        end = self.initial.end_step if end_step is None else min(
            end_step, self.initial.end_step
        )
        summaries: list[dict[str, Any]] = []
        while self.state.step < end:
            if cancel_check is not None and cancel_check():
                break
            if pause_check is not None:
                pause_check()
            summary = self.advance()
            summaries.append(summary)
            if on_step is not None:
                on_step(summary)
        return summaries

    @staticmethod
    def hash_from_state_dict(state: Mapping[str, Any]) -> tuple[dict[str, Any], str]:
        covers = [
            {
                "cover_id": item["cover_id"],
                "remaining": int(item["remaining"]),
                "cracked": bool(item["cracked"]),
                "alive": bool(item["alive"]),
            }
            for item in state["covers"]
        ]
        pending = {
            str(cover_id): int(amount) for cover_id, amount in state["pending"]
        }
        revocations = []
        for item in state["revocations"]:
            revocations.append(
                {
                    "revocation_id": str(item.get("revocation_id", "")),
                    "player_id": str(item.get("player_id", "")),
                    "client_seq": int(item.get("client_seq", 0)),
                    "step": int(item.get("step", 0)),
                    "status": str(item.get("status", "")),
                    "reason_code": item.get("reason_code"),
                    "restore_amount": int(item.get("restore_amount", 0)),
                    "before_pending": [
                        [cover_id, int(value)]
                        for cover_id, value in sorted(
                            dict(item.get("before_pending", {})).items()
                        )
                    ],
                    "after_pending": [
                        [cover_id, int(value)]
                        for cover_id, value in sorted(
                            dict(item.get("after_pending", {})).items()
                        )
                    ],
                }
            )
        previous = (
            str(state["hash_chain"][-2]["state_hash"])
            if len(state.get("hash_chain", [])) >= 2
            else GENESIS_HASH
        )
        if state.get("hash_chain"):
            previous = str(state["hash_chain"][-1].get("previous_hash", previous))
        payload = state_payload(
            step=int(state["step"]),
            covers=covers,
            pending=pending,
            revocations=revocations,
            previous_hash=previous,
        )
        return payload, compute_state_hash(payload)

