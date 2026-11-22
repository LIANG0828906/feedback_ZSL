"""Independent replay re-simulation and earliest-divergence diagnostics.

This module deliberately does not import or call the engine or the replay
recorder.  It reads validated replay records and re-evaluates the rules with an
independent state machine.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .codes import (
    E_ACTION,
    E_BUFFER,
    E_COOLDOWN,
    E_DIVERGE,
    E_FINISHED,
    E_ITEM,
    E_MOVE,
    E_NUM,
    E_OK,
    E_ORDER,
    E_RANGE,
    E_REPLAY,
    E_RES,
    E_UNIT,
    TickArenaError,
)
from .diff import first_difference
from .events import EventSource, JsonlEventSource, RawEvent
from .model import MatchDef, SkillDef, UnitDef, parse_initial_state
from .numeric import BPS, NumericError, apply_bps, assert_no_float, require_int
from .replay import iter_replay_body, load_and_validate_replay
from .rng import derive_u64, roll_bps, signed_jitter_bps


@dataclass(frozen=True)
class VerificationReport:
    code: str
    message: str
    tick: int | None = None
    rule: str | None = None
    recorded: Any = None
    recomputed: Any = None
    final_state: dict[str, Any] | None = None

    @property
    def ok(self) -> bool:
        return self.code == E_OK


@dataclass
class _VUnit:
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
    def initial(cls, definition: UnitDef) -> "_VUnit":
        return cls(
            definition,
            definition.x,
            definition.y,
            definition.hp,
            definition.resource,
            True,
            None,
            {skill_id: 0 for skill_id in definition.skills},
            dict(definition.items),
        )

    def revive(self, hp_bps: int, resource_bps: int) -> None:
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

    def tick_dict(self) -> dict[str, Any]:
        return {
            "x": self.x,
            "y": self.y,
            "hp": self.hp,
            "resource": self.resource,
            "alive": self.alive,
            "cooldowns": dict(sorted(self.cooldowns.items())),
        }

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


def _summary(event: RawEvent, code: str | None) -> dict[str, Any]:
    payload = event.payload
    event_type = payload.get("type")
    actor = payload.get("actor")
    return {
        "seq": event.index,
        "type": event_type if isinstance(event_type, str) else None,
        "actor": actor if isinstance(actor, str) else None,
        "code": code,
    }


def _bulk(code: str, count: int, first: int | None) -> dict[str, Any]:
    return {
        "seq": first if first is not None else -1,
        "type": "*",
        "actor": "*",
        "code": code,
        "count": count,
    }


class _Verifier:
    """Independent implementation of the public rules."""

    def __init__(self, match: MatchDef, source: EventSource) -> None:
        self.match = match
        self.source = source
        self.tick = -1
        self.units = {
            definition.unit_id: _VUnit.initial(definition)
            for definition in match.units
        }
        self.scores = {team: 0 for team in match.team_ids}
        self.result: dict[str, Any] | None = None
        self.event_offset = 0
        self.rng_decisions = 0
        self.rejected_total = 0
        self.event_buffer_limit = 4_096
        self.last_events: list[dict[str, Any]] = []

    @property
    def done(self) -> bool:
        return self.result is not None

    def _reject(self, code: str) -> str:
        self.rejected_total += 1
        return code

    def _start_of_tick(self) -> None:
        rules = self.match.rules
        for definition in self.match.units:
            unit = self.units[definition.unit_id]
            if not unit.alive:
                if unit.respawn_at is not None and self.tick >= unit.respawn_at:
                    unit.revive(rules.revive_hp_bps, rules.revive_resource_bps)
                else:
                    continue
            for skill_id in unit.cooldowns:
                if unit.cooldowns[skill_id] > 0:
                    unit.cooldowns[skill_id] -= 1
            unit.resource = min(
                definition.max_resource,
                unit.resource + rules.passive_resource,
            )

    def _apply(self, event: RawEvent) -> str | None:
        payload = event.payload
        try:
            assert_no_float(payload)
        except NumericError:
            return self._reject(E_NUM)
        if not isinstance(payload, dict):
            return self._reject(E_ACTION)
        event_type = payload.get("type")
        actor_id = payload.get("actor")
        if not isinstance(event_type, str):
            return self._reject(E_ACTION)
        if not isinstance(actor_id, str):
            return self._reject(E_UNIT)
        unit = self.units.get(actor_id)
        if unit is None or not unit.alive:
            return self._reject(E_UNIT)

        if event_type == "move":
            try:
                dx = require_int(payload.get("dx"), "dx")
                dy = require_int(payload.get("dy"), "dy")
            except NumericError:
                return self._reject(E_NUM)
            if (
                abs(dx) > unit.definition.move_speed
                or abs(dy) > unit.definition.move_speed
            ):
                return self._reject(E_MOVE)
            nx = unit.x + dx
            ny = unit.y + dy
            if not (0 <= nx < self.match.width and 0 <= ny < self.match.height):
                return self._reject(E_MOVE)
            unit.x = nx
            unit.y = ny
            return None

        if event_type == "gather":
            rules = self.match.rules
            gain = rules.gather_base
            if rules.gather_variance:
                gain += derive_u64(
                    self.match.seed, self.tick, event.index, "gather:gain"
                ) % (rules.gather_variance + 1)
            unit.resource = min(unit.definition.max_resource, unit.resource + gain)
            self.rng_decisions += 1
            if (
                roll_bps(self.match.seed, self.tick, event.index, "gather:drop")
                < rules.gather_drop_bps
            ):
                unit.items["potion"] = unit.items.get("potion", 0) + rules.potion_drop
            self.rng_decisions += 1
            return None

        if event_type == "item":
            if payload.get("item") != "potion":
                return self._reject(E_ITEM)
            stock = unit.items.get("potion", 0)
            if stock <= 0:
                return self._reject(E_ITEM)
            unit.items["potion"] = stock - 1
            unit.hp = min(
                unit.definition.max_hp,
                unit.hp
                + apply_bps(
                    unit.definition.max_hp,
                    self.match.rules.potion_heal_bps,
                ),
            )
            return None

        if event_type == "skill":
            skill_id = payload.get("skill")
            target_id = payload.get("target")
            if not isinstance(skill_id, str) or not isinstance(target_id, str):
                return self._reject(E_ACTION)
            skill: SkillDef | None = unit.definition.skills.get(skill_id)
            if skill is None:
                return self._reject(E_ACTION)
            target = self.units.get(target_id)
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
            if max(abs(unit.x - target.x), abs(unit.y - target.y)) > skill.range:
                return self._reject(E_RANGE)
            unit.resource -= skill.cost
            unit.cooldowns[skill_id] = skill.cooldown
            if (
                roll_bps(
                    self.match.seed,
                    self.tick,
                    event.index,
                    f"skill:{skill_id}:hit",
                )
                >= skill.hit_bps
            ):
                self.rng_decisions += 1
                return None
            self.rng_decisions += 1
            crit = roll_bps(
                self.match.seed,
                self.tick,
                event.index,
                f"skill:{skill_id}:crit",
            )
            jitter = signed_jitter_bps(
                self.match.seed,
                self.tick,
                event.index,
                f"skill:{skill_id}:jitter",
                skill.variance_bps,
            )
            self.rng_decisions += 2
            damage = apply_bps(skill.damage, BPS + jitter)
            if crit < skill.crit_bps:
                damage = apply_bps(damage, skill.crit_multiplier_bps)
            damage = max(1, damage - target.definition.defense)
            target.hp = max(0, target.hp - damage)
            if target.hp == 0:
                target.alive = False
                target.resource = 0
                target.respawn_at = self.tick + target.definition.revive_ticks
                killer_team = unit.definition.team
                self.scores[killer_team] += self.match.rules.score_per_kill
                if self.scores[killer_team] >= self.match.rules.win_score:
                    self.result = {
                        "winner_team": killer_team,
                        "reason": "score_limit",
                        "locked_tick": self.tick,
                        "scores": dict(sorted(self.scores.items())),
                    }
            return None

        return self._reject(E_ACTION)

    def _process_events(self) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
        summaries: list[dict[str, Any]] = []
        rejected: list[dict[str, Any]] = []
        used = 0
        limit = self.event_buffer_limit
        while True:
            remaining = limit - used
            if remaining <= 0:
                peek = self.source.peek_tick()
                if peek is not None and peek <= self.tick:
                    count, first, _ = self.source.discard_for_tick(self.tick)
                    if count:
                        summary = _bulk(E_BUFFER, count, first)
                        summaries.append(summary)
                        rejected.append(summary)
                        self.rejected_total += count
                break
            chunk = self.source.take_for_tick(self.tick, remaining)
            if not chunk:
                break
            used += len(chunk)
            for event in chunk:
                if self.done:
                    code = E_FINISHED
                    self.rejected_total += 1
                else:
                    code = event.code
                    if code is None:
                        code = self._apply(event)
                    else:
                        self.rejected_total += 1
                summary = _summary(event, code)
                summaries.append(summary)
                if code is not None:
                    rejected.append(summary)
            if self.done:
                peek = self.source.peek_tick()
                if peek is not None and peek <= self.tick:
                    count, first, _ = self.source.discard_for_tick(self.tick)
                    if count:
                        summary = _bulk(E_FINISHED, count, first)
                        summaries.append(summary)
                        rejected.append(summary)
                        self.rejected_total += count
                break
        if not self.done and self.tick == self.match.tick_count - 1:
            count, first, _ = self.source.drain_remaining()
            if count:
                summary = _bulk(E_ORDER, count, first)
                summaries.append(summary)
                rejected.append(summary)
                self.rejected_total += count
        return summaries, rejected

    def _evaluate_tick_limit(self) -> None:
        ranked: dict[str, tuple[int, int, int]] = {}
        for team in self.match.team_ids:
            hp_total = 0
            resource_total = 0
            for definition in self.match.units:
                if definition.team == team:
                    unit = self.units[definition.unit_id]
                    hp_total += unit.hp
                    resource_total += unit.resource
            ranked[team] = (self.scores[team], hp_total, resource_total)
        best = max(ranked.values())
        winners = [team for team, rank in ranked.items() if rank == best]
        self.result = {
            "winner_team": winners[0] if len(winners) == 1 else None,
            "reason": "tick_limit" if len(winners) == 1 else "tick_limit_draw",
            "locked_tick": self.tick,
            "scores": dict(sorted(self.scores.items())),
        }

    def advance_one(self) -> None:
        self.tick += 1
        self._start_of_tick()
        summaries, _ = self._process_events()
        self.last_events = summaries
        self.event_offset = self.source.tell()
        if not self.done and self.tick == self.match.tick_count - 1:
            self._evaluate_tick_limit()

    def tick_record(self) -> dict[str, Any]:
        return {
            "kind": "tick",
            "tick": self.tick,
            "units": {
                definition.unit_id: self.units[definition.unit_id].tick_dict()
                for definition in self.match.units
            },
            "scores": dict(sorted(self.scores.items())),
            "result": self.result,
            "events": self.last_events,
            "event_offset": self.event_offset,
        }

    def full_snapshot(self) -> dict[str, Any]:
        return {
            "tick": self.tick,
            "units": {
                definition.unit_id: self.units[definition.unit_id].full_dict()
                for definition in self.match.units
            },
            "scores": dict(sorted(self.scores.items())),
            "result": self.result,
            "event_offset": self.event_offset,
            "rng_decisions": self.rng_decisions,
            "rejected_total": self.rejected_total,
        }

    def key_metrics(self) -> dict[str, Any]:
        return {
            "final_tick": self.tick,
            "scores": dict(sorted(self.scores.items())),
            "result": self.result,
            "event_offset": self.event_offset,
            "rng_decisions": self.rng_decisions,
            "rejected_total": self.rejected_total,
        }


def _report_diff(
    *,
    tick: int,
    path: str,
    recorded: Any,
    recomputed: Any,
    message: str = "replay simulation diverged",
) -> VerificationReport:
    return VerificationReport(
        E_DIVERGE,
        message,
        tick=tick,
        rule=path,
        recorded=recorded,
        recomputed=recomputed,
    )


def verify_replay(
    replay_path: str | Path,
    event_source: EventSource | str | Path,
) -> VerificationReport:
    """Re-simulate a replay from its initial state and event stream.

    The replay integrity pass and the rule replay are separate passes.  On a
    semantic mismatch the earliest tick and field path are returned.
    """

    try:
        meta = load_and_validate_replay(replay_path)
    except TickArenaError as exc:
        return VerificationReport(exc.code, exc.message)

    owns_source = not isinstance(event_source, EventSource)
    source: EventSource
    try:
        source = (
            JsonlEventSource(event_source)
            if owns_source
            else event_source
        )
    except TickArenaError as exc:
        return VerificationReport(E_REPLAY, f"cannot load event stream: {exc.message}")

    try:
        header = meta.header
        if source.fingerprint != header.get("event_fingerprint"):
            return VerificationReport(
                E_REPLAY,
                "event stream fingerprint does not match replay header",
            )
        try:
            match = parse_initial_state(header["initial_state"])
        except TickArenaError as exc:
            return VerificationReport(E_REPLAY, f"invalid replay initial state: {exc.message}")
        if source.initial_fingerprint != match.initial_fingerprint:
            return VerificationReport(
                E_REPLAY,
                "event stream initial state does not match replay initial state",
            )
        if meta.footer.get("final_tick") is None:
            return VerificationReport(E_REPLAY, "replay final tick is missing")

        verifier = _Verifier(match, source)
        verifier.event_buffer_limit = require_int(
            header.get("event_buffer_limit"),
            "event_buffer_limit",
            minimum=1,
        )
        source.reset()
        tick_records = 0
        seen_tick = -1
        try:
            body = iter_replay_body(meta.path)
            for record in body:
                kind = record.get("kind")
                if kind == "tick":
                    tick = record.get("tick")
                    if isinstance(tick, bool) or not isinstance(tick, int):
                        return VerificationReport(E_REPLAY, "tick record has invalid tick")
                    if verifier.done:
                        return _report_diff(
                            tick=tick,
                            path="tick",
                            recorded=record,
                            recomputed="match already finished",
                            message="extra tick record after locked result",
                        )
                    expected = verifier.tick + 1
                    if tick != expected:
                        return _report_diff(
                            tick=expected,
                            path="tick",
                            recorded=tick,
                            recomputed=expected,
                            message="replay tick sequence is not contiguous",
                        )
                    verifier.advance_one()
                    expected_record = verifier.tick_record()
                    diff = first_difference(record, expected_record)
                    if diff is not None:
                        return _report_diff(
                            tick=tick,
                            path=diff.path,
                            recorded=diff.recorded,
                            recomputed=diff.recomputed,
                        )
                    tick_records += 1
                    seen_tick = tick
                elif kind == "snapshot":
                    tick = record.get("tick")
                    if isinstance(tick, bool) or not isinstance(tick, int):
                        return VerificationReport(E_REPLAY, "snapshot has invalid tick")
                    if tick != verifier.tick:
                        return _report_diff(
                            tick=max(0, tick),
                            path="snapshot.tick",
                            recorded=tick,
                            recomputed=verifier.tick,
                        )
                    diff = first_difference(record, {
                        "kind": "snapshot",
                        **verifier.full_snapshot(),
                    })
                    if diff is not None:
                        return _report_diff(
                            tick=tick,
                            path=diff.path,
                            recorded=diff.recorded,
                            recomputed=diff.recomputed,
                            message="snapshot state diverged",
                        )
                else:
                    return VerificationReport(E_REPLAY, f"unknown replay record kind {kind!r}")
        except (OSError, ValueError, KeyError, TypeError) as exc:
            return VerificationReport(E_REPLAY, f"invalid replay body: {exc}")

        if not verifier.done:
            return _report_diff(
                tick=verifier.tick + 1,
                path="tick",
                recorded=None,
                recomputed=verifier.full_snapshot(),
                message="replay ended before a locked result",
            )
        footer = meta.footer
        final_tick = footer.get("final_tick")
        if final_tick != verifier.tick:
            return _report_diff(
                tick=verifier.tick,
                path="final_tick",
                recorded=final_tick,
                recomputed=verifier.tick,
            )
        final_diff = first_difference(
            footer.get("final_state"),
            verifier.full_snapshot(),
        )
        if final_diff is not None:
            return _report_diff(
                tick=verifier.tick,
                path=f"final_state.{final_diff.path}",
                recorded=final_diff.recorded,
                recomputed=final_diff.recomputed,
                message="final state diverged",
            )
        metrics_diff = first_difference(
            footer.get("key_metrics"),
            verifier.key_metrics(),
        )
        if metrics_diff is not None:
            return _report_diff(
                tick=verifier.tick,
                path=f"key_metrics.{metrics_diff.path}",
                recorded=metrics_diff.recorded,
                recomputed=metrics_diff.recomputed,
                message="key metrics diverged",
            )
        if tick_records != verifier.tick + 1:
            return VerificationReport(
                E_REPLAY,
                f"replay has {tick_records} tick records for final tick {seen_tick}",
            )
        return VerificationReport(
            E_OK,
            "replay verified",
            tick=verifier.tick,
            final_state=verifier.full_snapshot(),
        )
    finally:
        if owns_source:
            source.close()

