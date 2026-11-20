"""每步原子检查点与强制终止后的续算。

检查点写入流程：写临时文件 -> flush/fsync -> os.replace 原子重命名。
写入任何阶段失败都不会覆盖上一有效检查点，并以 E_CHECKPOINT_WRITE
报告步号。续算时完整重放哈希链，发现版本不符/字段缺失/断链即以
E_RESUME_INVALID 中止并定位首个断链步，且不会写出任何新检查点。
"""

from __future__ import annotations

import os
import tempfile

from .errors import E_CHECKPOINT_WRITE, E_RESUME_INVALID, ReplayError
from .models import (
    CHECKPOINT_VERSION,
    RULE_VERSION,
    ExerciseConfig,
    canonical_dumps,
)
from .engine import GENESIS_HASH, QuotaReplayEngine, _sha256_hex

CHECKPOINT_FILENAME = "checkpoint.json"

_REQUIRED_TOP_FIELDS = (
    "version",
    "rule_version",
    "exercise_id",
    "total_steps",
    "step_index",
    "cumulative_used",
    "cumulative_penalty",
    "prev_hash",
    "buffer_watermark",
    "step_records",
)

_REQUIRED_RECORD_FIELDS = (
    "step",
    "cumulative_used",
    "cumulative_penalty",
    "step_penalty",
    "penalties",
    "op_client_seqs",
    "step_hash",
    "prev_hash",
)


class CheckpointStore:
    def __init__(self, directory: str):
        self.directory = directory
        self.path = os.path.join(directory, CHECKPOINT_FILENAME)
        os.makedirs(directory, exist_ok=True)

    def write(
        self,
        config: ExerciseConfig,
        state: dict,
        fail_at: str | None = None,
    ) -> str:
        """原子写出检查点；``fail_at`` 仅用于故障注入测试。"""
        payload = {
            "version": CHECKPOINT_VERSION,
            "rule_version": RULE_VERSION,
            "exercise_id": config.exercise_id,
            "total_steps": config.total_steps,
            **state,
        }
        encoded = (canonical_dumps(payload) + "\n").encode("utf-8")

        tmp_fd, tmp_path = tempfile.mkstemp(
            prefix=".checkpoint.", suffix=".tmp", dir=self.directory
        )
        try:
            with os.fdopen(tmp_fd, "wb") as handle:
                handle.write(encoded)
                handle.flush()
                os.fsync(handle.fileno())
                if fail_at == "after_write":
                    raise OSError("故障注入：临时文件写入后失败")
            if fail_at == "before_replace":
                raise OSError("故障注入：原子重命名前失败")
            os.replace(tmp_path, self.path)
            if fail_at == "after_replace":
                raise OSError("故障注入：重命名后失败")
        except OSError as exc:
            if os.path.exists(tmp_path):
                try:
                    os.unlink(tmp_path)
                except OSError:
                    pass
            raise ReplayError(
                E_CHECKPOINT_WRITE,
                f"第 {state.get('step_index', '?')} 步检查点写入失败: {exc}",
                step=state.get("step_index"),
                path=self.path,
            ) from exc
        return self.path

    def exists(self) -> bool:
        return os.path.exists(self.path)

    def load_raw(self) -> dict:
        import json

        try:
            with open(self.path, "r", encoding="utf-8") as handle:
                return json.load(handle)
        except (OSError, json.JSONDecodeError) as exc:
            raise ReplayError(
                E_RESUME_INVALID,
                f"检查点无法读取: {exc}",
                broken_step=0,
            ) from exc

    def validate(self, payload: dict, config: ExerciseConfig) -> None:
        """校验版本、字段完整性与哈希链；任何失败都在写新检查点之前抛出。"""
        if not isinstance(payload, dict):
            raise ReplayError(
                E_RESUME_INVALID, "检查点根对象不是 JSON 对象", broken_step=0
            )

        if payload.get("version") != CHECKPOINT_VERSION:
            raise ReplayError(
                E_RESUME_INVALID,
                f"检查点版本不符: 期望 {CHECKPOINT_VERSION}，"
                f"实际 {payload.get('version')!r}",
                broken_step=0,
            )
        if payload.get("rule_version") != RULE_VERSION:
            raise ReplayError(
                E_RESUME_INVALID,
                f"规则版本不符: 期望 {RULE_VERSION}，"
                f"实际 {payload.get('rule_version')!r}",
                broken_step=0,
            )
        if payload.get("exercise_id") != config.exercise_id:
            raise ReplayError(
                E_RESUME_INVALID,
                "检查点 exercise_id 与当前演练不一致",
                broken_step=0,
            )

        for field_name in _REQUIRED_TOP_FIELDS:
            if field_name not in payload:
                raise ReplayError(
                    E_RESUME_INVALID,
                    f"检查点缺少必需字段 '{field_name}'",
                    broken_step=0,
                )

        records = payload["step_records"]
        if not isinstance(records, list):
            raise ReplayError(
                E_RESUME_INVALID, "step_records 应为数组", broken_step=0
            )

        expected_prev = GENESIS_HASH
        for index, record in enumerate(records):
            broken_step = record.get("step", index) if isinstance(record, dict) else index
            if not isinstance(record, dict):
                raise ReplayError(
                    E_RESUME_INVALID,
                    f"第 {broken_step} 步记录不是 JSON 对象",
                    broken_step=broken_step,
                )
            for field_name in _REQUIRED_RECORD_FIELDS:
                if field_name not in record:
                    raise ReplayError(
                        E_RESUME_INVALID,
                        f"第 {broken_step} 步记录缺少字段 '{field_name}'",
                        broken_step=broken_step,
                    )
            if record["prev_hash"] != expected_prev:
                raise ReplayError(
                    E_RESUME_INVALID,
                    f"第 {broken_step} 步哈希链断链: "
                    f"期望 prev_hash={expected_prev}，"
                    f"实际 {record['prev_hash']!r}",
                    broken_step=broken_step,
                )
            recomputed = _recompute_record_hash(record)
            if recomputed != record["step_hash"]:
                raise ReplayError(
                    E_RESUME_INVALID,
                    f"第 {broken_step} 步哈希校验失败: 内容被篡改或损坏",
                    broken_step=broken_step,
                )
            expected_prev = record["step_hash"]

        if payload["prev_hash"] != expected_prev:
            raise ReplayError(
                E_RESUME_INVALID,
                "检查点链尾哈希与 prev_hash 不一致",
                broken_step=payload.get("step_index", 0),
            )

        if records:
            last = records[-1]
            if last["step"] != payload["step_index"] - 1:
                raise ReplayError(
                    E_RESUME_INVALID,
                    "step_index 与已提交步数不一致",
                    broken_step=payload["step_index"],
                )

    def resume_engine(
        self, config: ExerciseConfig, operations: list[Operation]
    ) -> QuotaReplayEngine:
        """加载并校验最近有效检查点，返回停在断点上的引擎。"""
        payload = self.load_raw()
        self.validate(payload, config)
        state = {
            "step_index": payload["step_index"],
            "cumulative_used": payload["cumulative_used"],
            "cumulative_penalty": payload["cumulative_penalty"],
            "prev_hash": payload["prev_hash"],
            "step_records": payload["step_records"],
            "buffer_watermark": payload["buffer_watermark"],
        }
        return QuotaReplayEngine.restore(config, operations, state)


def _recompute_record_hash(record: dict) -> str:
    payload = {
        "step": record["step"],
        "prev_hash": record["prev_hash"],
        "cumulative_used": record["cumulative_used"],
        "cumulative_penalty": record["cumulative_penalty"],
        "penalties": record["penalties"],
        "op_client_seqs": record["op_client_seqs"],
    }
    return _sha256_hex(canonical_dumps(payload))
