"""Structured run diagnostics and incremental-vs-full audit results."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class StageDiagnostic:
    stage: str
    display_name: str
    status: str
    recomputes: int = 0
    hits: int = 0
    elapsed_sec: float = 0.0
    invalidated: bool = False
    cache_key: str | None = None
    error: dict[str, Any] | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "stage": self.stage,
            "display_name": self.display_name,
            "status": self.status,
            "recomputes": self.recomputes,
            "hits": self.hits,
            "elapsed_sec": round(self.elapsed_sec, 6),
            "invalidated": self.invalidated,
            "cache_key": self.cache_key,
            "error": self.error,
        }


@dataclass
class RunDiagnostics:
    run_id: str
    path_type: str
    source: str
    status: str
    stages: list[StageDiagnostic]
    started_at: str
    finished_at: str
    total_elapsed_sec: float
    effective_cache_entries: int
    full_stage_count: int
    cache_hit_count: int
    recompute_count: int
    snapshot_path: str
    snapshot_writes: int = 0
    chain_complete: bool = False
    restored_from_snapshot: bool = False
    previous_snapshot_complete: bool | None = None
    max_parallelism: int = 1
    error: dict[str, Any] | None = None
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "run_id": self.run_id,
            "path_type": self.path_type,
            "source": self.source,
            "status": self.status,
            "stages": [stage.to_dict() for stage in self.stages],
            "started_at": self.started_at,
            "finished_at": self.finished_at,
            "total_elapsed_sec": round(self.total_elapsed_sec, 6),
            "effective_cache_entries": self.effective_cache_entries,
            "full_stage_count": self.full_stage_count,
            "cache_hit_count": self.cache_hit_count,
            "recompute_count": self.recompute_count,
            "snapshot_path": self.snapshot_path,
            "snapshot_writes": self.snapshot_writes,
            "chain_complete": self.chain_complete,
            "restored_from_snapshot": self.restored_from_snapshot,
            "previous_snapshot_complete": self.previous_snapshot_complete,
            "max_parallelism": self.max_parallelism,
            "error": self.error,
            "notes": list(self.notes),
        }

    def summary_text(self) -> str:
        lines = [
            f"运行: {self.run_id}  路径: {self.path_type}  状态: {self.status}",
            (
                f"缓存: {self.effective_cache_entries}/{self.full_stage_count}  "
                f"命中: {self.cache_hit_count}  重算: {self.recompute_count}  "
                f"并行度: {self.max_parallelism}"
            ),
            f"总耗时: {self.total_elapsed_sec:.4f}s  快照提交: {self.snapshot_writes}",
        ]
        for stage in self.stages:
            lines.append(
                f"- {stage.display_name:<6} {stage.status:<11} "
                f"重算={stage.recomputes} 命中={stage.hits} "
                f"耗时={stage.elapsed_sec:.4f}s"
                + (" [失效]" if stage.invalidated else "")
            )
        if self.error:
            lines.append(f"错误: {self.error}")
        lines.extend(self.notes)
        return "\n".join(lines)


@dataclass
class ComparisonReport:
    equal: bool
    code: str | None
    message: str
    stage: str | None = None
    x: int | None = None
    y: int | None = None
    channel: int | None = None
    compared_stages: list[str] = field(default_factory=list)
    incremental_cache_entries: int = 0
    full_stage_count: int = 0
    full_diagnostics: RunDiagnostics | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "equal": self.equal,
            "code": self.code,
            "message": self.message,
            "stage": self.stage,
            "x": self.x,
            "y": self.y,
            "channel": self.channel,
            "compared_stages": list(self.compared_stages),
            "incremental_cache_entries": self.incremental_cache_entries,
            "full_stage_count": self.full_stage_count,
            "full_diagnostics": (
                self.full_diagnostics.to_dict() if self.full_diagnostics else None
            ),
        }

