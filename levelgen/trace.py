"""Append-only hash-chained traces and deterministic replay."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from .canonical import ZERO_HASH, content_hash, normalize_json
from .errors import E_TRACE_INVALID, LevelGenError
from .models import GenerationRequest, Level


TRACE_FORMAT = "levelgen-trace/1"


class TraceRecorder:
    def __init__(self, request: GenerationRequest) -> None:
        self._entries: list[dict[str, Any]] = []
        self._previous_hash = ZERO_HASH
        self._append(
            "input",
            {
                "input_hash": request.input_hash,
            },
        )

    def _append(self, entry_type: str, payload: dict[str, Any]) -> dict[str, Any]:
        entry: dict[str, Any] = normalize_json(
            {
                "prev": self._previous_hash,
                "seq": len(self._entries),
                "type": entry_type,
                **payload,
            }
        )
        entry["hash"] = content_hash(entry)
        self._entries.append(entry)
        self._previous_hash = entry["hash"]
        return entry

    @property
    def previous_hash(self) -> str:
        return self._previous_hash

    def stage_start(self, stage: str, attempt: int, snapshot_hash: str) -> None:
        self._append(
            "stage_start",
            {
                "attempt": attempt,
                "snapshot_hash": snapshot_hash,
                "stage": stage,
            },
        )

    def stage_end(self, stage: str, attempt: int, level: Level) -> None:
        self._append(
            "stage_end",
            {
                "attempt": attempt,
                "content_hash": level.content_hash,
                "stage": stage,
            },
        )

    def stage_failure(
        self,
        stage: str,
        attempt: int,
        error: LevelGenError,
    ) -> None:
        self._append(
            "stage_failure",
            {
                "attempt": attempt,
                "message": error.message,
                "report": error.report,
                "stage": stage,
            },
        )

    def stage_rollback(
        self,
        stage: str,
        attempt: int,
        restored_hash: str,
    ) -> None:
        self._append(
            "stage_rollback",
            {
                "attempt": attempt,
                "restored_hash": restored_hash,
                "stage": stage,
            },
        )

    def operation(self, stage: str, attempt: int, op: str, data: Any) -> None:
        self._append(
            "op",
            {
                "attempt": attempt,
                "data": data,
                "op": op,
                "stage": stage,
            },
        )

    def rng(
        self,
        method: str,
        args: list[Any],
        result: Any,
        stream: str,
    ) -> None:
        self._append(
            "rng",
            {
                "args": list(args),
                "method": method,
                "result": result,
                "stream": stream,
            },
        )

    def check(self, stage: str, result: dict[str, Any]) -> None:
        self._append(
            "check",
            {
                "result": result,
                "stage": stage,
            },
        )

    def conflict(self, report: dict[str, Any]) -> None:
        self._append("conflict", {"report": report})

    def final(self, level: Level) -> None:
        self._append(
            "final",
            {
                "level_hash": level.content_hash,
            },
        )

    def as_dict(self, request: GenerationRequest) -> dict[str, Any]:
        return {
            "entries": [dict(entry) for entry in self._entries],
            "format": TRACE_FORMAT,
            "input": request.to_dict(),
        }


def validate_trace(trace: Any) -> None:
    if not isinstance(trace, dict):
        raise LevelGenError(
            E_TRACE_INVALID,
            "轨迹必须是 JSON 对象",
            point="trace",
        )
    if trace.get("format") != TRACE_FORMAT:
        raise LevelGenError(
            E_TRACE_INVALID,
            f"轨迹格式必须是 {TRACE_FORMAT}",
            point="trace.format",
        )
    if not isinstance(trace.get("input"), dict):
        raise LevelGenError(
            E_TRACE_INVALID,
            "轨迹缺少输入请求",
            point="trace.input",
        )
    entries = trace.get("entries")
    if not isinstance(entries, list) or not entries:
        raise LevelGenError(
            E_TRACE_INVALID,
            "轨迹缺少条目",
            point="trace.entries",
        )

    previous = ZERO_HASH
    for index, entry in enumerate(entries):
        point = f"trace.entries[{index}]"
        if not isinstance(entry, dict):
            raise LevelGenError(E_TRACE_INVALID, "轨迹条目必须是对象", point=point)
        if entry.get("seq") != index:
            raise LevelGenError(
                E_TRACE_INVALID,
                f"轨迹序号断裂：期望 {index}，实际 {entry.get('seq')}",
                point=f"{point}.seq",
            )
        if entry.get("prev") != previous:
            raise LevelGenError(
                E_TRACE_INVALID,
                "轨迹哈希断链：prev 与上一条内容哈希不一致",
                point=f"{point}.prev",
            )
        recorded_hash = entry.get("hash")
        if not isinstance(recorded_hash, str):
            raise LevelGenError(
                E_TRACE_INVALID,
                "轨迹条目缺少内容哈希",
                point=f"{point}.hash",
            )
        expected_hash = content_hash(
            {key: value for key, value in entry.items() if key != "hash"}
        )
        if recorded_hash != expected_hash:
            raise LevelGenError(
                E_TRACE_INVALID,
                "轨迹内容哈希不匹配",
                point=f"{point}.hash",
            )
        previous = recorded_hash

    if entries[0].get("type") != "input":
        raise LevelGenError(
            E_TRACE_INVALID,
            "轨迹首条必须是 input",
            point="trace.entries[0].type",
        )
    if entries[0].get("input_hash") != content_hash(trace["input"]):
        raise LevelGenError(
            E_TRACE_INVALID,
            "轨迹 input_hash 与输入请求不一致",
            point="trace.entries[0].input_hash",
        )
    final_entries = [entry for entry in entries if entry.get("type") == "final"]
    conflict_entries = [entry for entry in entries if entry.get("type") == "conflict"]
    if not final_entries and not conflict_entries:
        raise LevelGenError(
            E_TRACE_INVALID,
            "轨迹没有 final 或 conflict 终态",
            point="trace.entries",
        )


def trace_level_hash(trace: dict[str, Any]) -> str:
    validate_trace(trace)
    final_entries = [
        entry for entry in trace["entries"] if entry.get("type") == "final"
    ]
    if not final_entries:
        raise LevelGenError(
            E_TRACE_INVALID,
            "冲突轨迹没有终局关卡",
            point="trace.entries",
        )
    level_hash = final_entries[-1].get("level_hash")
    if not isinstance(level_hash, str):
        raise LevelGenError(
            E_TRACE_INVALID,
            "final 条目缺少 level_hash",
            point="trace.entries",
        )
    return level_hash


def trace_content_hash(trace: dict[str, Any]) -> str:
    validate_trace(trace)
    return content_hash(trace)


@dataclass
class ReplayResult:
    level: Level | None
    trace: dict[str, Any]
    conflict: dict[str, Any] | None = None

    @property
    def ok(self) -> bool:
        return self.level is not None


def replay_trace(trace: dict[str, Any]) -> ReplayResult:
    """Regenerate from the embedded input and compare the complete hash chain."""

    validate_trace(trace)
    from .generator import generate

    request = GenerationRequest.from_dict(trace["input"])
    regenerated = generate(request.to_dict())
    expected_trace = regenerated.trace
    original_entries = trace["entries"]
    expected_entries = expected_trace["entries"]
    divergence = 0
    limit = min(len(original_entries), len(expected_entries))
    while (
        divergence < limit
        and original_entries[divergence] == expected_entries[divergence]
    ):
        divergence += 1
    if (
        len(original_entries) != len(expected_entries)
        or divergence < limit
    ):
        raise LevelGenError(
            E_TRACE_INVALID,
            "轨迹与确定性复算结果不一致",
            point=f"trace.entries[{divergence}]",
        )
    if expected_trace != trace:
        raise LevelGenError(
            E_TRACE_INVALID,
            "轨迹内容与确定性复算结果不一致",
            point="trace",
        )
    if not regenerated.ok:
        return ReplayResult(
            level=None,
            trace=expected_trace,
            conflict=regenerated.conflict,
        )
    if regenerated.level is None:
        raise LevelGenError(
            E_TRACE_INVALID,
            "复算未产出关卡",
            point="trace",
        )
    if trace_level_hash(trace) != regenerated.level.content_hash:
        raise LevelGenError(
            E_TRACE_INVALID,
            "终局关卡哈希断言失败",
            point="trace.entries.final.level_hash",
        )
    return ReplayResult(level=regenerated.level, trace=expected_trace)
