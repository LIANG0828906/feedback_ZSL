"""重演编排：单演练串行推进、检查点续算、协作式取消与并行工作池。"""

from __future__ import annotations

import os
import threading
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass

from .checkpoint import CheckpointStore
from .engine import QuotaReplayEngine
from .errors import E_CANCELLED, E_QUOTA_EXCEEDED, ReplayError
from .models import ExerciseConfig, Operation, StepResult, canonical_dumps


class CancellationToken:
    """协作式取消令牌；暂停门控让 GUI 可以挂起/恢复重演。"""

    def __init__(self):
        self._cancel = threading.Event()
        self._pause = threading.Event()

    def cancel(self):
        self._cancel.set()

    @property
    def cancelled(self) -> bool:
        return self._cancel.is_set()

    def pause(self):
        self._pause.set()

    def resume(self):
        self._pause.clear()

    def gate(self):
        """每步结算前调用：暂停时阻塞，取消时抛错。"""
        if self._cancel.is_set():
            raise ReplayError(E_CANCELLED, "重演被协作式取消")
        while self._pause.is_set() and not self._cancel.is_set():
            self._cancel.wait(timeout=0.05)
        if self._cancel.is_set():
            raise ReplayError(E_CANCELLED, "重演被协作式取消")


@dataclass
class ReplayOutcome:
    exercise_id: str
    results: list
    checkpoint_path: str | None
    trajectory_path: str | None
    cancelled: bool = False
    error: ReplayError | None = None

    @property
    def step_hashes(self) -> list[str]:
        return [r.step_hash for r in self.results]


def replay_exercise(
    config: ExerciseConfig,
    operations: list[Operation],
    checkpoint_dir: str | None = None,
    trajectory_path: str | None = None,
    resume: bool = True,
    token: CancellationToken | None = None,
    on_step=None,
    strict_quota: bool = False,
) -> ReplayOutcome:
    """串行重演一场演练；同演练内逻辑步严格串行（单线程顺序推进）。"""
    token = token or CancellationToken()
    store = CheckpointStore(checkpoint_dir) if checkpoint_dir else None

    if store is not None and resume and store.exists():
        engine = store.resume_engine(config, operations)
    else:
        engine = QuotaReplayEngine(config, operations)

    trajectory_handle = None
    if trajectory_path:
        os.makedirs(os.path.dirname(os.path.abspath(trajectory_path)), exist_ok=True)
        if engine.step_index == 0:
            trajectory_handle = open(trajectory_path, "w", encoding="utf-8", newline="\n")
        else:
            trajectory_handle = open(
                trajectory_path, "a", encoding="utf-8", newline="\n"
            )

    try:
        while engine.step_index < config.total_steps:
            token.gate()
            result = engine.step()
            if strict_quota and result.quota_events:
                event = result.quota_events[0]
                raise ReplayError(
                    E_QUOTA_EXCEEDED,
                    (
                        f"步 {event.step} 玩家 {event.player_id} 透支: "
                        f"累计所需 {event.attempted_total} > 上限 {event.quota}"
                    ),
                    step=event.step,
                    player_id=event.player_id,
                    attempted_total=event.attempted_total,
                    quota=event.quota,
                    policy=event.policy,
                    rejected_op_ids=list(event.rejected_op_ids),
                    op_client_seqs=list(result.op_client_seqs),
                )
            if trajectory_handle is not None:
                _write_trajectory_lines(trajectory_handle, result)
            if store is not None:
                store.write(config, engine.snapshot())
            if on_step is not None:
                on_step(result)
    except ReplayError:
        if trajectory_handle is not None:
            trajectory_handle.close()
        raise
    finally:
        if trajectory_handle is not None and not trajectory_handle.closed:
            trajectory_handle.flush()
            trajectory_handle.close()

    return ReplayOutcome(
        exercise_id=config.exercise_id,
        results=engine.results,
        checkpoint_path=store.path if store else None,
        trajectory_path=trajectory_path,
    )


def _write_trajectory_lines(handle, result: StepResult) -> None:
    """每玩家一行的确定性轨迹日志（规范 JSON，固定字段顺序）。"""
    for player_id in sorted(result.cumulative_used):
        record = {
            "step": result.step,
            "player_id": player_id,
            "step_used": result.step_used[player_id],
            "cumulative_used": result.cumulative_used[player_id],
            "step_penalty": result.step_penalty[player_id],
            "cumulative_penalty": result.cumulative_penalty[player_id],
            "step_hash": result.step_hash,
        }
        handle.write(canonical_dumps(record) + "\n")


@dataclass
class BatchItem:
    name: str
    config: ExerciseConfig
    operations: list
    checkpoint_dir: str | None = None
    trajectory_path: str | None = None


def replay_batch(
    items: list[BatchItem],
    parallelism: int = 1,
    resume: bool = True,
    token: CancellationToken | None = None,
    on_progress=None,
) -> list[ReplayOutcome]:
    """并行重演多场演练。

    每场演练在一个工作任务内部严格串行；返回结果按输入顺序排列，
    且结果内容只由输入决定，与并行度/完成先后无关。
    """
    token = token or CancellationToken()
    outcomes: list[ReplayOutcome | None] = [None] * len(items)

    def _run(index: int, item: BatchItem) -> tuple[int, ReplayOutcome]:
        outcome = replay_exercise(
            item.config,
            item.operations,
            checkpoint_dir=item.checkpoint_dir,
            trajectory_path=item.trajectory_path,
            resume=resume,
            token=token,
        )
        if on_progress is not None:
            on_progress(index, outcome)
        return index, outcome

    workers = max(1, min(parallelism, len(items) or 1))
    if workers == 1:
        for index, item in enumerate(items):
            done_index, outcome = _run(index, item)
            outcomes[done_index] = outcome
    else:
        with ThreadPoolExecutor(max_workers=workers) as pool:
            futures = [pool.submit(_run, idx, item) for idx, item in enumerate(items)]
            for future in as_completed(futures):
                done_index, outcome = future.result()
                outcomes[done_index] = outcome

    return outcomes  # type: ignore[return-value]
