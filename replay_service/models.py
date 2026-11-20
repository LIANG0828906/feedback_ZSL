"""演练配置、操作记录与结算结果的数据模型。

所有结构化数据都通过 :func:`canonical_dumps` 做规范序列化，
保证相同语义内容在任意并行度下产生逐字节一致的输出。
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field

CANONICAL_KW = dict(sort_keys=True, separators=(",", ":"), ensure_ascii=False)

CHECKPOINT_VERSION = "quota-replay-cp/v1"
BASELINE_VERSION = "quota-replay-baseline/v1"
RULE_VERSION = "quota-rules/v1"

POLICY_REJECT = "reject"
POLICY_DEGRADE = "degrade"


def canonical_dumps(obj) -> str:
    """确定性 JSON 序列化：键排序、紧凑分隔、不转义非 ASCII。"""
    return json.dumps(_jsonable(obj), **CANONICAL_KW)


def _jsonable(obj):
    if hasattr(obj, "to_dict"):
        return obj.to_dict()
    if isinstance(obj, dict):
        return {str(k): _jsonable(v) for k, v in obj.items()}
    if isinstance(obj, (list, tuple)):
        return [_jsonable(v) for v in obj]
    return obj


@dataclass(frozen=True)
class Operation:
    """单条带客户端序号的操作。

    ``client_ts`` / ``arrival_ts`` 仅用于延迟观测，绝不参与结算。
    """

    op_id: str
    player_id: str
    client_seq: int
    effective_step: int
    required_compute: float
    client_ts: float | None = None
    arrival_ts: float | None = None

    def to_dict(self) -> dict:
        return {
            "op_id": self.op_id,
            "player_id": self.player_id,
            "client_seq": self.client_seq,
            "effective_step": self.effective_step,
            "required_compute": self.required_compute,
        }


@dataclass(frozen=True)
class PenaltyRule:
    """透支惩罚规则。

    reject  ：按客户端序号逐条拒绝使累计用量超限的操作，
              每条按 ``reject_penalty`` 计扣减；
    degrade ：整步降级，允许操作全部生效，对总超出量按
              ``degrade_rate`` 计一次扣减，并叠加固定扣减。
    """

    policy: str
    reject_penalty: float
    degrade_rate: float
    degrade_fixed: float

    def to_dict(self) -> dict:
        return {
            "policy": self.policy,
            "reject_penalty": self.reject_penalty,
            "degrade_rate": self.degrade_rate,
            "degrade_fixed": self.degrade_fixed,
        }


@dataclass(frozen=True)
class ExerciseConfig:
    exercise_id: str
    total_steps: int
    per_step_quota: float
    player_ids: tuple[str, ...]
    penalty_rule: PenaltyRule

    def to_dict(self) -> dict:
        return {
            "exercise_id": self.exercise_id,
            "total_steps": self.total_steps,
            "per_step_quota": self.per_step_quota,
            "player_ids": list(self.player_ids),
            "penalty_rule": self.penalty_rule.to_dict(),
        }


@dataclass
class PenaltyEvent:
    step: int
    player_id: str
    op_id: str | None
    client_seq: int | None
    kind: str
    overage: float
    deduction: float

    def to_dict(self) -> dict:
        return {
            "step": self.step,
            "player_id": self.player_id,
            "op_id": self.op_id,
            "client_seq": self.client_seq,
            "kind": self.kind,
            "overage": self.overage,
            "deduction": self.deduction,
        }


@dataclass
class QuotaEvent:
    """一次 E_QUOTA_EXCEEDED 判定（拒绝或整步降级各一条）。"""

    step: int
    player_id: str
    attempted_total: float
    quota: float
    policy: str
    rejected_op_ids: list[str] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "step": self.step,
            "player_id": self.player_id,
            "attempted_total": self.attempted_total,
            "quota": self.quota,
            "policy": self.policy,
            "rejected_op_ids": list(self.rejected_op_ids),
        }


@dataclass
class StepResult:
    step: int
    step_used: dict
    cumulative_used: dict
    step_penalty: dict
    cumulative_penalty: dict
    penalties: list
    quota_events: list
    op_client_seqs: list
    step_hash: str

    def to_dict(self) -> dict:
        return {
            "step": self.step,
            "step_used": dict(self.step_used),
            "cumulative_used": dict(self.cumulative_used),
            "step_penalty": dict(self.step_penalty),
            "cumulative_penalty": dict(self.cumulative_penalty),
            "penalties": [p.to_dict() for p in self.penalties],
            "quota_events": [e.to_dict() for e in self.quota_events],
            "op_client_seqs": list(self.op_client_seqs),
            "step_hash": self.step_hash,
        }
