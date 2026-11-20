"""回放管线编排：归一 -> 差分重建 -> 确定性推进 -> 快照 -> 基线比对。"""
from __future__ import annotations

import threading
from dataclasses import dataclass, field
from pathlib import Path

from . import snapshot as snapshot_mod
from .baseline import BASELINE_MODEL_VERSION, ForkPoint, compare, load_baseline
from .diff_codec import decode_diff, diff_states, validate_field_order
from .engine import DeterministicEngine, TickReport
from .errors import ErrorCode, ReplayError
from .injector import validate_injection
from .models import NormalizedReplay, RoomState
from .parser import parse_replay


@dataclass
class ReplayResult:
    room_id: str
    version: int
    total_ticks: int
    states: dict[int, RoomState]
    reports: dict[int, TickReport]
    hash_chain: list[str]
    snapshot_sources: dict[int, str]
    fork: ForkPoint | None = None
    recovery_tick: int | None = None

    def to_summary(self) -> dict:
        return {
            "room_id": self.room_id,
            "version": self.version,
            "total_ticks": self.total_ticks,
            "hash_chain_head": self.hash_chain[0][:16] if self.hash_chain else None,
            "hash_chain_tail": self.hash_chain[-1][:16] if self.hash_chain else None,
            "fork": self.fork.to_dict() if self.fork else None,
            "recovery_tick": self.recovery_tick,
            "snapshot_sources": dict(sorted(self.snapshot_sources.items())),
        }


class RoomTaskLock:
    """同一房间同时只允许一个推进任务；重复启动返回 E_BUSY。"""

    def __init__(self) -> None:
        self._rooms: set[str] = set()
        self._cond = threading.Condition()

    def acquire(self, room_id: str) -> None:
        with self._cond:
            if room_id in self._rooms:
                raise ReplayError(
                    ErrorCode.E_BUSY,
                    f"room {room_id} already has a running replay task",
                    field_path=f"room:{room_id}",
                )
            self._rooms.add(room_id)

    def release(self, room_id: str) -> None:
        with self._cond:
            self._rooms.discard(room_id)
            self._cond.notify_all()


ROOM_LOCK = RoomTaskLock()


@dataclass
class ReplayPipeline:
    snapshot_dir: str | Path
    baseline_path: str | Path | None = None
    snapshot_every: int = 1
    fault: str | None = None
    fault_record: int = 0
    fault_extra: dict = field(default_factory=dict)
    _cancel: threading.Event = field(default_factory=threading.Event)
    _pause: threading.Event = field(default_factory=threading.Event)

    def request_cancel(self) -> None:
        self._cancel.set()

    def pause(self) -> None:
        self._pause.set()

    def resume(self) -> None:
        self._pause.clear()

    def _gate(self) -> None:
        if self._cancel.is_set():
            raise ReplayError(ErrorCode.E_BUSY, "replay cancelled cooperatively")
        while self._pause.is_set() and not self._cancel.is_set():
            self._pause.wait(timeout=0.05)
        if self._cancel.is_set():
            raise ReplayError(ErrorCode.E_BUSY, "replay cancelled while paused")

    def run(self, source: str | Path | dict) -> ReplayResult:
        # 入口注入校验：任何解析/状态文件写出之前拦截。
        if self.fault:
            validate_injection(
                {} if not isinstance(source, dict) else source,
                self.fault,
                self.fault_record,
                {**self.fault_extra, "snapshot_dir": self.snapshot_dir},
            )

        replay = parse_replay(source)
        room_id = replay.room_id
        ROOM_LOCK.acquire(room_id)
        try:
            return self._run_locked(replay)
        finally:
            ROOM_LOCK.release(room_id)

    def _run_locked(self, replay: NormalizedReplay) -> ReplayResult:
        self._reconstruct_snapshots(replay)
        recovery = snapshot_mod.verify_and_recover(
            self.snapshot_dir,
            replay.initial_state,
            replay.operations,
            replay.late_window,
        )
        if recovery is not None:
            engine = recovery.engine
            start_tick = recovery.last_tick
        else:
            engine = DeterministicEngine(replay.initial_state, replay.late_window)
            start_tick = 0

        engine.ingest(replay.operations)
        states: dict[int, RoomState] = {}
        reports: dict[int, TickReport] = {}
        hash_chain: list[str] = []

        for tick in range(start_tick + 1, replay.total_ticks + 1):
            self._gate()
            report = engine.step()
            states[tick] = engine.state.clone()
            reports[tick] = report
            hash_chain.append(report.state_hash)
            if self.snapshot_every > 0 and tick % self.snapshot_every == 0:
                snapshot_mod.write_snapshot_atomic(self.snapshot_dir, engine, replay.late_window)

        fork = None
        if self.baseline_path is not None:
            baseline = load_baseline(self.baseline_path, BASELINE_MODEL_VERSION)
            fork = compare(baseline, states, reports)

        return ReplayResult(
            room_id=replay.room_id,
            version=replay.version,
            total_ticks=replay.total_ticks,
            states=states,
            reports=reports,
            hash_chain=hash_chain,
            snapshot_sources=dict(replay.snapshot_sources),
            fork=fork,
            recovery_tick=recovery.source_tick if recovery else None,
        )

    def _reconstruct_snapshots(self, replay: NormalizedReplay) -> None:
        """跨版本状态重建：v3 差分还原 + v2 快照校验；缺失快照由操作重放推导。"""
        if replay.version == 3:
            self._reconstruct_v3(replay)
        elif replay.version == 2:
            replay.snapshot_sources = {tick: "recorded" for tick in replay.snapshots}
            self._derive_missing(replay)
        else:
            replay.snapshot_sources = {tick: "derived:operation-replay" for tick in range(1, replay.total_ticks + 1)}

    def _reconstruct_v3(self, replay: NormalizedReplay) -> None:
        base = replay.initial_state
        validate_field_order(replay.field_order, base)
        decoded: dict[int, RoomState] = {}
        for tick in sorted(replay.diffs):
            record_index = tick
            state = decode_diff(base, replay.diffs[tick], record_index, tick)
            decoded[tick] = state
            if tick in replay.snapshots:
                mismatches = diff_states(replay.snapshots[tick], state)
                if mismatches:
                    first = mismatches[0]
                    raise ReplayError(
                        ErrorCode.E_DIFF_INVALID,
                        "decoded diff disagrees with recorded snapshot",
                        record_index=record_index,
                        field_path=first["path"],
                        detail={"expected": first["expected"], "actual": first["actual"]},
                    )
            base = state
        replay.snapshots.update(decoded)
        replay.snapshot_sources = {tick: "recorded:v3-diff" for tick in decoded}
        self._derive_missing(replay)

    def _derive_missing(self, replay: NormalizedReplay) -> None:
        """v1/v2 升级到 v3 内部表示时，缺失快照由操作序列重放推导并标注来源。"""
        engine = DeterministicEngine(replay.initial_state, replay.late_window)
        engine.ingest(replay.operations)
        for tick in range(1, replay.total_ticks + 1):
            engine.step()
            if tick not in replay.snapshots:
                replay.snapshots[tick] = engine.state.clone()
                replay.snapshot_sources[tick] = "derived:operation-replay"
