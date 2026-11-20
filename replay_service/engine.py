"""按逻辑步推进的确定性配额结算引擎。

设计要点：
* 同一步内先按 (player_id, client_seq) 稳定排序再结算，
  裁决顺序只由客户端序号决定，与输入到达顺序/并行度无关；
* 引擎不读取系统时间、不使用全局随机状态；
* 每步哈希仅由结算后的累计配额与惩罚明细的规范序列化得出，
  并与上一步哈希组成哈希链；
* :meth:`run`（离线整批）与 :meth:`step`（在线逐步）走同一份
  结算代码，相同输入逐步结果完全一致。
"""

from __future__ import annotations

import copy
import hashlib

from .models import (
    POLICY_DEGRADE,
    POLICY_REJECT,
    ExerciseConfig,
    Operation,
    PenaltyEvent,
    QuotaEvent,
    StepResult,
    canonical_dumps,
)

GENESIS_HASH = "0" * 64


def _sha256_hex(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


class QuotaReplayEngine:
    def __init__(self, config: ExerciseConfig, operations: list[Operation]):
        self.config = config
        self._by_step: dict[int, list[Operation]] = self._index_operations(operations)
        self.step_index = 0
        self.cumulative_used = {pid: 0.0 for pid in config.player_ids}
        self.cumulative_penalty = {pid: 0.0 for pid in config.player_ids}
        self.prev_hash = GENESIS_HASH
        self.step_records: list[dict] = []
        self.results: list[StepResult] = []
        self.quota_events: list[QuotaEvent] = []
        self.penalties: list[PenaltyEvent] = []
        self._consumed_ops = 0

    @staticmethod
    def _index_operations(operations):
        by_step: dict[int, list[Operation]] = {}
        for op in operations:
            by_step.setdefault(op.effective_step, []).append(op)
        return by_step

    @property
    def buffer_watermark(self) -> int:
        """已结算操作数量（操作缓冲高水位）。"""
        return self._consumed_ops

    def run(self, until_step: int | None = None, on_step=None) -> list[StepResult]:
        """离线/在线统一入口：从当前步推进到 ``until_step``（不含）或结束。"""
        end = self.config.total_steps if until_step is None else until_step
        while self.step_index < end:
            result = self.step()
            if on_step is not None:
                on_step(result)
        return self.results

    def step(self) -> StepResult:
        if self.step_index >= self.config.total_steps:
            raise StopIteration("所有逻辑步已结算完毕")
        current = self.step_index
        ops = sorted(
            self._by_step.get(current, []),
            key=lambda op: (op.player_id, op.client_seq),
        )
        result = self._settle(current, ops)
        self.step_index += 1
        self._consumed_ops += len(ops)
        self.results.append(result)
        return result

    def _settle(self, step: int, ops: list[Operation]) -> StepResult:
        rule = self.config.penalty_rule
        quota = self.config.per_step_quota

        attempted: dict[str, float] = {pid: 0.0 for pid in self.config.player_ids}
        for op in ops:
            attempted[op.player_id] += op.required_compute

        accepted: list[Operation] = []
        rejected_ids: set[str] = set()
        step_quota_events: list[QuotaEvent] = []
        step_penalties: list[PenaltyEvent] = []
        step_penalty = {pid: 0.0 for pid in self.config.player_ids}
        step_used = {pid: 0.0 for pid in self.config.player_ids}

        if rule.policy == POLICY_REJECT:
            running = {pid: 0.0 for pid in self.config.player_ids}
            for op in ops:
                projected = running[op.player_id] + op.required_compute
                if projected > quota:
                    rejected_ids.add(op.op_id)
                    overage = projected - quota
                    deduction = rule.reject_penalty
                    step_penalty[op.player_id] += deduction
                    step_penalties.append(
                        PenaltyEvent(
                            step=step,
                            player_id=op.player_id,
                            op_id=op.op_id,
                            client_seq=op.client_seq,
                            kind="reject",
                            overage=overage,
                            deduction=deduction,
                        )
                    )
                else:
                    running[op.player_id] = projected
                    accepted.append(op)
            for pid in self.config.player_ids:
                if attempted[pid] > quota:
                    step_quota_events.append(
                        QuotaEvent(
                            step=step,
                            player_id=pid,
                            attempted_total=attempted[pid],
                            quota=quota,
                            policy=POLICY_REJECT,
                            rejected_op_ids=[],
                        )
                    )
            for event in step_quota_events:
                event.rejected_op_ids = [
                    op.op_id
                    for op in ops
                    if op.player_id == event.player_id and op.op_id in rejected_ids
                ]
        else:  # POLICY_DEGRADE：整步降级，操作全部生效，超出量统一计罚
            for op in ops:
                accepted.append(op)
            for pid in self.config.player_ids:
                if attempted[pid] > quota:
                    overage = attempted[pid] - quota
                    deduction = rule.degrade_fixed + rule.degrade_rate * overage
                    step_penalty[pid] += deduction
                    step_penalties.append(
                        PenaltyEvent(
                            step=step,
                            player_id=pid,
                            op_id=None,
                            client_seq=None,
                            kind="degrade",
                            overage=overage,
                            deduction=deduction,
                        )
                    )
                    step_quota_events.append(
                        QuotaEvent(
                            step=step,
                            player_id=pid,
                            attempted_total=attempted[pid],
                            quota=quota,
                            policy=POLICY_DEGRADE,
                            rejected_op_ids=[],
                        )
                    )

        for op in accepted:
            step_used[op.player_id] += op.required_compute

        for pid in self.config.player_ids:
            self.cumulative_used[pid] += step_used[pid]
            self.cumulative_penalty[pid] += step_penalty[pid]

        self.penalties.extend(step_penalties)
        self.quota_events.extend(step_quota_events)

        op_client_seqs = [
            {"player_id": op.player_id, "client_seq": op.client_seq}
            for op in ops
        ]
        step_hash = self._hash_step(step, step_penalties, op_client_seqs)

        result = StepResult(
            step=step,
            step_used=dict(step_used),
            cumulative_used=dict(self.cumulative_used),
            step_penalty=dict(step_penalty),
            cumulative_penalty=dict(self.cumulative_penalty),
            penalties=list(step_penalties),
            quota_events=list(step_quota_events),
            op_client_seqs=op_client_seqs,
            step_hash=step_hash,
        )

        record = {
            "step": step,
            "cumulative_used": dict(self.cumulative_used),
            "cumulative_penalty": dict(self.cumulative_penalty),
            "step_penalty": dict(step_penalty),
            "penalties": [p.to_dict() for p in step_penalties],
            "op_client_seqs": op_client_seqs,
            "step_hash": step_hash,
            "prev_hash": self.prev_hash,
        }
        self.prev_hash = step_hash
        self.step_records.append(record)
        return result

    def _hash_step(self, step, step_penalties, op_client_seqs) -> str:
        """步骤哈希：只依赖结算后累计配额与惩罚明细（含链上前缀）。"""
        payload = {
            "step": step,
            "prev_hash": self.prev_hash,
            "cumulative_used": dict(self.cumulative_used),
            "cumulative_penalty": dict(self.cumulative_penalty),
            "penalties": [p.to_dict() for p in step_penalties],
            "op_client_seqs": op_client_seqs,
        }
        return _sha256_hex(canonical_dumps(payload))

    def snapshot(self) -> dict:
        """供检查点使用的深拷贝状态。"""
        return {
            "step_index": self.step_index,
            "cumulative_used": copy.deepcopy(self.cumulative_used),
            "cumulative_penalty": copy.deepcopy(self.cumulative_penalty),
            "prev_hash": self.prev_hash,
            "step_records": copy.deepcopy(self.step_records),
            "buffer_watermark": self._consumed_ops,
        }

    @classmethod
    def restore(cls, config: ExerciseConfig, operations: list[Operation], state: dict):
        engine = cls(config, operations)
        engine.step_index = state["step_index"]
        engine.cumulative_used = dict(state["cumulative_used"])
        engine.cumulative_penalty = dict(state["cumulative_penalty"])
        engine.prev_hash = state["prev_hash"]
        engine.step_records = list(state["step_records"])
        engine._consumed_ops = state["buffer_watermark"]
        engine.results = [
            engine._result_from_record(
                record,
                engine.step_records[index - 1] if index > 0 else None,
            )
            for index, record in enumerate(engine.step_records)
        ]
        return engine

    @staticmethod
    def _result_from_record(record: dict, prev_record: dict | None) -> StepResult:
        prev_used = (
            prev_record["cumulative_used"]
            if prev_record is not None
            else {pid: 0.0 for pid in record["cumulative_used"]}
        )
        step_used = {
            pid: record["cumulative_used"][pid] - prev_used.get(pid, 0.0)
            for pid in record["cumulative_used"]
        }
        return StepResult(
            step=record["step"],
            step_used=step_used,
            cumulative_used=dict(record["cumulative_used"]),
            step_penalty=dict(record["step_penalty"]),
            cumulative_penalty=dict(record["cumulative_penalty"]),
            penalties=[PenaltyEvent(**p) for p in record["penalties"]],
            quota_events=[],
            op_client_seqs=list(record["op_client_seqs"]),
            step_hash=record["step_hash"],
        )
