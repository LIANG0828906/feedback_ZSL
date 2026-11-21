"""档位参数版本模型。

每档参数包含确定性重采样率、增益与帧量化参数；每次提交生成单调递增的
参数版本，版本记录参数内容哈希与提交来源（``new`` / ``rollback``）。
回退生成新版本而不改写历史；内容相同的普通提交去重并返回既有版本号。
"""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Literal

from .errors import AuditError, ErrorCode
from .util import canonical_json, content_hash

CommitOrigin = Literal["new", "rollback"]

# 确定性允许的目标采样率（覆盖 16k–48k 常见档位）。
ALLOWED_TARGET_RATES = (16000, 22050, 24000, 32000, 44100, 48000)
MIN_QUANTUM_TICKS = 250  # 0.25ms，量化刻度下限


@dataclass(frozen=True)
class TierParams:
    """单档参数（全部为整数/可精确表示的字段，保证序列化确定）。"""

    target_rate: int  # 重采样目标采样率 Hz
    gain_q8: int  # 增益 Q8 定点数，256 表示 1.0
    quantum_ticks: int  # 帧量化刻度（整数微钟）
    slice_span_ticks: int  # 切片跨度（整数微秒，必须是 quantum_ticks 的整数倍）

    def validate(self) -> None:
        loc = ("params",)
        if self.target_rate not in ALLOWED_TARGET_RATES:
            raise AuditError(
                ErrorCode.E_STATE_INVALID,
                f"目标采样率 {self.target_rate} 不在允许集合 {ALLOWED_TARGET_RATES}",
                loc + ("target_rate",),
            )
        if not 0 <= self.gain_q8 <= 512:
            raise AuditError(
                ErrorCode.E_STATE_INVALID,
                f"增益 Q8 {self.gain_q8} 超出 [0,512]",
                loc + ("gain_q8",),
            )
        if self.quantum_ticks < MIN_QUANTUM_TICKS:
            raise AuditError(
                ErrorCode.E_STATE_INVALID,
                f"量化刻度 {self.quantum_ticks} 小于下限 {MIN_QUANTUM_TICKS}",
                loc + ("quantum_ticks",),
            )
        if self.slice_span_ticks <= 0:
            raise AuditError(
                ErrorCode.E_STATE_INVALID,
                "切片跨度必须为正",
                loc + ("slice_span_ticks",),
            )
        if self.slice_span_ticks % self.quantum_ticks != 0:
            raise AuditError(
                ErrorCode.E_STATE_INVALID,
                "切片跨度必须是量化刻度的整数倍",
                loc + ("slice_span_ticks",),
                {"quantum_ticks": self.quantum_ticks},
            )

    def content_key(self) -> dict:
        return asdict(self)

    def digest(self) -> str:
        return content_hash({"tier_params": self.content_key()})


@dataclass(frozen=True)
class ParamVersion:
    """参数版本记录。"""

    tier: str
    version: int
    params: TierParams
    content_hash: str
    origin: CommitOrigin
    rolled_back_from: int | None  # 回退时指向被复制的历史版本

    def to_dict(self) -> dict:
        return {
            "tier": self.tier,
            "version": self.version,
            "params": self.params.content_key(),
            "content_hash": self.content_hash,
            "origin": self.origin,
            "rolled_back_from": self.rolled_back_from,
        }

    @classmethod
    def from_dict(cls, data: dict) -> "ParamVersion":
        return cls(
            tier=data["tier"],
            version=int(data["version"]),
            params=TierParams(**data["params"]),
            content_hash=data["content_hash"],
            origin=data["origin"],
            rolled_back_from=data.get("rolled_back_from"),
        )


class ParameterStore:
    """各档位参数版本的内存模型与 JSON 快照持久化。"""

    def __init__(self, snapshot_path: str | Path | None = None) -> None:
        self.snapshot_path = Path(snapshot_path) if snapshot_path else None
        self._versions: dict[str, list[ParamVersion]] = {}
        if self.snapshot_path and self.snapshot_path.exists():
            self._load()

    def tiers(self) -> list[str]:
        return sorted(self._versions)

    def versions(self, tier: str) -> list[ParamVersion]:
        return list(self._versions.get(tier, ()))

    def latest(self, tier: str) -> ParamVersion:
        if tier not in self._versions:
            raise AuditError(
                ErrorCode.E_NOT_FOUND,
                f"档位 {tier} 尚无参数版本",
                ("parameter_store", tier),
            )
        return self._versions[tier][-1]

    def get(self, tier: str, version: int) -> ParamVersion:
        for record in self._versions.get(tier, ()):
            if record.version == version:
                return record
        raise AuditError(
            ErrorCode.E_NOT_FOUND,
            f"档位 {tier} 不存在版本 {version}",
            ("parameter_store", tier, str(version)),
        )

    def _append(self, tier: str, record: ParamVersion) -> None:
        self._versions.setdefault(tier, []).append(record)
        self._save()

    def submit(
        self,
        tier: str,
        params: TierParams,
        origin: CommitOrigin = "new",
        rolled_back_from: int | None = None,
    ) -> ParamVersion:
        """提交参数。

        普通提交（``new``）内容相同时去重并返回既有版本；回退提交
        （``rollback``）始终生成新版本，保留完整历史轨迹。
        """

        params.validate()
        digest = params.digest()
        history = self._versions.setdefault(tier, [])
        if origin == "new":
            for record in history:
                if record.content_hash == digest:
                    return record
            next_version = (history[-1].version + 1) if history else 1
            record = ParamVersion(tier, next_version, params, digest, "new", None)
        elif origin == "rollback":
            if rolled_back_from is None:
                raise AuditError(
                    ErrorCode.E_STATE_INVALID,
                    "回退提交必须指定 rolled_back_from",
                    ("parameter_store", tier, "rolled_back_from"),
                )
            source = self.get(tier, rolled_back_from)
            if source.params.digest() != digest:
                raise AuditError(
                    ErrorCode.E_STATE_INVALID,
                    "回退参数内容与历史版本不一致",
                    ("parameter_store", tier, str(rolled_back_from)),
                    {"expected": source.content_hash, "actual": digest},
                )
            next_version = (history[-1].version + 1) if history else 1
            record = ParamVersion(
                tier, next_version, params, digest, "rollback", rolled_back_from
            )
        else:  # pragma: no cover - 类型已约束
            raise AuditError(
                ErrorCode.E_STATE_INVALID, f"未知提交来源 {origin}", ("origin",)
            )
        self._append(tier, record)
        return record

    def rollback(self, tier: str, target_version: int) -> ParamVersion:
        """一键回退：复制历史参数并以新版本提交。"""
        source = self.get(tier, target_version)
        return self.submit(
            tier,
            source.params,
            origin="rollback",
            rolled_back_from=target_version,
        )

    def _save(self) -> None:
        if not self.snapshot_path:
            return
        self.snapshot_path.parent.mkdir(parents=True, exist_ok=True)
        payload = {
            tier: [record.to_dict() for record in records]
            for tier, records in self._versions.items()
        }
        tmp = self.snapshot_path.with_suffix(".json.tmp")
        tmp.write_bytes(canonical_json(payload))
        tmp.replace(self.snapshot_path)

    def _load(self) -> None:
        data = json.loads(self.snapshot_path.read_text(encoding="utf-8"))
        self._versions = {
            tier: [ParamVersion.from_dict(item) for item in records]
            for tier, records in data.items()
        }
