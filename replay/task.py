"""回放任务：状态机、确定性执行、快照提交与恢复、任务级轨迹日志。"""

from __future__ import annotations

import threading
import time
from dataclasses import dataclass
from pathlib import Path

from .config import FrozenConfig
from .engine import RuleEngine
from .errors import (
    E_BUDGET_EXCEEDED,
    E_CANCELLED,
    E_RECOVERY_INVALID,
    BudgetExceeded,
    CancelledError,
    ReplayError,
)
from .hashing import GENESIS_HASH, hash_json, write_json_atomic, write_text_atomic
from .model import MatchDescription, ResourceUsage, make_initial_runtime_state
from .snapshot import SnapshotStore

WAITING = "WAITING"
RUNNING = "RUNNING"
PAUSED = "PAUSED"
COMPLETED = "COMPLETED"
CANCELLED = "CANCELLED"
FAILED = "FAILED"

TERMINAL_STATES = (COMPLETED, CANCELLED, FAILED)


@dataclass
class TraceRecord:
    observation: str
    source: str
    turn: int
    status: str
    compute_total: int
    memory_total: int
    state_hash: str
    event_prefix_hash: str

    def to_line(self) -> str:
        return (
            f"{self.observation}|{self.source}|turn={self.turn}|"
            f"status={self.status}|compute={self.compute_total}|"
            f"memory={self.memory_total}|state_hash={self.state_hash}|"
            f"event_prefix_hash={self.event_prefix_hash}\n"
        )


class ReplayTask:
    def __init__(
        self,
        seq: int,
        desc: MatchDescription,
        config: FrozenConfig,
        output_root: str | Path,
    ) -> None:
        self.seq = seq
        self.desc = desc
        self.config = config
        self.match_id = desc.match_id
        self.task_dir = Path(output_root) / self.match_id
        self.store = SnapshotStore(self.task_dir)
        self.cancel_event = threading.Event()
        self.pause_event = threading.Event()
        self._lock = threading.RLock()
        self._status = WAITING
        self.error_code: str | None = None
        self.error_message: str | None = None
        self.current_turn = 0
        self.usage = ResourceUsage()
        self.last_state_hash = ""
        self.last_event_prefix_hash = GENESIS_HASH
        self.trace: list[TraceRecord] = []
        # 回合边界的可调度停顿：仅用于让暂停/取消检查点可被外部命中，
        # 不参与任何哈希与输出，默认值可由调度器按配置覆盖。
        self.turn_pause_seconds = 0.02

    # ---- 状态查询（加锁，供 GUI 轮询） ----
    def snapshot_status(self) -> dict:
        with self._lock:
            return {
                "seq": self.seq,
                "match_id": self.match_id,
                "status": self._status,
                "error_code": self.error_code,
                "error_message": self.error_message,
                "current_turn": self.current_turn,
                "total_turns": self.desc.turn_count,
                "compute_total": self.usage.compute_total,
                "memory_total": self.usage.memory_total,
                "memory_peak": self.usage.memory_peak,
                "last_state_hash": self.last_state_hash,
                "last_event_prefix_hash": self.last_event_prefix_hash,
            }

    @property
    def status(self) -> str:
        with self._lock:
            return self._status

    def is_terminal(self) -> bool:
        return self.status in TERMINAL_STATES

    def _set_status(self, status: str) -> None:
        with self._lock:
            self._status = status

    # ---- 协作式控制 ----
    def request_pause(self) -> None:
        self.pause_event.set()

    def resume(self) -> None:
        self.pause_event.clear()

    def request_cancel(self) -> None:
        self.cancel_event.set()
        # 暂停中的任务也要能立即响应取消。
        self.pause_event.clear()

    def _checkpoint(self) -> None:
        """资源检查点与回合边界统一调用的协作式检查点。"""
        if self.cancel_event.is_set():
            raise CancelledError(
                f"任务 {self.match_id} 在检查点被取消"
            )
        if self.pause_event.is_set():
            with self._lock:
                if self._status == RUNNING:
                    self._status = PAUSED
            while self.pause_event.is_set() and not self.cancel_event.is_set():
                # 等待恢复或取消；不消耗任何计算资源，状态保持 PAUSED。
                self.pause_event.wait(timeout=0.05)
            with self._lock:
                if self._status == PAUSED:
                    self._status = RUNNING
        if self.cancel_event.is_set():
            raise CancelledError(f"任务 {self.match_id} 在检查点被取消")

    def _record_trace(
        self,
        observation: str,
        source: str,
        turn: int,
        status: str,
        state_hash: str,
        event_prefix_hash: str,
    ) -> None:
        record = TraceRecord(
            observation=observation,
            source=source,
            turn=turn,
            status=status,
            compute_total=self.usage.compute_total,
            memory_total=self.usage.memory_total,
            state_hash=state_hash,
            event_prefix_hash=event_prefix_hash,
        )
        self.trace.append(record)

    def flush_trace(self) -> Path:
        """任务级轨迹日志：字段顺序固定，保证逐字节可复现。"""
        lines = [
            "# observation|source_of_truth|turn|status|compute_total|"
            "memory_total|state_hash|event_prefix_hash\n"
        ]
        lines.extend(record.to_line() for record in self.trace)
        return write_text_atomic(self.task_dir / "trace.log", "".join(lines))

    # ---- 执行 ----
    def execute(self, recover: bool) -> dict:
        """任务主入口，由调度器工作线程调用。"""
        with self._lock:
            self._status = RUNNING
        self._record_trace(
            "TASK_START", "scheduler", 0, RUNNING, "", GENESIS_HASH
        )
        try:
            engine, start_turn = self._build_engine(recover)
            self._run_loops(engine, start_turn)
        except CancelledError as exc:
            self._fail(CANCELLED, E_CANCELLED, str(exc))
        except BudgetExceeded as exc:
            # 超预算终止：保留最后一次已提交快照，等待后续从快照恢复。
            self._fail(FAILED, E_BUDGET_EXCEEDED, str(exc))
        except ReplayError as exc:
            self._fail(FAILED, exc.code, str(exc))
        self.flush_trace()
        return self.snapshot_status()

    def _build_engine(self, recover: bool) -> tuple[RuleEngine, int]:
        point = self.store.recovery_point()
        if point is None:
            if recover:
                raise ReplayError(
                    E_RECOVERY_INVALID,
                    "要求从快照恢复但不存在任何已提交快照",
                )
            state = make_initial_runtime_state(self.desc)
            usage = ResourceUsage()
            start_turn = 1
            self._record_trace(
                "RECOVERY_SCAN", "snapshot_store", 0, RUNNING, "", GENESIS_HASH
            )
        else:
            snapshot, usage = point
            state = snapshot["state"]
            start_turn = int(snapshot["turn"]) + 1
            # 恢复内部哈希链游标，保证下一快照的 prev_snapshot_hash 接续。
            state["snapshot_hash"] = snapshot["snapshot_hash"]
            self.current_turn = snapshot["turn"]
            self.usage = usage
            self.last_state_hash = snapshot["state_hash"]
            self.last_event_prefix_hash = snapshot["event_prefix_hash"]
            self._record_trace(
                "RECOVERY_SCAN",
                "snapshot_store",
                snapshot["turn"],
                RUNNING,
                snapshot["state_hash"],
                snapshot["event_prefix_hash"],
            )
        engine = RuleEngine(
            self.desc,
            self.config.task_compute_budget,
            self.config.task_memory_budget,
            usage=usage,
            state=state,
            cancel_checkpoint=self._checkpoint,
        )
        if point is not None:
            # 恢复后事件序号必须从已提交事件流末尾继续，保证 seq 全局连续；
            # 全新运行不得读取磁盘残留，避免事件被重复计入。
            engine.events = self._load_committed_events()
        return engine, start_turn

    def _run_loops(self, engine: RuleEngine, start_turn: int) -> None:
        for turn in range(start_turn, self.desc.turn_count + 1):
            self._checkpoint()  # 回合边界检查点
            if self.turn_pause_seconds:
                time.sleep(self.turn_pause_seconds)
            snapshot = engine.run_turn()
            # run_turn 内部已通过资源检查点；这里是回合结束的边界检查点。
            self._checkpoint()
            # 原子提交后才更新任务对外可见进度，在途状态不落盘。
            self.store.commit(snapshot)
            with self._lock:
                self.current_turn = turn
                self.usage = engine.meter.usage
                self.last_state_hash = snapshot["state_hash"]
                self.last_event_prefix_hash = snapshot["event_prefix_hash"]
            self._record_trace(
                "TURN_COMMITTED",
                "snapshot_store",
                turn,
                RUNNING,
                snapshot["state_hash"],
                snapshot["event_prefix_hash"],
            )

        verdict = engine.verdict()
        result = {
            "match_id": self.match_id,
            "turn": self.desc.turn_count,
            "verdict": verdict,
            "final_state_hash": self.last_state_hash,
            # engine.events 已在 run_turn 内逐回合追加；恢复场景下还预置了
            # 已提交事件，因此这里天然是完整事件流，不能再重复 extend。
            "event_stream_hash": hash_json(engine.events),
            "snapshot_hashes": [
                self.store.load(turn)["snapshot_hash"]
                for turn in self.store.list_turns()
            ],
            "resource_usage": self.usage.to_dict(),
        }
        write_json_atomic(self.task_dir / "result.json", result)
        with self._lock:
            self._status = COMPLETED
        self._record_trace(
            "TASK_COMPLETED",
            "rule_engine",
            self.desc.turn_count,
            COMPLETED,
            self.last_state_hash,
            self.last_event_prefix_hash,
        )

    def _load_committed_events(self) -> list[dict]:
        events: list[dict] = []
        for turn in self.store.list_turns():
            events.extend(self.store.load(turn)["events"])
        return events

    def _fail(self, status: str, code: str, message: str) -> None:
        with self._lock:
            self._status = status
            self.error_code = code
            self.error_message = message
        self._record_trace(
            "TASK_FAILED" if status == FAILED else "TASK_CANCELLED",
            "scheduler",
            self.current_turn,
            status,
            self.last_state_hash,
            self.last_event_prefix_hash,
        )
        write_json_atomic(
            self.task_dir / "error.json",
            {
                "match_id": self.match_id,
                "error_code": code,
                "error_message": message,
                "committed_turn": self.current_turn,
            },
        )
