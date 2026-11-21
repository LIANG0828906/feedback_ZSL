"""审看编排引擎：增量重算、回退失效、任务锁与轨迹日志。

引擎把参数版本模型、依赖图谱、切片存储与跨档校验组合成可重复执行的
确定性流程。同一档位同时只允许一个重算任务（重复启动 ``E_BUSY``），
取消或异常后释放文件句柄并保留最后一次已提交状态。
"""

from __future__ import annotations

import json
import threading
from dataclasses import dataclass, field
from pathlib import Path

from .errors import AuditError, ErrorCode
from .graph import DependencyGraph, RecomputePlan
from .parameters import ParameterStore, ParamVersion, TierParams
from .slicing import (
    CancelledError,
    SliceArtifact,
    generate_slice,
    quantized_slice_bounds,
)
from .span import SpanMismatch, compare_tier_spans
from .storage import GENESIS_HASH, SliceStore, slice_file_name
from .timebase import SourceTimebase
from .util import canonical_json, content_hash

TRAJ_NAME = "trajectory.jsonl"


@dataclass
class RecomputeOutcome:
    tier: str
    param_version: int
    plan: RecomputePlan
    artifacts: list[SliceArtifact]
    reused: list[dict]
    recomputed: list[dict]
    stale_rejections: list[dict] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "tier": self.tier,
            "param_version": self.param_version,
            "plan": self.plan.to_dict(),
            "reused": self.reused,
            "recomputed": self.recomputed,
            "stale_rejections": self.stale_rejections,
        }


class AuditEngine:
    """单进程内的档位审看引擎（线程安全的任务锁与轨迹追加）。"""

    def __init__(
        self,
        source: SourceTimebase,
        work_dir: str | Path,
        parameter_store: ParameterStore | None = None,
    ) -> None:
        self.source = source
        self.work_dir = Path(work_dir)
        self.work_dir.mkdir(parents=True, exist_ok=True)
        self.store = SliceStore(self.work_dir)
        self.params = parameter_store or ParameterStore(
            self.work_dir / "parameters.json"
        )
        self.graph = DependencyGraph(source)
        self.trajectory_path = self.work_dir / TRAJ_NAME
        self._locks: dict[str, threading.Lock] = {}
        self._active: set[str] = set()
        self._cancel: dict[str, threading.Event] = {}
        self._guard = threading.Lock()
        # 每档最近一次成功状态：param_version -> 已提交切片数
        self.committed: dict[str, dict[int, int]] = {}
        self._manifest_keys: set[tuple[str, int, int]] = set()
        self._manifest_tail = GENESIS_HASH
        self._recover_state()

    # ---- 轨迹日志 -------------------------------------------------------
    def log_event(self, event: str, payload: dict) -> None:
        record = {"event": event, **payload}
        with self._guard:
            with self.trajectory_path.open("ab") as handle:
                handle.write(canonical_json(record) + b"\n")

    # ---- 恢复 -----------------------------------------------------------
    def _recover_state(self) -> None:
        rows, removed_tmp = self.store.recover()
        for row in rows:
            data = row.data
            tier = data["tier"]
            version = data["param_version"]
            self.committed.setdefault(tier, {}).setdefault(version, 0)
            self.committed[tier][version] += 1
            self._manifest_keys.add((tier, version, data["slice_index"]))
            self.graph.register_slice(
                tier,
                version,
                data["slice_index"],
                _safe_params(self.params, tier, version),
            )
            self._manifest_tail = data["row_hash"]
        if rows or removed_tmp:
            self.log_event(
                "recovery",
                {
                    "rows": len(rows),
                    "removed_temp_files": removed_tmp,
                    "graph_version": self.graph.graph_version,
                },
            )

    def _is_committed(self, tier: str, version: int, slice_index: int) -> bool:
        # 清单内已提交的切片不重复写入。
        return (tier, version, slice_index) in self._manifest_keys

    # ---- 任务锁 ---------------------------------------------------------
    def _acquire(self, tier: str) -> threading.Event:
        with self._guard:
            lock = self._locks.setdefault(tier, threading.Lock())
            if tier in self._active:
                raise AuditError(
                    ErrorCode.E_BUSY,
                    f"档位 {tier} 已有重算任务在运行",
                    ("engine", tier),
                )
            if not lock.acquire(blocking=False):
                raise AuditError(
                    ErrorCode.E_BUSY,
                    f"档位 {tier} 任务锁被占用",
                    ("engine", tier),
                )
            self._active.add(tier)
            event = threading.Event()
            self._cancel[tier] = event
            return event

    def _release(self, tier: str) -> None:
        with self._guard:
            self._active.discard(tier)
            self._cancel.pop(tier, None)
            lock = self._locks.get(tier)
        if lock and lock.locked():
            lock.release()

    def request_cancel(self, tier: str) -> bool:
        with self._guard:
            event = self._cancel.get(tier)
        if event is None:
            return False
        event.set()
        self.log_event("cancel_requested", {"tier": tier})
        return True

    # ---- 参数提交与重算 -------------------------------------------------
    def submit_params(self, tier: str, params: TierParams) -> ParamVersion:
        record = self.params.submit(tier, params, origin="new")
        self.log_event(
            "params_submit",
            {
                "tier": tier,
                "version": record.version,
                "origin": record.origin,
                "content_hash": record.content_hash,
            },
        )
        return record

    def rollback_params(self, tier: str, target_version: int) -> ParamVersion:
        record = self.params.rollback(tier, target_version)
        self.log_event(
            "params_rollback",
            {
                "tier": tier,
                "version": record.version,
                "rolled_back_from": target_version,
                "content_hash": record.content_hash,
            },
        )
        return record

    def _previous_version(self, tier: str, current: int) -> ParamVersion | None:
        previous = [
            v for v in self.params.versions(tier) if v.version < current
        ]
        return previous[-1] if previous else None

    def recompute(
        self,
        tier: str,
        version: int | None = None,
    ) -> RecomputeOutcome:
        """对指定档位版本执行增量重算并原子提交。"""

        record = (
            self.params.latest(tier)
            if version is None
            else self.params.get(tier, version)
        )
        cancel_event = self._acquire(tier)
        try:
            return self._recompute_locked(tier, record, cancel_event)
        finally:
            self._release(tier)

    def _recompute_locked(
        self,
        tier: str,
        record: ParamVersion,
        cancel_event: threading.Event,
    ) -> RecomputeOutcome:
        params = record.params
        previous = self._previous_version(tier, record.version)
        old_params = previous.params if previous else None
        new_bounds = quantized_slice_bounds(self.source, params)
        old_count = (
            len(quantized_slice_bounds(self.source, old_params))
            if old_params
            else 0
        )
        plan = self.graph.plan_recompute(
            tier,
            old_params,
            params,
            old_slice_count=old_count,
            new_slice_count=len(new_bounds),
        )

        artifacts: list[SliceArtifact] = [None] * len(new_bounds)  # type: ignore
        reused: list[dict] = []
        recomputed: list[dict] = []
        stale: list[dict] = []

        for index, (start_tick, end_tick) in enumerate(new_bounds):
            if cancel_event.is_set():
                self.log_event(
                    "cancelled",
                    {"tier": tier, "version": record.version, "slice_index": index},
                )
                raise CancelledError(tier, record.version, index)

            self.graph.register_slice(tier, record.version, index, params)
            is_rollback = record.origin == "rollback"
            # 回退：全部切片重算，仅当历史缓存哈希与重算结果一致才允许复用；
            # 普通增量：仅图谱判定可复用且上一版本已提交的切片才尝试缓存。
            reuse_candidate = is_rollback or (
                index in plan.reuse
                and previous is not None
                and self._is_committed(tier, previous.version, index)
            )
            if reuse_candidate:
                cache_version = (
                    record.rolled_back_from
                    if is_rollback and record.rolled_back_from is not None
                    else (previous.version if previous else record.version)
                )
                verdict = self._try_reuse(
                    tier,
                    cache_version,
                    record.version,
                    index,
                    start_tick,
                    end_tick,
                    params,
                    stale,
                    rollback=is_rollback,
                )
                if verdict is not None:
                    artifacts[index] = verdict
                    reused.append(
                        {
                            "slice_index": index,
                            "source_version": previous.version,
                            "content_hash": verdict.content_hash_value,
                        }
                    )
                    continue

            artifact = generate_slice(
                self.source,
                tier,
                record.version,
                params,
                index,
                start_tick,
                end_tick,
            )
            self._commit_artifact(artifact)
            artifacts[index] = artifact
            recomputed.append(
                {
                    "slice_index": index,
                    "content_hash": artifact.content_hash_value,
                    "reason": plan.reasons.get(index, "重算"),
                }
            )

        self.committed.setdefault(tier, {})[record.version] = len(new_bounds)
        outcome = RecomputeOutcome(
            tier=tier,
            param_version=record.version,
            plan=plan,
            artifacts=artifacts,
            reused=reused,
            recomputed=recomputed,
            stale_rejections=stale,
        )
        self.log_event(
            "recompute_done",
            {
                "tier": tier,
                "version": record.version,
                "origin": record.origin,
                "reuse_count": len(reused),
                "recompute_count": len(recomputed),
                "stale_count": len(stale),
                "stale_first": stale[0] if stale else None,
                "content_hashes": [a.content_hash_value for a in artifacts],
                "graph_version": self.graph.graph_version,
            },
        )
        return outcome

    def _try_reuse(
        self,
        tier: str,
        source_version: int,
        target_version: int,
        index: int,
        start_tick: int,
        end_tick: int,
        params: TierParams,
        stale: list[dict],
        rollback: bool,
    ) -> SliceArtifact | None:
        """尝试复用缓存；哈希与重算期望不一致即拒绝并记录首个冲突。"""
        cache_version = source_version
        cached_bytes = (
            self.store.read_slice_bytes(tier, cache_version, index)
            if cache_version is not None
            else None
        )
        if cached_bytes is None:
            stale.append(
                {
                    "slice_index": index,
                    "reason": "回退历史缓存切片缺失" if rollback else "缓存切片文件缺失",
                    "cached_hash": None,
                }
            )
            return None
        try:
            cached = json.loads(cached_bytes)
        except json.JSONDecodeError:
            stale.append(
                {
                    "slice_index": index,
                    "reason": "缓存切片无法解析",
                    "cached_hash": None,
                }
            )
            return None
        # 用目标版本参数重新计算期望内容（不提交），与缓存逐字节比对。
        expected = generate_slice(
            self.source,
            tier,
            target_version,
            params,
            index,
            start_tick,
            end_tick,
        )
        cached_hash = content_hash(cached)
        if cached_hash != expected.content_hash_value:
            stale.append(
                {
                    "slice_index": index,
                    "reason": (
                        "回退缓存哈希与历史版本重算结果不一致"
                        if rollback
                        else "缓存内容哈希与目标参数版本不一致"
                    ),
                    "cached_hash": cached_hash,
                    "expected_hash": expected.content_hash_value,
                    "rollback": rollback,
                }
            )
            self.log_event(
                "stale_cache_rejected",
                {
                    "tier": tier,
                    "version": target_version,
                    "slice_index": index,
                    "cached_hash": cached_hash,
                    "expected_hash": expected.content_hash_value,
                },
            )
            return None
        return expected

    def _commit_artifact(self, artifact: SliceArtifact) -> None:
        key = (
            artifact.tier,
            artifact.param_version,
            artifact.slice_index,
        )
        if key in self._manifest_keys:
            return
        self.store.write_slice_atomic(
            artifact
        )
        row = artifact.manifest_row(self.graph.graph_version)
        row["file_name"] = slice_file_name(
            artifact.tier, artifact.param_version, artifact.slice_index
        )
        record = self.store.append_manifest_row(row, self._manifest_tail)
        self._manifest_tail = record["row_hash"]
        self._manifest_keys.add(key)

    # ---- 跨档校验 -------------------------------------------------------
    def check_spans(self, tier_a: str, tier_b: str) -> list[SpanMismatch]:
        va = self.params.latest(tier_a)
        vb = self.params.latest(tier_b)
        mismatches = compare_tier_spans(
            self.source,
            tier_a,
            va.version,
            va.params,
            tier_b,
            vb.version,
            vb.params,
        )
        self.log_event(
            "span_check",
            {
                "tier_a": tier_a,
                "tier_b": tier_b,
                "mismatch_count": len(mismatches),
                "first_mismatch": (
                    mismatches[0].to_dict() if mismatches else None
                ),
            },
        )
        return mismatches

    def load_trajectory(self) -> list[dict]:
        if not self.trajectory_path.exists():
            return []
        return [
            json.loads(line)
            for line in self.trajectory_path.read_text(encoding="utf-8").splitlines()
            if line.strip()
        ]


def _safe_params(
    store: ParameterStore, tier: str, version: int
) -> TierParams:
    """恢复图谱时容忍参数快照缺失（退回占位参数，仅用于边登记）。"""
    try:
        return store.get(tier, version).params
    except AuditError:
        return TierParams(
            target_rate=16000,
            gain_q8=256,
            quantum_ticks=1000,
            slice_span_ticks=1_000_000,
        )
