"""Tick-level deterministic arena rules engine."""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .checkpoint import CheckpointStore, ResumeInfo
from .codes import (
    E_ACTION,
    E_ARG,
    E_BUFFER,
    E_CANCELLED,
    E_COOLDOWN,
    E_FINISHED,
    E_ITEM,
    E_MEMORY,
    E_MORE,
    E_MOVE,
    E_NUM,
    E_OK,
    E_ORDER,
    E_PAUSED,
    E_RANGE,
    E_RES,
    E_STATE,
    E_UNIT,
    TickArenaError,
)
from .events import EventSource, RawEvent
from .model import MatchDef, SkillDef, UnitDef, parse_initial_state
from .numeric import BPS, NumericError, apply_bps, assert_no_float, require_int, state_size_estimate
from .replay import REPLAY_FORMAT_VERSION, ReplayRecorder, build_replay_header
from .rng import derive_u64, roll_bps, signed_jitter_bps


@dataclass(frozen=True)
class EngineLimits:
    max_batch_ticks: int = 1_024
    event_buffer_limit: int = 4_096
    memory_limit_bytes: int = 64 * 1024 * 1024

    def validate(self) -> "EngineLimits":
        require_int(self.max_batch_ticks, "max_batch_ticks", minimum=1)
        require_int(self.event_buffer_limit, "event_buffer_limit", minimum=1)
        require_int(self.memory_limit_bytes, "memory_limit_bytes", minimum=1)
        return self


@dataclass
class UnitRuntime:
    definition: UnitDef
    x: int
    y: int
    hp: int
    resource: int
    alive: bool
    respawn_at: int | None
    cooldowns: dict[str, int]
    items: dict[str, int]

    @classmethod
    def initial(cls, definition: UnitDef) -> "UnitRuntime":
        return cls(
            definition=definition,
            x=definition.x,
            y=definition.y,
            hp=definition.hp,
            resource=definition.resource,
            alive=True,
            respawn_at=None,
            cooldowns={skill_id: 0 for skill_id in definition.skills},
            items=dict(definition.items),
        )

    def revive(self, tick: int, hp_bps: int, resource_bps: int) -> None:
        self.alive = True
        self.x = self.definition.spawn_x
        self.y = self.definition.spawn_y
        self.hp = max(1, apply_bps(self.definition.max_hp, hp_bps))
        self.resource = min(
            self.definition.max_resource,
            apply_bps(self.definition.max_resource, resource_bps),
        )
        self.respawn_at = None
        for skill_id in self.cooldowns:
            self.cooldowns[skill_id] = 0

    def to_state_dict(self) -> dict[str, Any]:
        return {
            "x": self.x,
            "y": self.y,
            "hp": self.hp,
            "resource": self.resource,
            "alive": self.alive,
            "respawn_at": self.respawn_at,
            "cooldowns": dict(self.cooldowns),
            "items": dict(self.items),
        }

    @classmethod
    def from_state_dict(cls, definition: UnitDef, raw: Any) -> "UnitRuntime":
        if not isinstance(raw, dict):
            raise TickArenaError(E_STATE, f"unit {definition.unit_id} state is invalid")
        x = require_int(raw.get("x"), "unit.x", minimum=0)
        y = require_int(raw.get("y"), "unit.y", minimum=0)
        hp = require_int(raw.get("hp"), "unit.hp", minimum=0)
        resource = require_int(raw.get("resource"), "unit.resource", minimum=0)
        alive_raw = raw.get("alive")
        if not isinstance(alive_raw, bool):
            raise TickArenaError(E_STATE, "unit.alive must be boolean")
        respawn_raw = raw.get("respawn_at")
        respawn_at = (
            None
            if respawn_raw is None
            else require_int(respawn_raw, "unit.respawn_at", minimum=0)
        )
        cooldowns_raw = raw.get("cooldowns")
        if not isinstance(cooldowns_raw, dict):
            raise TickArenaError(E_STATE, "unit.cooldowns must be an object")
        cooldowns: dict[str, int] = {}
        for skill_id in definition.skills:
            cooldowns[skill_id] = require_int(
                cooldowns_raw.get(skill_id, 0),
                f"unit.cooldowns.{skill_id}",
                minimum=0,
            )
        items_raw = raw.get("items", {})
        if not isinstance(items_raw, dict):
            raise TickArenaError(E_STATE, "unit.items must be an object")
        items = {
            str(item_id): require_int(value, f"unit.items.{item_id}", minimum=0)
            for item_id, value in items_raw.items()
        }
        return cls(
            definition=definition,
            x=x,
            y=y,
            hp=hp,
            resource=resource,
            alive=alive_raw,
            respawn_at=respawn_at,
            cooldowns=cooldowns,
            items=items,
        )

    def full_dict(self) -> dict[str, Any]:
        return {
            "player_id": self.definition.player_id,
            "team": self.definition.team,
            "x": self.x,
            "y": self.y,
            "hp": self.hp,
            "max_hp": self.definition.max_hp,
            "resource": self.resource,
            "max_resource": self.definition.max_resource,
            "alive": self.alive,
            "respawn_at": self.respawn_at,
            "cooldowns": dict(sorted(self.cooldowns.items())),
            "items": dict(sorted(self.items.items())),
        }

    def tick_dict(self) -> dict[str, Any]:
        return {
            "x": self.x,
            "y": self.y,
            "hp": self.hp,
            "resource": self.resource,
            "alive": self.alive,
            "cooldowns": dict(sorted(self.cooldowns.items())),
        }


@dataclass
class EngineState:
    tick: int
    units: dict[str, UnitRuntime]
    scores: dict[str, int]
    result: dict[str, Any] | None
    event_offset: int
    rng_decisions: int
    rejected_total: int

    @classmethod
    def initial(cls, match: MatchDef) -> "EngineState":
        return cls(
            tick=-1,
            units={
                unit.unit_id: UnitRuntime.initial(unit)
                for unit in match.units
            },
            scores={team: 0 for team in match.team_ids},
            result=None,
            event_offset=0,
            rng_decisions=0,
            rejected_total=0,
        )

    def to_state_dict(self) -> dict[str, Any]:
        return {
            "tick": self.tick,
            "units": {
                unit_id: unit.to_state_dict()
                for unit_id, unit in self.units.items()
            },
            "scores": dict(sorted(self.scores.items())),
            "result": self.result,
            "event_offset": self.event_offset,
            "rng_decisions": self.rng_decisions,
            "rejected_total": self.rejected_total,
        }

    @classmethod
    def from_state_dict(cls, match: MatchDef, raw: Any) -> "EngineState":
        if not isinstance(raw, dict):
            raise TickArenaError(E_STATE, "checkpoint engine state is invalid")
        tick = require_int(raw.get("tick"), "state.tick", minimum=-1)
        units_raw = raw.get("units")
        if not isinstance(units_raw, dict):
            raise TickArenaError(E_STATE, "checkpoint units are missing")
        units: dict[str, UnitRuntime] = {}
        for unit in match.units:
            if unit.unit_id not in units_raw:
                raise TickArenaError(E_STATE, f"checkpoint unit {unit.unit_id} is missing")
            units[unit.unit_id] = UnitRuntime.from_state_dict(
                unit, units_raw[unit.unit_id]
            )
        scores_raw = raw.get("scores")
        if not isinstance(scores_raw, dict):
            raise TickArenaError(E_STATE, "checkpoint scores are missing")
        scores = {
            team: require_int(scores_raw.get(team), f"scores.{team}", minimum=0)
            for team in match.team_ids
        }
        result = raw.get("result")
        if result is not None and not isinstance(result, dict):
            raise TickArenaError(E_STATE, "checkpoint result is invalid")
        event_offset = require_int(
            raw.get("event_offset"), "state.event_offset", minimum=0
        )
        rng_decisions = require_int(
            raw.get("rng_decisions"), "state.rng_decisions", minimum=0
        )
        rejected_total = require_int(
            raw.get("rejected_total"), "state.rejected_total", minimum=0
        )
        return cls(
            tick=tick,
            units=units,
            scores=scores,
            result=dict(result) if result is not None else None,
            event_offset=event_offset,
            rng_decisions=rng_decisions,
            rejected_total=rejected_total,
        )


@dataclass(frozen=True)
class StepResult:
    code: str
    tick: int
    finished: bool
    rejected: tuple[dict[str, Any], ...] = ()
    message: str = ""


@dataclass(frozen=True)
class BatchResult:
    code: str
    ticks_advanced: int
    tick: int
    finished: bool
    rejection_counts: dict[str, int] = field(default_factory=dict)
    message: str = ""


def _event_summary(event: RawEvent, code: str | None) -> dict[str, Any]:
    payload = event.payload
    event_type = payload.get("type")
    actor = payload.get("actor")
    return {
        "seq": event.index,
        "type": event_type if isinstance(event_type, str) else None,
        "actor": actor if isinstance(actor, str) else None,
        "code": code,
    }


def _overflow_summary(code: str, count: int, first: int | None) -> dict[str, Any]:
    return {
        "seq": first if first is not None else -1,
        "type": "*",
        "actor": "*",
        "code": code,
        "count": count,
    }


class Engine:
    """Deterministic tick engine.

    The engine itself is single-owner/single-threaded.  Run independent matches
    concurrently, never mutate one Engine from multiple threads.
    """

    def __init__(
        self,
        initial_state: Any,
        event_source: EventSource,
        *,
        replay_path: str | Path | None = None,
        snapshot_interval: int = 100,
        checkpoint_store: CheckpointStore | None = None,
        checkpoint_every: int = 256,
        limits: EngineLimits | None = None,
        capture_trace: bool = False,
        restore_state: EngineState | None = None,
        replay_recorder: ReplayRecorder | None = None,
        create_recorder: bool = True,
    ) -> None:
        self.match = parse_initial_state(initial_state)
        self.source = event_source
        if self.source.initial_fingerprint != self.match.initial_fingerprint:
            raise TickArenaError(
                E_STATE, "event stream initial fingerprint does not match initial state"
            )
        self.snapshot_interval = require_int(
            snapshot_interval, "snapshot_interval", minimum=1
        )
        self.checkpoint_every = require_int(
            checkpoint_every, "checkpoint_every", minimum=1
        )
        self.limits = (limits or EngineLimits()).validate()
        self.checkpoint_store = checkpoint_store
        if self.checkpoint_store is not None and restore_state is None:
            # A non-resume construction starts a new run.  Never let it append
            # to, or be blocked by, a checkpoint from a previous run.
            self.checkpoint_store.invalidate("fresh engine start")
        self.state = restore_state if restore_state is not None else EngineState.initial(self.match)
        self._validate_restored_state()
        self._pause_event = threading.Event()
        self._cancel_event = threading.Event()
        self._trace: list[dict[str, Any]] | None = [] if capture_trace else None
        self._finalized = False
        self._last_step_rejections: tuple[dict[str, Any], ...] = ()
        self.replay_path = Path(replay_path) if replay_path is not None else None
        self.recorder = replay_recorder
        if self.recorder is None and self.replay_path is not None and create_recorder:
            header = build_replay_header(
                self.match,
                self.source.fingerprint,
                snapshot_interval=self.snapshot_interval,
                event_buffer_limit=self.limits.event_buffer_limit,
            )
            self.recorder = ReplayRecorder(self.replay_path, header)
        if self.done:
            self._finalized = True

    def _validate_restored_state(self) -> None:
        if self.state.tick > self.match.tick_count - 1:
            raise TickArenaError(E_STATE, "restored tick exceeds match tick_count")
        for unit in self.match.units:
            runtime = self.state.units.get(unit.unit_id)
            if runtime is None:
                raise TickArenaError(E_STATE, f"restored unit {unit.unit_id} is missing")
            if not 0 <= runtime.x < self.match.width:
                raise TickArenaError(E_STATE, "restored unit x is out of bounds")
            if not 0 <= runtime.y < self.match.height:
                raise TickArenaError(E_STATE, "restored unit y is out of bounds")
            if not 0 <= runtime.hp <= unit.max_hp:
                raise TickArenaError(E_STATE, "restored unit hp is out of range")
            if not 0 <= runtime.resource <= unit.max_resource:
                raise TickArenaError(E_STATE, "restored unit resource is out of range")
        if sorted(self.state.scores) != list(self.match.team_ids):
            raise TickArenaError(E_STATE, "restored team scores do not match match teams")

    @property
    def done(self) -> bool:
        return self.state.result is not None

    @property
    def trace(self) -> list[dict[str, Any]] | None:
        return self._trace

    def request_pause(self) -> None:
        self._pause_event.set()

    def request_cancel(self) -> None:
        self._cancel_event.set()

    def clear_pause(self) -> None:
        self._pause_event.clear()

    def clear_cancel(self) -> None:
        self._cancel_event.clear()

    def full_snapshot(self) -> dict[str, Any]:
        return {
            "tick": self.state.tick,
            "units": {
                unit.unit_id: self.state.units[unit.unit_id].full_dict()
                for unit in self.match.units
            },
            "scores": dict(sorted(self.state.scores.items())),
            "result": self.state.result,
            "event_offset": self.state.event_offset,
            "rng_decisions": self.state.rng_decisions,
            "rejected_total": self.state.rejected_total,
        }

    def _tick_record(self, events: list[dict[str, Any]]) -> dict[str, Any]:
        return {
            "kind": "tick",
            "tick": self.state.tick,
            "units": {
                unit.unit_id: self.state.units[unit.unit_id].tick_dict()
                for unit in self.match.units
            },
            "scores": dict(sorted(self.state.scores.items())),
            "result": self.state.result,
            "events": events,
            "event_offset": self.state.event_offset,
        }

    def _snapshot_record(self) -> dict[str, Any]:
        return {
            "kind": "snapshot",
            "tick": self.state.tick,
            "units": {
                unit.unit_id: self.state.units[unit.unit_id].full_dict()
                for unit in self.match.units
            },
            "scores": dict(sorted(self.state.scores.items())),
            "result": self.state.result,
            "event_offset": self.state.event_offset,
            "rng_decisions": self.state.rng_decisions,
            "rejected_total": self.state.rejected_total,
        }

    def _key_metrics(self) -> dict[str, Any]:
        return {
            "final_tick": self.state.tick,
            "scores": dict(sorted(self.state.scores.items())),
            "result": self.state.result,
            "event_offset": self.state.event_offset,
            "rng_decisions": self.state.rng_decisions,
            "rejected_total": self.state.rejected_total,
        }

    def _start_of_tick(self, tick: int) -> None:
        rules = self.match.rules
        for definition in self.match.units:
            unit = self.state.units[definition.unit_id]
            if not unit.alive:
                if unit.respawn_at is not None and tick >= unit.respawn_at:
                    unit.revive(
                        tick,
                        rules.revive_hp_bps,
                        rules.revive_resource_bps,
                    )
                else:
                    continue
            for skill_id in unit.cooldowns:
                if unit.cooldowns[skill_id] > 0:
                    unit.cooldowns[skill_id] -= 1
            unit.resource = min(
                definition.max_resource,
                unit.resource + rules.passive_resource,
            )

    def _reject(self, code: str) -> str:
        self.state.rejected_total += 1
        return code

    def _actor(self, payload: dict[str, Any]) -> UnitRuntime | None:
        actor = payload.get("actor")
        if not isinstance(actor, str):
            return None
        return self.state.units.get(actor)

    def _apply_event(self, event: RawEvent) -> str | None:
        payload = event.payload
        try:
            assert_no_float(payload, "$event")
        except NumericError:
            return self._reject(E_NUM)
        if not isinstance(payload, dict):
            return self._reject(E_ACTION)
        event_type = payload.get("type")
        if not isinstance(event_type, str):
            return self._reject(E_ACTION)
        unit = self._actor(payload)
        if unit is None:
            return self._reject(E_UNIT)
        if not unit.alive:
            return self._reject(E_UNIT)

        if event_type == "move":
            try:
                dx = require_int(payload.get("dx"), "event.dx")
                dy = require_int(payload.get("dy"), "event.dy")
            except NumericError:
                return self._reject(E_NUM)
            if (
                abs(dx) > unit.definition.move_speed
                or abs(dy) > unit.definition.move_speed
            ):
                return self._reject(E_MOVE)
            new_x = unit.x + dx
            new_y = unit.y + dy
            if not (0 <= new_x < self.match.width and 0 <= new_y < self.match.height):
                return self._reject(E_MOVE)
            unit.x = new_x
            unit.y = new_y
            return None

        if event_type == "gather":
            rules = self.match.rules
            gain_roll = derive_u64(
                self.match.seed, self.state.tick, event.index, "gather:gain"
            )
            gain = rules.gather_base
            if rules.gather_variance:
                gain += gain_roll % (rules.gather_variance + 1)
            unit.resource = min(
                unit.definition.max_resource,
                unit.resource + gain,
            )
            self.state.rng_decisions += 1
            drop = roll_bps(
                self.match.seed, self.state.tick, event.index, "gather:drop"
            )
            self.state.rng_decisions += 1
            if drop < rules.gather_drop_bps:
                unit.items["potion"] = unit.items.get("potion", 0) + rules.potion_drop
            return None

        if event_type == "item":
            item_id = payload.get("item")
            if item_id != "potion":
                return self._reject(E_ITEM)
            count = unit.items.get("potion", 0)
            if count <= 0:
                return self._reject(E_ITEM)
            unit.items["potion"] = count - 1
            heal = apply_bps(
                unit.definition.max_hp,
                self.match.rules.potion_heal_bps,
            )
            unit.hp = min(unit.definition.max_hp, unit.hp + heal)
            return None

        if event_type == "skill":
            skill_id = payload.get("skill")
            target_id = payload.get("target")
            if not isinstance(skill_id, str) or not isinstance(target_id, str):
                return self._reject(E_ACTION)
            skill = unit.definition.skills.get(skill_id)
            if skill is None:
                return self._reject(E_ACTION)
            target = self.state.units.get(target_id)
            if (
                target is None
                or not target.alive
                or target.definition.team == unit.definition.team
            ):
                return self._reject(E_UNIT)
            if unit.cooldowns.get(skill_id, 0) > 0:
                return self._reject(E_COOLDOWN)
            if unit.resource < skill.cost:
                return self._reject(E_RES)
            distance = max(abs(unit.x - target.x), abs(unit.y - target.y))
            if distance > skill.range:
                return self._reject(E_RANGE)

            unit.resource -= skill.cost
            unit.cooldowns[skill_id] = skill.cooldown
            hit = roll_bps(
                self.match.seed,
                self.state.tick,
                event.index,
                f"skill:{skill_id}:hit",
            )
            self.state.rng_decisions += 1
            if hit >= skill.hit_bps:
                return None
            crit = roll_bps(
                self.match.seed,
                self.state.tick,
                event.index,
                f"skill:{skill_id}:crit",
            )
            self.state.rng_decisions += 1
            jitter = signed_jitter_bps(
                self.match.seed,
                self.state.tick,
                event.index,
                f"skill:{skill_id}:jitter",
                skill.variance_bps,
            )
            self.state.rng_decisions += 1
            damage = apply_bps(skill.damage, BPS + jitter)
            if crit < skill.crit_bps:
                damage = apply_bps(damage, skill.crit_multiplier_bps)
            damage = max(1, damage - target.definition.defense)
            target.hp = max(0, target.hp - damage)
            if target.hp == 0:
                target.alive = False
                target.resource = 0
                target.respawn_at = self.state.tick + target.definition.revive_ticks
                killer_team = unit.definition.team
                self.state.scores[killer_team] += self.match.rules.score_per_kill
                if self.state.scores[killer_team] >= self.match.rules.win_score:
                    self.state.result = {
                        "winner_team": killer_team,
                        "reason": "score_limit",
                        "locked_tick": self.state.tick,
                        "scores": dict(sorted(self.state.scores.items())),
                    }
            return None

        return self._reject(E_ACTION)

    def _evaluate_tick_limit(self) -> None:
        ranked: dict[str, tuple[int, int, int]] = {}
        for team in self.match.team_ids:
            hp_total = 0
            resource_total = 0
            for definition in self.match.units:
                if definition.team != team:
                    continue
                unit = self.state.units[definition.unit_id]
                hp_total += unit.hp
                resource_total += unit.resource
            ranked[team] = (
                self.state.scores[team],
                hp_total,
                resource_total,
            )
        best = max(ranked.values())
        winners = [team for team, rank in ranked.items() if rank == best]
        self.state.result = {
            "winner_team": winners[0] if len(winners) == 1 else None,
            "reason": "tick_limit" if len(winners) == 1 else "tick_limit_draw",
            "locked_tick": self.state.tick,
            "scores": dict(sorted(self.state.scores.items())),
        }

    def _process_events(self, tick: int) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
        summaries: list[dict[str, Any]] = []
        rejected: list[dict[str, Any]] = []
        used = 0
        while True:
            remaining = self.limits.event_buffer_limit - used
            if remaining <= 0:
                peek = self.source.peek_tick()
                if peek is not None and peek <= tick:
                    count, first, _ = self.source.discard_for_tick(tick)
                    if count:
                        summary = _overflow_summary(E_BUFFER, count, first)
                        summaries.append(summary)
                        rejected.append(summary)
                        self.state.rejected_total += count
                break
            chunk = self.source.take_for_tick(tick, remaining)
            if not chunk:
                break
            used += len(chunk)
            for event in chunk:
                if self.state.result is not None:
                    code = E_FINISHED
                    self.state.rejected_total += 1
                else:
                    code = event.code
                    if code is None:
                        code = self._apply_event(event)
                    else:
                        self.state.rejected_total += 1
                summary = _event_summary(event, code)
                summaries.append(summary)
                if code is not None:
                    rejected.append(summary)
            if self.state.result is not None:
                peek = self.source.peek_tick()
                if peek is not None and peek <= tick:
                    count, first, _ = self.source.discard_for_tick(tick)
                    if count:
                        summary = _overflow_summary(E_FINISHED, count, first)
                        summaries.append(summary)
                        rejected.append(summary)
                        self.state.rejected_total += count
                break
        if self.state.result is None and tick == self.match.tick_count - 1:
            count, first, _ = self.source.drain_remaining()
            if count:
                summary = _overflow_summary(E_ORDER, count, first)
                summaries.append(summary)
                rejected.append(summary)
                self.state.rejected_total += count
        return summaries, rejected

    def _advance_tick(self) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
        tick = self.state.tick + 1
        if tick >= self.match.tick_count:
            raise TickArenaError(E_STATE, "attempted to advance past tick_count")
        self.state.tick = tick
        self._start_of_tick(tick)
        summaries, rejected = self._process_events(tick)
        self.state.event_offset = self.source.tell()
        if self.state.result is None and tick == self.match.tick_count - 1:
            self._evaluate_tick_limit()
        record = self._tick_record(summaries)
        if self.recorder is not None:
            self.recorder.record_tick(record)
            if tick % self.snapshot_interval == 0:
                self.recorder.record_snapshot(self._snapshot_record())
        if self._trace is not None:
            self._trace.append(record)
        self._last_step_rejections = tuple(rejected)
        if self.done:
            self._finalize_run()
        elif (
            self.checkpoint_store is not None
            and (tick + 1) % self.checkpoint_every == 0
        ):
            self._commit_checkpoint()
        return record, rejected

    def _memory_estimate(self) -> int:
        buffer_budget = self.limits.event_buffer_limit * 256
        return state_size_estimate(
            len(self.state.units),
            self.source.pending_event_bytes + buffer_budget,
        )

    def step(self) -> StepResult:
        if self._cancel_event.is_set():
            self._commit_checkpoint(force=True)
            return StepResult(E_CANCELLED, self.state.tick, self.done, message="cancelled")
        if self._pause_event.is_set():
            self._commit_checkpoint(force=True)
            return StepResult(E_PAUSED, self.state.tick, self.done, message="paused")
        if self._memory_estimate() > self.limits.memory_limit_bytes:
            return StepResult(
                E_MEMORY,
                self.state.tick,
                self.done,
                message="memory watermark exceeded",
            )
        if self.done:
            return StepResult(E_OK, self.state.tick, True, message="match already finished")
        _, rejected = self._advance_tick()
        if self._cancel_event.is_set():
            self._commit_checkpoint(force=True)
            return StepResult(
                E_CANCELLED,
                self.state.tick,
                self.done,
                rejected=tuple(rejected),
                message="cancelled",
            )
        if self._pause_event.is_set():
            self._commit_checkpoint(force=True)
            return StepResult(
                E_PAUSED,
                self.state.tick,
                self.done,
                rejected=tuple(rejected),
                message="paused",
            )
        return StepResult(
            E_OK,
            self.state.tick,
            self.done,
            rejected=tuple(rejected),
        )

    def run_batch(self, batch_ticks: int | None = None) -> BatchResult:
        limit = (
            self.limits.max_batch_ticks
            if batch_ticks is None
            else require_int(batch_ticks, "batch_ticks", minimum=1)
        )
        if limit > self.limits.max_batch_ticks:
            return BatchResult(
                E_ARG,
                0,
                self.state.tick,
                self.done,
                message=(
                    f"batch_ticks {limit} exceeds max_batch_ticks "
                    f"{self.limits.max_batch_ticks}"
                ),
            )
        if self.done:
            return BatchResult(E_OK, 0, self.state.tick, True, message="already finished")
        advanced = 0
        rejection_counts: dict[str, int] = {}
        for _ in range(limit):
            if self._cancel_event.is_set():
                self._commit_checkpoint(force=True)
                return BatchResult(
                    E_CANCELLED,
                    advanced,
                    self.state.tick,
                    self.done,
                    rejection_counts,
                    "cancelled",
                )
            if self._memory_estimate() > self.limits.memory_limit_bytes:
                return BatchResult(
                    E_MEMORY,
                    advanced,
                    self.state.tick,
                    self.done,
                    rejection_counts,
                    "memory watermark exceeded",
                )
            _, rejected = self._advance_tick()
            advanced += 1
            for summary in rejected:
                code = summary.get("code")
                if isinstance(code, str):
                    count = summary.get("count", 1)
                    rejection_counts[code] = rejection_counts.get(code, 0) + int(count)
            if self._pause_event.is_set():
                self._commit_checkpoint(force=True)
                return BatchResult(
                    E_PAUSED,
                    advanced,
                    self.state.tick,
                    self.done,
                    rejection_counts,
                    "paused",
                )
            if self.done:
                break
        code = E_OK if self.done else E_MORE
        return BatchResult(
            code,
            advanced,
            self.state.tick,
            self.done,
            rejection_counts,
        )

    def _commit_checkpoint(self, *, force: bool = False) -> None:
        if self.checkpoint_store is None:
            return
        if self.state.tick < 0:
            return
        replay_progress: dict[str, Any] | None = None
        replay_path = str(self.replay_path) if self.replay_path is not None else None
        if self.recorder is not None and not self.recorder._closed:
            replay_progress = self.recorder.progress()
        payload = {
            "seed": self.match.seed,
            "max_ticks": self.match.tick_count,
            "initial_fingerprint": self.match.initial_fingerprint,
            "event_fingerprint": self.source.fingerprint,
            "replay_format_version": REPLAY_FORMAT_VERSION,
            "limits": {
                "max_batch_ticks": self.limits.max_batch_ticks,
                "event_buffer_limit": self.limits.event_buffer_limit,
                "memory_limit_bytes": self.limits.memory_limit_bytes,
            },
            "state": self.state.to_state_dict(),
            "rng_progress": {
                "derivation": "blake2b-seed-tick-event-purpose-v1",
                "decisions": self.state.rng_decisions,
                "next_tick": self.state.tick + 1,
                "event_offset": self.source.tell(),
            },
            "event": {
                "index": self.source.tell(),
                "last_seen_tick": self.source.last_seen_tick,
            },
            "replay": {
                "enabled": self.recorder is not None,
                "path": replay_path,
                "progress": replay_progress,
            },
            "replay_finalized": self._finalized,
        }
        result = self.checkpoint_store.commit(payload, self.state.tick)
        if not result.ok:
            raise TickArenaError(result.code, result.message)

    def _finalize_run(self) -> None:
        if self._finalized:
            return
        final_snapshot = self._snapshot_record()
        if self.recorder is not None:
            self.recorder.finalize(
                self.full_snapshot(),
                self._key_metrics(),
                final_snapshot=final_snapshot,
            )
        self._finalized = True
        if self.checkpoint_store is not None:
            self._commit_checkpoint(force=True)

    def resource_stats(self) -> dict[str, Any]:
        return {
            "tick": self.state.tick,
            "estimated_state_bytes": self._memory_estimate(),
            "memory_limit_bytes": self.limits.memory_limit_bytes,
            "pending_event_bytes": self.source.pending_event_bytes,
            "max_inflight_events": self.source.max_inflight_events,
            "event_buffer_limit": self.limits.event_buffer_limit,
            "max_batch_ticks": self.limits.max_batch_ticks,
        }

    def close(self) -> None:
        """Release the replay partial and file-backed event reader.

        Call this before simulating a process restart.  A paused replay remains
        resumable through its persisted checkpoint/progress pair.
        """

        if self.recorder is not None and not self.recorder._closed:
            self.recorder.close_partial()
        close_source = getattr(self.source, "close", None)
        if callable(close_source):
            close_source()

    def __enter__(self) -> "Engine":
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.close()

    @classmethod
    def resume(
        cls,
        initial_state: Any,
        event_source: EventSource,
        *,
        checkpoint_store: CheckpointStore,
        replay_path: str | Path | None = None,
        snapshot_interval: int = 100,
        checkpoint_every: int = 256,
        limits: EngineLimits | None = None,
        capture_trace: bool = False,
    ) -> tuple["Engine", ResumeInfo]:
        match = parse_initial_state(initial_state)
        loaded = checkpoint_store.load()
        if not loaded.result.ok or loaded.envelope is None:
            event_source.reset()
            engine = cls(
                initial_state,
                event_source,
                replay_path=replay_path,
                snapshot_interval=snapshot_interval,
                checkpoint_store=checkpoint_store,
                checkpoint_every=checkpoint_every,
                limits=limits,
                capture_trace=capture_trace,
            )
            return engine, ResumeInfo(
                resumed=False,
                restarted=True,
                code=E_STATE,
                reason=loaded.result.message or "checkpoint not found",
            )

        payload = loaded.payload
        invalid_reason: str | None = None
        effective_limits = limits
        persisted_limits = payload.get("limits")
        if isinstance(persisted_limits, dict):
            try:
                persisted = EngineLimits(
                    max_batch_ticks=require_int(
                        persisted_limits.get("max_batch_ticks"),
                        "limits.max_batch_ticks",
                        minimum=1,
                    ),
                    event_buffer_limit=require_int(
                        persisted_limits.get("event_buffer_limit"),
                        "limits.event_buffer_limit",
                        minimum=1,
                    ),
                    memory_limit_bytes=require_int(
                        persisted_limits.get("memory_limit_bytes"),
                        "limits.memory_limit_bytes",
                        minimum=1,
                    ),
                )
                if limits is None:
                    effective_limits = persisted
                elif limits.event_buffer_limit != persisted.event_buffer_limit:
                    invalid_reason = (
                        "checkpoint event_buffer_limit mismatch: "
                        f"{persisted.event_buffer_limit} != {limits.event_buffer_limit}"
                    )
            except TickArenaError as exc:
                invalid_reason = f"checkpoint limits invalid: {exc.message}"
        else:
            invalid_reason = "checkpoint limits are missing"
        req = (
            ("initial_fingerprint", match.initial_fingerprint),
            ("event_fingerprint", event_source.fingerprint),
            ("seed", match.seed),
            ("max_ticks", match.tick_count),
            ("replay_format_version", REPLAY_FORMAT_VERSION),
        )
        for key, expected in req:
            if invalid_reason is not None:
                break
            if payload.get(key) != expected:
                invalid_reason = (
                    f"checkpoint {key} mismatch: persisted {payload.get(key)!r}, "
                    f"current {expected!r}"
                )
                break
        if invalid_reason is None:
            event_meta = payload.get("event")
            if not isinstance(event_meta, dict):
                invalid_reason = "checkpoint event metadata is missing"
            else:
                index = event_meta.get("index")
                last_seen = event_meta.get("last_seen_tick")
                if (
                    isinstance(index, bool)
                    or not isinstance(index, int)
                    or not 0 <= index <= event_source.total_events
                    or isinstance(last_seen, bool)
                    or not isinstance(last_seen, int)
                ):
                    invalid_reason = "checkpoint event offset is invalid"
        restored_state: EngineState | None = None
        if invalid_reason is None:
            try:
                restored_state = EngineState.from_state_dict(match, payload.get("state"))
            except TickArenaError as exc:
                invalid_reason = f"checkpoint state invalid: {exc.message}"
        if invalid_reason is None and restored_state is not None:
            if restored_state.event_offset != payload["event"]["index"]:
                invalid_reason = "checkpoint state event offset mismatch"
        recorder: ReplayRecorder | None = None
        if invalid_reason is None and replay_path is not None:
            replay_meta = payload.get("replay")
            if not isinstance(replay_meta, dict) or not replay_meta.get("enabled"):
                invalid_reason = "checkpoint has no replay metadata"
            else:
                persisted_path = replay_meta.get("path")
                if persisted_path != str(Path(replay_path)):
                    invalid_reason = "checkpoint replay path mismatch"
                elif payload.get("replay_finalized"):
                    recorder = None
                else:
                    progress = replay_meta.get("progress")
                    if not isinstance(progress, dict):
                        invalid_reason = "checkpoint replay progress is missing"
                    else:
                        header = build_replay_header(
                            match,
                            event_source.fingerprint,
                            snapshot_interval=snapshot_interval,
                            event_buffer_limit=(
                                effective_limits or EngineLimits()
                            ).event_buffer_limit,
                        )
                        try:
                            recorder = ReplayRecorder(
                                replay_path,
                                header,
                                resume_progress=progress,
                            )
                        except TickArenaError as exc:
                            invalid_reason = f"replay checkpoint invalid: {exc.message}"
        if invalid_reason is not None or restored_state is None:
            checkpoint_store.invalidate(invalid_reason or "invalid checkpoint")
            event_source.reset()
            engine = cls(
                initial_state,
                event_source,
                replay_path=replay_path,
                snapshot_interval=snapshot_interval,
                checkpoint_store=checkpoint_store,
                checkpoint_every=checkpoint_every,
                limits=effective_limits,
                capture_trace=capture_trace,
            )
            return engine, ResumeInfo(
                resumed=False,
                restarted=True,
                code=E_STATE,
                reason=invalid_reason or "invalid checkpoint",
            )

        event_source.seek(
            payload["event"]["index"],
            payload["event"]["last_seen_tick"],
        )
        engine = cls(
            initial_state,
            event_source,
            replay_path=replay_path,
            snapshot_interval=snapshot_interval,
            checkpoint_store=checkpoint_store,
            checkpoint_every=checkpoint_every,
            limits=effective_limits,
            capture_trace=capture_trace,
            restore_state=restored_state,
            replay_recorder=recorder,
            create_recorder=not bool(payload.get("replay_finalized")),
        )
        return engine, ResumeInfo(
            resumed=True,
            restarted=False,
            code=E_OK,
            reason="checkpoint restored",
        )

