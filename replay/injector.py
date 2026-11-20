"""异常注入验证通道（数据处理入口）。

所有注入都发生在解析/推进/落盘之前，绝不修改结算器或快照模块源码。
每类故障返回独立业务码并附注入点定位；对于“在任何状态文件/哈希链写出前拦截”
的要求，注入器先校验故障并抛出，再允许进入管线。
"""
from __future__ import annotations

import json
from pathlib import Path

from .errors import ErrorCode, ReplayError
from .models import WORLD_LIMIT

FAULT_TYPES = frozenset(
    {
        "duplicate_op",
        "out_of_bounds",
        "late_op",
        "truncated_diff",
        "corrupt_snapshot",
        "format_order",
    }
)


def _point(fault: str, detail: str) -> str:
    return f"inject:{fault}:{detail}"


def validate_injection(raw: dict, fault: str, record_index: int = 0, extra: dict | None = None) -> None:
    """入口校验：命中故障立即抛错，发生在解析器与任何文件写出之前。"""
    extra = extra or {}
    point = _point(fault, f"record#{record_index}")
    if fault == "duplicate_op":
        _assert_fault_present(
            raw, record_index, lambda data: _has_duplicate(data),
            "client_seq", point,
        )
        raise ReplayError(
            ErrorCode.E_OP_DUPLICATE,
            "injected duplicate client_seq rejected at ingress",
            record_index=record_index,
            field_path=f"$.records[{record_index}].op.client_seq",
            injection_point=point,
        )
    if fault == "out_of_bounds":
        coord = _detect_oob_coord(raw, record_index)
        if coord is None:
            raise ReplayError(
                ErrorCode.E_FORMAT_INVALID,
                "out_of_bounds injection declared but no illegal coordinate found",
                record_index=record_index,
                injection_point=point,
            )
        raise ReplayError(
            ErrorCode.E_OP_OUT_OF_BOUNDS,
            f"injected coordinate {coord} outside [-{WORLD_LIMIT},{WORLD_LIMIT}]",
            record_index=record_index,
            field_path=f"$.records[{record_index}].op.payload",
            injection_point=point,
            detail={"coord": coord},
        )
    if fault == "late_op":
        window = int(raw.get("late_window", extra.get("late_window", 0)))
        op_tick = _safe_index(raw, record_index)
        if op_tick is None or op_tick >= 0:
            raise ReplayError(
                ErrorCode.E_FORMAT_INVALID,
                "late_op injection declared but op tick is within declared window",
                record_index=record_index,
                injection_point=point,
            )
        raise ReplayError(
            ErrorCode.E_OP_LATE,
            "injected operation exceeds declared late window",
            record_index=record_index,
            field_path=f"$.records[{record_index}].op.tick",
            injection_point=point,
            detail={"late_window": window},
        )
    if fault == "truncated_diff":
        record = _safe_record(raw, record_index)
        diff = record.get("diff", {}) if record else {}
        if not diff.get("truncated"):
            raise ReplayError(
                ErrorCode.E_FORMAT_INVALID,
                "truncated_diff injection declared but diff not marked truncated",
                record_index=record_index,
                injection_point=point,
            )
        raise ReplayError(
            ErrorCode.E_DIFF_TRUNCATED,
            "injected truncated v3 diff record intercepted before decode",
            record_index=record_index,
            field_path=f"$.records[{record_index}].diff.changes",
            injection_point=point,
        )
    if fault == "format_order":
        if not _has_order_fault(raw):
            raise ReplayError(
                ErrorCode.E_FORMAT_INVALID,
                "format_order injection declared but records are tick-monotonic",
                record_index=record_index,
                injection_point=point,
            )
        raise ReplayError(
            ErrorCode.E_FORMAT_ORDER,
            "injected out-of-order replay record intercepted at ingress",
            record_index=record_index,
            field_path=f"$.records[{record_index}].tick",
            injection_point=point,
        )
    if fault == "corrupt_snapshot":
        # 快照损坏属于恢复阶段故障：入口先确认快照文件存在，再由恢复器按
        # E_RECOVERY_INVALID 拦截；这里给出明确注入点，不允许静默跳过。
        snapshot_dir = Path(extra["snapshot_dir"])
        target = snapshot_dir / f"snapshot_{int(extra['tick']):08d}.json"
        if not target.exists():
            raise ReplayError(
                ErrorCode.E_RECOVERY_INVALID,
                f"corrupt-snapshot target missing: {target}",
                record_index=int(extra["tick"]),
                field_path=str(target),
                injection_point=_point(fault, f"tick#{extra['tick']}"),
            )
        return
    raise ReplayError(
        ErrorCode.E_FORMAT_INVALID,
        f"unknown injection fault '{fault}'",
        injection_point=f"inject:{fault}",
    )


def _assert_fault_present(raw: dict, index: int, predicate, field: str, point: str) -> None:
    if not raw or not predicate(raw):
        raise ReplayError(
            ErrorCode.E_FORMAT_INVALID,
            f"injection declared but fault condition absent for field '{field}'",
            record_index=index,
            injection_point=point,
        )


def _safe_record(raw: dict, index: int):
    records = raw.get("records") if isinstance(raw, dict) else None
    if isinstance(records, list) and 0 <= index < len(records):
        return records[index]
    return None


def _safe_index(raw: dict, index: int):
    record = _safe_record(raw, index)
    return record.get("op", {}).get("tick") if record else None


def _has_duplicate(raw: dict) -> bool:
    seen: set[tuple] = set()
    for record in raw.get("records", []):
        op = record.get("op", {})
        key = (op.get("player_id"), op.get("client_seq"))
        if key in seen:
            return True
        seen.add(key)
    return False


def _detect_oob_coord(raw: dict, index: int):
    record = _safe_record(raw, index)
    if not record:
        return None
    payload = record.get("op", {}).get("payload", {})
    for key in ("dx", "dy"):
        value = payload.get(key)
        if isinstance(value, (int, float)) and abs(value) > WORLD_LIMIT:
            return float(value)
    return None


def _has_order_fault(raw: dict) -> bool:
    ticks = [record.get("tick") for record in raw.get("records", [])]
    return any(ticks[i] < ticks[i - 1] for i in range(1, len(ticks)))


def apply_corrupt_snapshot(snapshot_dir: str | Path, tick: int) -> Path:
    """对已提交快照做确定性字节损坏（仅测试通道调用）。"""
    target = Path(snapshot_dir) / f"snapshot_{int(tick):08d}.json"
    payload = json.loads(target.read_text(encoding="utf-8"))
    payload["chain_prefix"] = "f" * 64
    payload["prev_hash"] = "f" * 64
    target.write_text(json.dumps(payload), encoding="utf-8")
    return target


def build_faulty_replay(base: dict, fault: str, record_index: int = 0) -> dict:
    """在内存样本上构造故障录像副本（不改动原始样本）。"""
    copy = json.loads(json.dumps(base))
    records = copy["records"]
    if fault == "duplicate_op":
        clone = json.loads(json.dumps(records[record_index]))
        records.insert(record_index + 1, clone)
    elif fault == "out_of_bounds":
        records[record_index]["op"]["payload"]["dx"] = WORLD_LIMIT + 50.0
    elif fault == "late_op":
        records[record_index]["op"]["tick"] = -copy["late_window"] - 1
    elif fault == "truncated_diff":
        records[record_index]["diff"]["changes"] = records[record_index]["diff"]["changes"][:-1]
        records[record_index]["diff"]["truncated"] = True
    elif fault == "format_order":
        if record_index + 1 < len(records):
            records[record_index], records[record_index + 1] = (
                records[record_index + 1],
                records[record_index],
            )
    return copy
