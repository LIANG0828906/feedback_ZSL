"""Four-phase deterministic level generator with rollback traces."""

from __future__ import annotations

import copy
import hashlib
from dataclasses import dataclass, field
from typing import Any, Iterable

from .errors import (
    E_CANCELLED,
    E_CONSTRAINT_CONFLICT,
    E_GENERATION_FAILED,
    WorkbenchError,
)
from .evaluator import EvaluationBundle, evaluate_constraints
from .models import Door, Level, Room, Tag, WorkbenchConfig
from .reachability import structural_connectivity
from .rng import DeterministicRandom, RandomSource, derive_seed
from .trace import TraceBuilder
from .util import content_hash, sha256_text


PHASES = ("room_layout", "door_assignment", "tag_placement", "key_chain")


@dataclass
class _State:
    rooms: list[Room] = field(default_factory=list)
    doors: list[Door] = field(default_factory=list)
    tags: list[Tag] = field(default_factory=list)
    segments: list[list[str]] = field(default_factory=list)

    def level(self, config: WorkbenchConfig) -> Level:
        return Level(
            grid_width=config.grid_width,
            grid_height=config.grid_height,
            rooms=tuple(self.rooms),
            doors=tuple(self.doors),
            tags=tuple(self.tags),
        )

    def content_hash(self, config: WorkbenchConfig) -> str:
        return content_hash(self.level(config).to_dict())


@dataclass
class _PhaseFailure(Exception):
    stage: str
    failure_id: str
    reason: str
    constraint_ids: list[str]
    minimal_evidence: dict[str, Any]
    checks: list[dict[str, Any]]


@dataclass
class GenerationResult:
    level: Level
    trace: dict[str, Any]
    evaluation: EvaluationBundle
    attribution: dict[str, Any]
    layout_hash: str
    attribution_hash: str
    seed_derivation_hash: str
    attempts: int
    stage_hashes: dict[str, str]

    @property
    def trace_hash(self) -> str:
        return str(self.trace["meta"]["trace_hash"])

    def summary(self) -> dict[str, Any]:
        return {
            "ok": True,
            "layout_hash": self.layout_hash,
            "trace_hash": self.trace_hash,
            "seed_derivation_hash": self.seed_derivation_hash,
            "attempts": self.attempts,
            "stage_hashes": self.stage_hashes,
            "hard_passed": not self.evaluation.failed_hard,
            "failed_hard": list(self.evaluation.failed_hard),
            "failed_soft": list(self.evaluation.failed_soft),
            "uncertain": list(self.evaluation.uncertain),
            "error_report": self.evaluation.error_report,
            "path": list(self.evaluation.reachability.path),
            "unlock_order": list(self.evaluation.reachability.unlock_order),
            "rooms": len(self.level.rooms),
            "doors": len(self.level.doors),
            "tags": len(self.level.tags),
        }


def _is_cancelled(cancel_event: Any | None) -> bool:
    if cancel_event is None:
        return False
    if callable(cancel_event):
        return bool(cancel_event())
    if hasattr(cancel_event, "is_set"):
        return bool(cancel_event.is_set())
    return False


def _raise_cancelled(stage: str) -> None:
    raise WorkbenchError(
        E_CANCELLED,
        "generation was cancelled; no level or trace was committed",
        stage=stage,
    )


def _phase_id(stage: str, attempt: int, retry: int) -> str:
    return f"{stage}/attempt={attempt}/retry={retry}"


def _draw_name(attempt: int, retry: int, name: str) -> str:
    return f"{name}.a{attempt}.r{retry}"


class DeterministicLevelGenerator:
    def __init__(
        self,
        config: WorkbenchConfig,
        *,
        random_source: RandomSource,
        trace: TraceBuilder,
        cancel_event: Any | None = None,
    ) -> None:
        self.config = config
        self.rng = random_source
        self.trace = trace
        self.cancel_event = cancel_event
        self.keychain = config.primary_keychain()
        self.keys = self.keychain.keys if self.keychain is not None else ()

    def generate(self) -> GenerationResult:
        self._preflight()
        initial_rng = self.rng.snapshot()
        last_failure: _PhaseFailure | None = None
        for attempt in range(self.config.max_attempts):
            if _is_cancelled(self.cancel_event):
                _raise_cancelled(last_failure.stage if last_failure else "room_layout")
            self.rng.restore(initial_rng)
            for nonce_index in range(attempt):
                self.rng.randint(
                    "room_layout",
                    f"attempt_nonce.{nonce_index}",
                    0,
                    (1 << 31) - 1,
                )
            state = _State()
            stage_hashes: dict[str, str] = {}
            attempt_failed = False
            for phase_index, stage in enumerate(PHASES):
                entrance_state = copy.deepcopy(state)
                entrance_rng = self.rng.snapshot()
                phase_passed = False
                failure: _PhaseFailure | None = None
                for retry in range(self.config.max_phase_retries):
                    if _is_cancelled(self.cancel_event):
                        _raise_cancelled(stage)
                    for nonce_index in range(retry):
                        self.rng.randint(
                            stage,
                            f"phase_nonce.{retry}.{nonce_index}",
                            0,
                            (1 << 31) - 1,
                        )
                    phase_start_hash = state.content_hash(self.config)
                    self.trace.add(
                        stage,
                        "phase_start",
                        {
                            "phase": stage,
                            "attempt": attempt,
                            "retry": retry,
                            "entrance_hash": phase_start_hash,
                        },
                    )
                    try:
                        state, checks, evaluation = self._run_phase(
                            stage,
                            state,
                            attempt,
                            retry,
                        )
                        phase_hash = state.content_hash(self.config)
                        stage_hashes[stage] = phase_hash
                        self.trace.add(
                            stage,
                            "constraint_check",
                            {
                                "phase": stage,
                                "attempt": attempt,
                                "retry": retry,
                                "checks": checks,
                                "quantization": self.config.to_dict().get("quantization", {}),
                            },
                        )
                        self.trace.add(
                            stage,
                            "phase_complete",
                            {
                                "phase": stage,
                                "attempt": attempt,
                                "retry": retry,
                                "content_hash": phase_hash,
                            },
                        )
                        phase_passed = True
                        if stage == "key_chain":
                            final_evaluation = evaluation
                        break
                    except _PhaseFailure as exc:
                        failure = exc
                        last_failure = exc
                        self.trace.add(
                            stage,
                            "phase_failure",
                            {
                                "phase": stage,
                                "attempt": attempt,
                                "retry": retry,
                                "failure_id": exc.failure_id,
                                "reason": exc.reason,
                                "violated_constraints": exc.constraint_ids,
                                "minimal_evidence": exc.minimal_evidence,
                                "checks": exc.checks,
                            },
                        )
                        state = copy.deepcopy(entrance_state)
                        self.rng.restore(entrance_rng)
                        self.trace.add(
                            stage,
                            "phase_rollback",
                            {
                                "phase": stage,
                                "attempt": attempt,
                                "retry": retry,
                                "restored_hash": state.content_hash(self.config),
                            },
                        )
                if not phase_passed:
                    attempt_failed = True
                    self.trace.add(
                        stage,
                        "attempt_rollback",
                        {
                            "attempt": attempt,
                            "failed_stage": stage,
                            "failure_id": failure.failure_id if failure else None,
                        },
                    )
                    break
            if attempt_failed:
                continue
            if _is_cancelled(self.cancel_event):
                _raise_cancelled("final")
            return self._finalize(state, evaluation, stage_hashes, attempt + 1)
        self._raise_exhausted(last_failure)
        raise AssertionError("unreachable")

    def _run_phase(
        self,
        stage: str,
        state: _State,
        attempt: int,
        retry: int,
    ) -> tuple[_State, list[dict[str, Any]], EvaluationBundle | None]:
        if stage == "room_layout":
            self._phase_room_layout(state, attempt, retry)
            checks = self._check_room_layout(state)
            if any(not item["passed"] for item in checks):
                raise self._failure_from_checks(
                    stage,
                    attempt,
                    retry,
                    "room layout violates a hard room-count constraint",
                    checks,
                )
            return state, checks, None
        if stage == "door_assignment":
            self._phase_door_assignment(state, attempt, retry)
            checks = self._check_doors(state)
            if any(not item["passed"] for item in checks):
                raise self._failure_from_checks(
                    stage,
                    attempt,
                    retry,
                    "door assignment is structurally disconnected",
                    checks,
                )
            return state, checks, None
        if stage == "tag_placement":
            self._phase_tag_placement(state, attempt, retry)
            checks = self._check_tags(state)
            if any(not item["passed"] for item in checks):
                raise self._failure_from_checks(
                    stage,
                    attempt,
                    retry,
                    "tag placement is incomplete",
                    checks,
                )
            return state, checks, None
        if stage == "key_chain":
            level = state.level(self.config)
            evaluation = evaluate_constraints(level, self.config)
            checks = self._compact_evaluation(evaluation)
            if evaluation.failed_hard:
                if self.config.uncertain_policy == "fail" and evaluation.uncertain:
                    reason = "hard constraint evaluation is uncertain under quantization"
                else:
                    reason = "one or more hard constraints are false"
                raise _PhaseFailure(
                    stage=stage,
                    failure_id=_phase_id(stage, attempt, retry),
                    reason=reason,
                    constraint_ids=list(evaluation.failed_hard),
                    minimal_evidence=evaluation.to_dict()["minimal_evidence"],
                    checks=checks,
                )
            return state, checks, evaluation
        raise AssertionError(f"unknown phase: {stage}")

    def _preflight(self) -> None:
        minimum, maximum = self.config.room_count_bounds()
        required = len(self.keys) + 1
        if maximum < required:
            room_constraints = [item.id for item in self.config.constraints_of_type("room_count")]
            key_constraints = [self.keychain.id] if self.keychain is not None else []
            raise WorkbenchError(
                E_CONSTRAINT_CONFLICT,
                "key chain needs more rooms than the room-count maximum allows",
                stage="room_layout",
                details={
                    "failed_stage": "room_layout",
                    "violated_constraints": room_constraints + key_constraints,
                    "minimal_evidence": {
                        "constraints": room_constraints + key_constraints,
                        "facts": [
                            f"key_count={len(self.keys)}",
                            f"required_rooms={required}",
                            f"room_count_max={maximum}",
                        ],
                        "sufficient": True,
                    },
                },
            )
        footprint = (
            (self.config.room_min_width + self.config.room_margin)
            * (self.config.room_min_height + self.config.room_margin)
            * required
        )
        if footprint > self.config.grid_width * self.config.grid_height:
            room_constraints = [item.id for item in self.config.constraints_of_type("room_count")]
            key_constraints = [self.keychain.id] if self.keychain is not None else []
            raise WorkbenchError(
                E_CONSTRAINT_CONFLICT,
                "grid cannot fit the minimum room count needed by the key chain",
                stage="room_layout",
                details={
                    "failed_stage": "room_layout",
                    "violated_constraints": room_constraints + key_constraints,
                    "minimal_evidence": {
                        "constraints": room_constraints + key_constraints,
                        "facts": [
                            f"required_rooms={required}",
                            f"minimum_footprint={footprint}",
                            f"grid_area={self.config.grid_width * self.config.grid_height}",
                        ],
                        "sufficient": True,
                    },
                },
            )

    def _phase_room_layout(self, state: _State, attempt: int, retry: int) -> None:
        minimum, maximum = self.config.room_count_bounds()
        required = len(self.keys) + 1
        low = max(minimum, required)
        if low > maximum:
            raise self._failure_from_checks(
                "room_layout",
                attempt,
                retry,
                "room-count range cannot host the required key chain",
                [
                    {
                        "constraint_id": self.keychain.id if self.keychain else "key_chain",
                        "passed": False,
                        "reason": "required room count exceeds maximum",
                    }
                ],
            )
        room_count = self.rng.randint(
            "room_layout",
            _draw_name(attempt, retry, "room_count"),
            low,
            maximum,
        )
        segment_count = len(self.keys) + 1
        sizes = [1] * segment_count
        for index in range(room_count - segment_count):
            target = self.rng.randint(
                "room_layout",
                _draw_name(attempt, retry, f"segment_extra_{index}"),
                0,
                segment_count - 1,
            )
            sizes[target] += 1

        placed: list[Room] = []
        for room_index in range(room_count):
            room = self._place_room(placed, room_index, attempt, retry)
            if room is None:
                checks = self._check_room_layout(_State(rooms=placed))
                raise self._failure_from_checks(
                    "room_layout",
                    attempt,
                    retry,
                    f"could not place room {room_index} without overlap",
                    checks,
                )
            placed.append(room)
        state.rooms = placed
        room_ids = [room.id for room in placed]
        self.rng.shuffle(
            "room_layout",
            _draw_name(attempt, retry, "room_order"),
            room_ids,
        )
        segments: list[list[str]] = []
        cursor = 0
        for size in sizes:
            segments.append(room_ids[cursor : cursor + size])
            cursor += size
        state.segments = segments

    def _place_room(
        self,
        placed: list[Room],
        room_index: int,
        attempt: int,
        retry: int,
    ) -> Room | None:
        config = self.config
        room_id = f"r{room_index}"
        for placement_attempt in range(320):
            width = self.rng.randint(
                "room_layout",
                _draw_name(attempt, retry, f"room.{room_index}.w.{placement_attempt}"),
                config.room_min_width,
                config.room_max_width,
            )
            height = self.rng.randint(
                "room_layout",
                _draw_name(attempt, retry, f"room.{room_index}.h.{placement_attempt}"),
                config.room_min_height,
                config.room_max_height,
            )
            x = self.rng.randint(
                "room_layout",
                _draw_name(attempt, retry, f"room.{room_index}.x.{placement_attempt}"),
                0,
                config.grid_width - width,
            )
            y = self.rng.randint(
                "room_layout",
                _draw_name(attempt, retry, f"room.{room_index}.y.{placement_attempt}"),
                0,
                config.grid_height - height,
            )
            candidate = Room(room_id, x, y, width, height)
            if self._room_fits(candidate, placed):
                return candidate
        for width in range(config.room_max_width, config.room_min_width - 1, -1):
            for height in range(config.room_max_height, config.room_min_height - 1, -1):
                if width > config.grid_width or height > config.grid_height:
                    continue
                for y in range(0, config.grid_height - height + 1):
                    for x in range(0, config.grid_width - width + 1):
                        candidate = Room(room_id, x, y, width, height)
                        if self._room_fits(candidate, placed):
                            return candidate
        return None

    def _room_fits(self, candidate: Room, placed: list[Room]) -> bool:
        margin = self.config.room_margin
        return not any(
            candidate.contains(room, margin) or room.contains(candidate, margin)
            for room in placed
        )

    def _phase_door_assignment(self, state: _State, attempt: int, retry: int) -> None:
        config = self.config
        doors: list[Door] = []
        used_pairs: set[tuple[str, str]] = set()
        door_index = 0

        def add_door(room_a: str, room_b: str, *, locked_by: str | None, mixed: bool) -> None:
            nonlocal door_index
            pair = tuple(sorted((room_a, room_b)))
            if pair in used_pairs:
                return
            used_pairs.add(pair)
            if locked_by is not None:
                direction = "both"
            elif mixed and config.direction_mode == "mixed":
                direction = self.rng.choice(
                    "door_assignment",
                    _draw_name(attempt, retry, f"door.{door_index}.direction"),
                    ("both", "forward", "reverse"),
                )
            else:
                direction = "both"
            doors.append(
                Door(
                    id=f"d{door_index}",
                    room_a=room_a,
                    room_b=room_b,
                    direction=direction,
                    locked_by=locked_by,
                )
            )
            door_index += 1

        for segment in state.segments:
            for left, right in zip(segment, segment[1:]):
                add_door(left, right, locked_by=None, mixed=False)
        for index, key in enumerate(self.keys):
            left = state.segments[index][-1]
            right = state.segments[index + 1][0]
            add_door(left, right, locked_by=key, mixed=False)

        target = self._door_target()
        candidates: list[tuple[str, str]] = []
        for segment in state.segments:
            for left_index, left in enumerate(segment):
                for right in segment[left_index + 1 :]:
                    if tuple(sorted((left, right))) not in used_pairs:
                        candidates.append((left, right))
        self.rng.shuffle(
            "door_assignment",
            _draw_name(attempt, retry, "extra_candidates"),
            candidates,
        )
        for room_a, room_b in candidates:
            if target is not None and len(doors) >= target:
                break
            add_door(room_a, room_b, locked_by=None, mixed=True)
        state.doors = doors

    def _door_target(self) -> int | None:
        constraints = self.config.constraints_of_type("door_count_target")
        if not constraints:
            return None
        return max(int(round(float(item.params["target"]))) for item in constraints)

    def _phase_tag_placement(self, state: _State, attempt: int, retry: int) -> None:
        tags: list[Tag] = []
        first_segment = state.segments[0]
        last_segment = state.segments[-1]
        start_room = first_segment[0]
        end_room = last_segment[-1]
        tags.append(Tag("tag_start", "start", start_room))
        tags.append(Tag("tag_end", "end", end_room))

        for index, key in enumerate(self.keys):
            room_id = self.rng.choice(
                "tag_placement",
                _draw_name(attempt, retry, f"key.{index}.room"),
                state.segments[index],
            )
            tags.append(Tag(f"tag_key_{index}", "key", room_id, key_id=key))

        locked = [door for door in state.doors if door.locked_by is not None]
        for index, door in enumerate(locked):
            room_id = state.segments[index][-1]
            tags.append(
                Tag(
                    f"tag_lock_{index}",
                    "lock",
                    room_id,
                    key_id=door.locked_by,
                    door_id=door.id,
                )
            )

        reward_candidates = [
            room.id for room in state.rooms if room.id not in (start_room, end_room)
        ]
        self.rng.shuffle(
            "tag_placement",
            _draw_name(attempt, retry, "reward_order"),
            reward_candidates,
        )
        if not reward_candidates:
            reward_candidates = [start_room]
        for index in range(self.config.rewards):
            room_id = reward_candidates[index % len(reward_candidates)]
            tags.append(Tag(f"tag_reward_{index}", "reward", room_id))
        state.tags = tags

    def _check_room_layout(self, state: _State) -> list[dict[str, Any]]:
        checks: list[dict[str, Any]] = []
        for constraint in self.config.constraints_of_type("room_count"):
            minimum = int(constraint.params["min"])
            maximum = int(constraint.params["max"])
            actual = len(state.rooms)
            checks.append(
                {
                    "constraint_id": constraint.id,
                    "type": constraint.type,
                    "hard": constraint.hard,
                    "passed": minimum <= actual <= maximum,
                    "observed": actual,
                    "min": minimum,
                    "max": maximum,
                }
            )
        if not checks:
            checks.append(
                {
                    "constraint_id": "implicit_room_count",
                    "type": "room_count",
                    "hard": True,
                    "passed": bool(state.rooms),
                    "observed": len(state.rooms),
                }
            )
        return checks

    def _check_doors(self, state: _State) -> list[dict[str, Any]]:
        level = state.level(self.config)
        connected, missing = structural_connectivity(level)
        constraint_id = (
            self.config.constraints_of_type("connectivity")[0].id
            if self.config.constraints_of_type("connectivity")
            else "implicit_connectivity"
        )
        return [
            {
                "constraint_id": constraint_id,
                "type": "connectivity",
                "hard": True,
                "passed": connected,
                "missing_rooms": list(missing),
                "doors": len(state.doors),
            }
        ]

    def _check_tags(self, state: _State) -> list[dict[str, Any]]:
        starts = [tag for tag in state.tags if tag.kind == "start"]
        ends = [tag for tag in state.tags if tag.kind == "end"]
        key_tags = {tag.key_id for tag in state.tags if tag.kind == "key"}
        lock_tags = [tag for tag in state.tags if tag.kind == "lock"]
        locked_doors = [door for door in state.doors if door.locked_by is not None]
        tag_rooms = {room.id for room in state.rooms}
        dangling = [tag.id for tag in state.tags if tag.room_id not in tag_rooms]
        required = set(self.keys)
        keychain_id = self.keychain.id if self.keychain is not None else "key_chain"
        checks = [
            {
                "constraint_id": keychain_id,
                "type": "tag_integrity",
                "hard": True,
                "passed": len(starts) == 1 and len(ends) == 1 and not dangling,
                "start_tags": len(starts),
                "end_tags": len(ends),
                "dangling_tags": dangling,
            },
            {
                "constraint_id": keychain_id,
                "type": "key_tag_integrity",
                "hard": True,
                "passed": required.issubset(key_tags),
                "required_keys": sorted(required),
                "key_tags": sorted(item for item in key_tags if item is not None),
            },
            {
                "constraint_id": keychain_id,
                "type": "lock_tag_integrity",
                "hard": True,
                "passed": {
                    (tag.door_id, tag.key_id) for tag in lock_tags
                }
                >= {(door.id, door.locked_by) for door in locked_doors},
                "locked_doors": [door.id for door in locked_doors],
                "lock_tags": [tag.id for tag in lock_tags],
            },
            {
                "constraint_id": "reward_labels",
                "type": "reward_integrity",
                "hard": False,
                "passed": len([tag for tag in state.tags if tag.kind == "reward"])
                >= self.config.rewards,
                "rewards": len([tag for tag in state.tags if tag.kind == "reward"]),
                "required_rewards": self.config.rewards,
            },
        ]
        return checks

    def _compact_evaluation(self, evaluation: EvaluationBundle) -> list[dict[str, Any]]:
        checks = []
        for constraint_id, result in evaluation.results.items():
            checks.append(
                {
                    "constraint_id": constraint_id,
                    "type": result["type"],
                    "hard": result["hard"],
                    "passed": result["passed"],
                    "fixed_truth": result.get("fixed_truth"),
                    "float_truth": result.get("float_truth"),
                    "truth_lower": result.get("truth_lower"),
                    "truth_upper": result.get("truth_upper"),
                    "uncertain": result.get("uncertain", False),
                    "attribution": result.get("attribution"),
                }
            )
        return checks

    def _failure_from_checks(
        self,
        stage: str,
        attempt: int,
        retry: int,
        reason: str,
        checks: list[dict[str, Any]],
    ) -> _PhaseFailure:
        failed = [
            check
            for check in checks
            if not check.get("passed") and check.get("hard", True)
        ]
        ids = sorted({str(check["constraint_id"]) for check in failed})
        facts = []
        for check in failed:
            facts.append(
                {
                    "constraint_id": check["constraint_id"],
                    "type": check.get("type"),
                    "facts": [
                        f"{key}={value}"
                        for key, value in sorted(check.items())
                        if key not in ("constraint_id", "type", "passed", "hard")
                    ],
                }
            )
        return _PhaseFailure(
            stage=stage,
            failure_id=_phase_id(stage, attempt, retry),
            reason=reason,
            constraint_ids=ids,
            minimal_evidence={
                "constraints": ids,
                "facts": facts,
                "sufficient": True,
            },
            checks=checks,
        )

    def _finalize(
        self,
        state: _State,
        evaluation: EvaluationBundle | None,
        stage_hashes: dict[str, str],
        attempts: int,
    ) -> GenerationResult:
        if evaluation is None:
            raise AssertionError("final evaluation is missing")
        level = state.level(self.config)
        layout_hash = content_hash(level.to_dict())
        attribution = {
            "generator_version": sha256_text("levelworkbench-generator/1")[:16],
            "seed": self.config.seed,
            "seed_derivation_hash": self.trace.meta["seed_derivation_hash"],
            "params_hash": self.trace.meta["params_hash"],
            "config_hash": self.trace.meta["config_hash"],
            "attempts": attempts,
            "layout_hash": layout_hash,
            "stage_hashes": dict(stage_hashes),
            "evaluation": evaluation.to_dict(),
            "failure": None,
        }
        attribution_hash = content_hash(attribution)
        self.trace.meta["draw_count"] = len(
            [event for event in self.trace.events if event.get("kind") == "random_draw"]
        )
        trace_document = self.trace.finish(
            layout_hash=layout_hash,
            attribution_hash=attribution_hash,
            attribution=attribution,
        )
        return GenerationResult(
            level=level,
            trace=trace_document,
            evaluation=evaluation,
            attribution=attribution,
            layout_hash=layout_hash,
            attribution_hash=attribution_hash,
            seed_derivation_hash=self.trace.meta["seed_derivation_hash"],
            attempts=attempts,
            stage_hashes=stage_hashes,
        )

    def _raise_exhausted(self, failure: _PhaseFailure | None) -> None:
        details: dict[str, Any] = {
            "failed_stage": failure.stage if failure else "room_layout",
            "violated_constraints": failure.constraint_ids if failure else [],
            "minimal_evidence": failure.minimal_evidence if failure else None,
            "failure_id": failure.failure_id if failure else None,
            "attempts": self.config.max_attempts,
            "reason": failure.reason if failure else None,
            "checks": failure.checks if failure else [],
        }
        raise WorkbenchError(
            E_GENERATION_FAILED,
            failure.reason if failure else "generation attempts were exhausted",
            stage=failure.stage if failure else "room_layout",
            details=details,
        )


def build_generator_trace(config: WorkbenchConfig, random_source: RandomSource) -> Any:
    params = {
        "phases": list(PHASES),
        "generation": config.to_dict().get("generation", {}),
        "quantization": config.to_dict().get("quantization", {}),
        "evaluation": config.to_dict().get("evaluation", {}),
    }
    constraints = [item.to_dict() for item in config.constraints]
    derived_seed = derive_seed(config.seed, constraints, params)
    seed_derivation_hash = hashlib.sha256(str(derived_seed).encode("ascii")).hexdigest()
    trace = TraceBuilder(
        config,
        seed_derivation_hash=seed_derivation_hash,
        params_hash=content_hash(params),
    )
    return trace, derived_seed


def generate_level(
    config: WorkbenchConfig,
    *,
    cancel_event: Any | None = None,
    replay_draws: list[dict[str, Any]] | None = None,
) -> GenerationResult:
    from .rng import ReplayRandom

    if replay_draws is None:
        params = {
            "phases": list(PHASES),
            "generation": config.to_dict().get("generation", {}),
            "quantization": config.to_dict().get("quantization", {}),
            "evaluation": config.to_dict().get("evaluation", {}),
        }
        trace, derived_seed = build_generator_trace(config, None)  # type: ignore[arg-type]
        random_source: RandomSource = DeterministicRandom(derived_seed, trace)
    else:
        trace, _derived_seed = build_generator_trace(config, None)  # type: ignore[arg-type]
        random_source = ReplayRandom(replay_draws, trace)
    generator = DeterministicLevelGenerator(
        config,
        random_source=random_source,
        trace=trace,
        cancel_event=cancel_event,
    )
    return generator.generate()
