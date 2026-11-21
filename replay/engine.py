"""Fixed-step replay engine for the fixed-point and binary64 paths."""

from __future__ import annotations

import math
from dataclasses import dataclass
from decimal import Decimal
from typing import Any, Callable

from .error_model import ErrorTracker
from .errors import E_COORD_RANGE, E_MODE_INVALID, fail
from .model import (
    STATE_MODEL_VERSION,
    ZERO_HASH,
    EntityState,
    Operation,
    Recording,
    RoomState,
    chain_hash,
    decimal_string,
    state_hash,
    state_to_dict,
)
from .numeric import PI, VelocityCache, normalize_angle, quantize
from .snapshot import Snapshot, SnapshotStore


VALID_MODES = ("fixed", "float")


@dataclass
class StepResult:
    step: int
    mode: str
    state_hash: str
    chain_hash: str
    chain_prefix: str
    state: dict[str, Any]
    room: RoomState
    entities: dict[str, EntityState]
    effective_ops: list[dict[str, Any]]
    error_summary: dict[str, Any]
    untrusted: list[dict[str, Any]]
    cache_hits: int

    def trajectory_record(self) -> dict[str, Any]:
        return {
            "step": self.step,
            "mode": self.mode,
            "effective_ops": self.effective_ops,
            "state_hash": self.state_hash,
            "chain_hash": self.chain_hash,
            "cache_hit": self.cache_hits > 0,
            "cache_hits": self.cache_hits,
            "state": self.state,
            "error_bounds": self.error_summary,
            "untrusted": self.untrusted,
        }


@dataclass
class RunResult:
    recording: Recording
    mode: str
    steps: list[StepResult]
    final_room: RoomState
    final_entities: dict[str, EntityState]
    final_state_hash: str
    final_chain_hash: str
    canceled: bool
    last_committed_step: int
    resumed_from: int
    cache_stats: dict[str, int]
    config_digest: str

    def trajectory_sha256(self) -> str:
        import hashlib

        lines = [serialize_log_record(step.trajectory_record()) for step in self.steps]
        payload = ("\n".join(lines) + ("\n" if lines else "")).encode("ascii")
        return hashlib.sha256(payload).hexdigest()

    def summary(self) -> dict[str, Any]:
        untrusted = [
            entry
            for step in self.steps
            for entry in step.untrusted
        ]
        return {
            "ok": True,
            "match_id": self.recording.match_id,
            "mode": self.mode,
            "state_model_version": STATE_MODEL_VERSION,
            "config_digest": self.config_digest,
            "source_version": self.recording.source_version,
            "steps_advanced": len(self.steps),
            "last_step": self.steps[-1].step if self.steps else self.resumed_from,
            "resumed_from": self.resumed_from,
            "canceled": self.canceled,
            "last_committed_step": self.last_committed_step,
            "final_state_hash": self.final_state_hash,
            "final_chain_hash": self.final_chain_hash,
            "cache_stats": self.cache_stats,
            "first_untrusted_step": untrusted[0]["step"] if untrusted else None,
            "untrusted_count": len(untrusted),
        }


class ReplayEngine:
    def __init__(
        self,
        recording: Recording,
        mode: str,
        *,
        cache: VelocityCache | None = None,
        snapshot_store: SnapshotStore | None = None,
        cancel_check: Callable[[], bool] | None = None,
        cancel_after: int | None = None,
    ) -> None:
        if mode not in VALID_MODES:
            fail(
                E_MODE_INVALID,
                "mode must be 'fixed' or 'float'",
                "mode",
                details={"allowed": list(VALID_MODES), "found": mode},
            )
        self.recording = recording
        self.mode = mode
        self.cache = cache or VelocityCache()
        self.snapshot_store = snapshot_store
        self.cancel_check = cancel_check
        self.cancel_after = cancel_after

        if snapshot_store is not None:
            if snapshot_store.match_id != recording.match_id:
                fail(E_MODE_INVALID, "snapshot store match id mismatch", "snapshot.match_id")
            if snapshot_store.mode != mode:
                fail(E_MODE_INVALID, "snapshot store mode mismatch", "snapshot.mode")
            if snapshot_store.config_digest != recording.config_digest():
                fail(
                    E_MODE_INVALID,
                    "snapshot store configuration digest mismatch",
                    "snapshot.config_digest",
                )

    def _cancelled(self, next_step: int) -> bool:
        if self.cancel_after is not None and next_step > self.cancel_after:
            return True
        if self.cancel_check is not None and self.cancel_check():
            return True
        return False

    def run(self) -> RunResult:
        if self.snapshot_store is not None:
            existing = self.snapshot_store.snapshot_files()
            if existing:
                fail(
                    "E_RECOVERY_INVALID",
                    "snapshot directory already contains steps; use resume",
                    "snapshot",
                    step=existing[0][0],
                    details={"first_existing_step": existing[0][0]},
                )
        room = self.recording.room.clone(numeric=self.mode)
        entities = {
            entity_id: entity.clone(numeric=self.mode)
            for entity_id, entity in self.recording.entities.items()
        }
        tracker = ErrorTracker(
            param_places=self.recording.config.fixed_param_decimals,
            accum_places=self.recording.config.fixed_accum_decimals,
            flip_threshold=self.recording.config.flip_threshold,
            resource_threshold=self.recording.config.resource_diff_threshold,
        )
        return self._advance(
            start_step=0,
            room=room,
            entities=entities,
            chain_prefix=ZERO_HASH,
            tracker=tracker,
            resumed_from=0,
        )

    def resume(self, snapshot: Snapshot) -> RunResult:
        if snapshot.mode != self.mode:
            fail(E_MODE_INVALID, "snapshot numeric mode mismatch", "snapshot.mode")
        if snapshot.step > self.recording.config.total_steps:
            fail(
                "E_RECOVERY_INVALID",
                "snapshot step exceeds recording length",
                "snapshot.step",
                step=snapshot.step,
            )
        tracker = ErrorTracker.from_snapshot_state(snapshot.error_state)
        return self._advance(
            start_step=snapshot.step,
            room=snapshot.room.clone(numeric=self.mode),
            entities={
                entity_id: entity.clone(numeric=self.mode)
                for entity_id, entity in snapshot.entities.items()
            },
            chain_prefix=snapshot.chain_hash,
            tracker=tracker,
            resumed_from=snapshot.step,
        )

    def _advance(
        self,
        *,
        start_step: int,
        room: RoomState,
        entities: dict[str, EntityState],
        chain_prefix: str,
        tracker: ErrorTracker,
        resumed_from: int,
    ) -> RunResult:
        total_steps = self.recording.config.total_steps
        steps: list[StepResult] = []
        current_chain = chain_prefix
        last_committed = start_step
        canceled = False
        final_state_digest = ""

        for step in range(start_step + 1, total_steps + 1):
            if self._cancelled(step):
                canceled = True
                break
            step_ops = self.recording.operations_by_step.get(step, [])
            op_records: list[dict[str, Any]] = []
            cache_hits = 0
            if self.mode == "fixed":
                for op in step_ops:
                    record, hit = self._apply_fixed(op, step, room, entities, tracker)
                    op_records.append(record)
                    cache_hits += int(hit)
            else:
                for op in step_ops:
                    record, hit = self._apply_float(op, step, room, entities)
                    op_records.append(record)
                    cache_hits += int(hit)

            digest = state_hash(
                step,
                room,
                entities,
                numeric=self.mode,
                version=STATE_MODEL_VERSION,
            )
            current_chain = chain_hash(current_chain, digest)
            evaluation = tracker.evaluate_step(
                step=step,
                entities=entities,
                room_resource=room.resource,
            )
            error_summary = tracker.summary(entities)
            state = state_to_dict(
                step,
                room,
                entities,
                numeric=self.mode,
                version=STATE_MODEL_VERSION,
            )
            result = StepResult(
                step=step,
                mode=self.mode,
                state_hash=digest,
                chain_hash=current_chain,
                chain_prefix=chain_prefix if step == start_step + 1 else steps[-1].chain_hash,
                state=state,
                room=room.clone(numeric=self.mode),
                entities={
                    entity_id: entity.clone(numeric=self.mode)
                    for entity_id, entity in entities.items()
                },
                effective_ops=op_records,
                error_summary=error_summary,
                untrusted=list(evaluation["untrusted"]),
                cache_hits=cache_hits,
            )
            steps.append(result)
            if self.snapshot_store is not None:
                self.snapshot_store.commit(
                    step=step,
                    room=room,
                    entities=entities,
                    state_digest=digest,
                    chain_prefix=result.chain_prefix,
                    chain_digest=current_chain,
                    error_summary=error_summary,
                    error_state=tracker.snapshot_state(),
                    effective_ops=op_records,
                )
                last_committed = step
            final_state_digest = digest

        if not final_state_digest:
            final_state_digest = state_hash(
                start_step,
                room,
                entities,
                numeric=self.mode,
                version=STATE_MODEL_VERSION,
            )
        final_chain = current_chain if steps else chain_prefix
        return RunResult(
            recording=self.recording,
            mode=self.mode,
            steps=steps,
            final_room=room.clone(numeric=self.mode),
            final_entities={
                entity_id: entity.clone(numeric=self.mode)
                for entity_id, entity in entities.items()
            },
            final_state_hash=final_state_digest,
            final_chain_hash=final_chain,
            canceled=canceled,
            last_committed_step=last_committed,
            resumed_from=resumed_from,
            cache_stats=self.cache.stats(),
            config_digest=self.recording.config_digest(),
        )

    def _apply_fixed(
        self,
        op: Operation,
        step: int,
        room: RoomState,
        entities: dict[str, EntityState],
        tracker: ErrorTracker,
    ) -> tuple[dict[str, Any], bool]:
        config = self.recording.config
        entity = entities[op.entity_id]
        q_speed, speed_dir = quantize(op.speed, config.fixed_param_decimals)
        q_heading, heading_dir = quantize(op.heading, config.fixed_param_decimals)
        q_resource, resource_dir = quantize(
            op.resource_delta, config.fixed_param_decimals
        )

        sin_value, cos_value, raw_vx, raw_vy, trig_error, cache_hit = self.cache.fixed(
            q_speed, q_heading, config.fixed_param_decimals
        )
        _ = (sin_value, cos_value)
        vx, vx_dir = quantize(raw_vx, config.fixed_accum_decimals)
        vy, vy_dir = quantize(raw_vy, config.fixed_accum_decimals)

        raw_x = Decimal(str(entity.x)) + vx * config.timestep
        raw_y = Decimal(str(entity.y)) + vy * config.timestep
        new_x, x_dir = quantize(raw_x, config.fixed_accum_decimals)
        new_y, y_dir = quantize(raw_y, config.fixed_accum_decimals)
        raw_resource = Decimal(str(room.resource)) + q_resource
        new_resource, resource_accum_dir = quantize(
            raw_resource, config.fixed_accum_decimals
        )
        self._check_coord(new_x, new_y, step, op)

        heading = q_heading
        flipped = False
        if max(abs(new_x), abs(new_y)) > config.flip_threshold:
            flipped = True
            heading, heading_flip_dir = quantize(
                normalize_angle(q_heading + PI), config.fixed_param_decimals
            )
            _ = heading_flip_dir
            _, _, raw_vx, raw_vy, _, flip_cache_hit = self.cache.fixed(
                q_speed, heading, config.fixed_param_decimals
            )
            cache_hit = cache_hit or flip_cache_hit
            vx, vx_dir = quantize(raw_vx, config.fixed_accum_decimals)
            vy, vy_dir = quantize(raw_vy, config.fixed_accum_decimals)
            entity.flip_count += 1

        entity.x = new_x
        entity.y = new_y
        entity.vx = vx
        entity.vy = vy
        entity.heading = heading
        room.resource = new_resource

        tracker.add_fixed_operation(
            entity_id=op.entity_id,
            op_id=op.op_id,
            step=step,
            speed=op.speed,
            heading=op.heading,
            dt=config.timestep,
            trig_error=trig_error,
        )
        tracker.add_fixed_resource(op_id=op.op_id, step=step)

        rounding = {
            "speed": {
                "original": decimal_string(op.speed),
                "quantized": decimal_string(q_speed),
                "direction": speed_dir,
            },
            "heading": {
                "original": decimal_string(op.heading),
                "quantized": decimal_string(q_heading),
                "direction": heading_dir,
            },
            "resource_delta": {
                "original": decimal_string(op.resource_delta),
                "quantized": decimal_string(q_resource),
                "direction": resource_dir,
            },
            "accumulated_position": {
                "x": {"direction": x_dir, "value": decimal_string(new_x)},
                "y": {"direction": y_dir, "value": decimal_string(new_y)},
            },
            "accumulated_resource": {
                "direction": resource_accum_dir,
                "value": decimal_string(new_resource),
            },
            "accumulated_velocity": {
                "x": {"direction": vx_dir, "value": decimal_string(vx)},
                "y": {"direction": vy_dir, "value": decimal_string(vy)},
            },
        }
        record = {
            **op.identity_dict(),
            "rounding": rounding,
            "cache_hit": cache_hit,
            "flip_triggered": flipped,
        }
        return record, cache_hit

    def _apply_float(
        self,
        op: Operation,
        step: int,
        room: RoomState,
        entities: dict[str, EntityState],
    ) -> tuple[dict[str, Any], bool]:
        config = self.recording.config
        entity = entities[op.entity_id]
        speed = float(op.speed)
        heading = float(op.heading)
        resource_delta = float(op.resource_delta)
        vx, vy, cache_hit = self.cache.binary64(speed, heading)
        new_x = float(entity.x) + vx * float(config.timestep)
        new_y = float(entity.y) + vy * float(config.timestep)
        new_resource = float(room.resource) + resource_delta
        self._check_coord(new_x, new_y, step, op)

        flipped = False
        if max(abs(new_x), abs(new_y)) > float(config.flip_threshold):
            flipped = True
            heading = _normalize_float(heading + math.pi)
            vx, vy, flip_cache_hit = self.cache.binary64(speed, heading)
            cache_hit = cache_hit or flip_cache_hit
            entity.flip_count += 1

        entity.x = new_x
        entity.y = new_y
        entity.vx = vx
        entity.vy = vy
        entity.heading = heading
        room.resource = new_resource
        record = {
            **op.identity_dict(),
            "rounding": {},
            "cache_hit": cache_hit,
            "flip_triggered": flipped,
        }
        return record, cache_hit

    def _check_coord(self, x: Any, y: Any, step: int, op: Operation) -> None:
        limit = self.recording.config.coord_limit
        if self.mode == "fixed":
            out = abs(Decimal(str(x))) > limit or abs(Decimal(str(y))) > limit
        else:
            out = abs(float(x)) > float(limit) or abs(float(y)) > float(limit)
        if out:
            fail(
                E_COORD_RANGE,
                "entity position exceeds coord_limit",
                f"operations.{op.source_index}.entity_id",
                step=step,
                details={
                    "entity_id": op.entity_id,
                    "x": str(x),
                    "y": str(y),
                    "coord_limit": decimal_string(limit),
                },
            )


def _normalize_float(value: float) -> float:
    value = math.fmod(value, 2.0 * math.pi)
    if value > math.pi:
        value -= 2.0 * math.pi
    elif value <= -math.pi:
        value += 2.0 * math.pi
    return value


def serialize_log_record(payload: dict[str, Any]) -> str:
    import json

    return json.dumps(
        payload,
        ensure_ascii=True,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def run_recording(
    recording: Recording,
    mode: str,
    *,
    cache: VelocityCache | None = None,
    snapshot_store: SnapshotStore | None = None,
    cancel_check: Callable[[], bool] | None = None,
    cancel_after: int | None = None,
) -> RunResult:
    return ReplayEngine(
        recording,
        mode,
        cache=cache,
        snapshot_store=snapshot_store,
        cancel_check=cancel_check,
        cancel_after=cancel_after,
    ).run()
