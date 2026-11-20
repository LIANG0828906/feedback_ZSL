"""v3 增量差分编码 / 解码与跨版本状态重建。

差分以字段路径（点路径）承载，changes 必须严格按路径字典序排列；解码从
base_tick 引用的完整状态出发逐条 set-path 还原。引用无法解析、字典序与状态
字段不匹配时抛 E_DIFF_INVALID，绝不产出错误状态。
"""
from __future__ import annotations

from .errors import ErrorCode, ReplayError
from .models import RoomState, canonical_dumps


def _set_path(root: dict, dotted: str, value, index: int) -> None:
    parts = dotted.split(".")
    cursor = root
    for depth, part in enumerate(parts[:-1]):
        if part not in cursor or not isinstance(cursor[part], dict):
            raise ReplayError(
                ErrorCode.E_DIFF_INVALID,
                f"diff path '{dotted}' cannot resolve container '{part}'",
                record_index=index,
                field_path=dotted,
            )
        cursor = cursor[part]
    leaf = parts[-1]
    if leaf not in cursor:
        raise ReplayError(
            ErrorCode.E_DIFF_INVALID,
            f"diff path '{dotted}' references unknown field '{leaf}'",
            record_index=index,
            field_path=dotted,
        )
    cursor[leaf] = value


def _collect_state_paths(payload: dict, prefix: str = "") -> list[str]:
    """枚举规范化状态中所有叶子字段路径（字典序）。"""
    leaves: list[str] = []
    for key in sorted(payload):
        value = payload[key]
        path = f"{prefix}.{key}" if prefix else key
        if isinstance(value, dict):
            leaves.extend(_collect_state_paths(value, path))
        else:
            leaves.append(path)
    return leaves


def validate_field_order(field_order: list[str], state: RoomState) -> None:
    """v3 声明的字段字典序必须与状态字段集合一致且有序，否则 E_DIFF_INVALID。"""
    actual = _collect_state_paths(state.canonical_payload())
    if field_order != sorted(field_order):
        raise ReplayError(
            ErrorCode.E_DIFF_INVALID,
            "declared field_order not lexicographically sorted",
            field_path="$.field_order",
        )
    if field_order != actual:
        missing = sorted(set(actual) - set(field_order))
        extra = sorted(set(field_order) - set(actual))
        raise ReplayError(
            ErrorCode.E_DIFF_INVALID,
            "field_order does not match state fields",
            field_path="$.field_order",
            detail={"missing": missing, "extra": extra},
        )


def decode_diff(base: RoomState, diff: dict, index: int, target_tick: int) -> RoomState:
    """从 base 状态与一条 v3 差分记录还原目标完整状态。"""
    base_tick = diff.get("base_tick")
    changes = diff.get("changes")
    if not isinstance(base_tick, int) or not isinstance(changes, list):
        raise ReplayError(
            ErrorCode.E_DIFF_INVALID,
            "malformed diff record",
            record_index=index,
            field_path="$.diff",
        )
    if base_tick != base.tick:
        raise ReplayError(
            ErrorCode.E_DIFF_INVALID,
            f"diff base_tick {base_tick} unresolvable (have {base.tick})",
            record_index=index,
            field_path="$.diff.base_tick",
        )
    paths = [change.get("path") for change in changes]
    if paths != sorted(paths):
        raise ReplayError(
            ErrorCode.E_DIFF_INVALID,
            "diff changes not in lexicographic order",
            record_index=index,
            field_path="$.diff.changes",
        )
    payload = base.canonical_payload()
    for change in changes:
        _set_path(payload, change["path"], change["value"], index)
    payload["tick"] = target_tick
    return RoomState.from_dict(payload)


def encode_diff(base: RoomState, target: RoomState) -> dict:
    """生成确定性 v3 差分：仅包含发生变化的叶子路径，按字典序排列。"""
    base_payload = base.canonical_payload()
    target_payload = target.canonical_payload()
    base_leaves = dict(_leaf_map(base_payload))
    target_leaves = dict(_leaf_map(target_payload))
    changes = []
    for path in sorted(target_leaves):
        if base_leaves.get(path) != target_leaves[path]:
            changes.append({"path": path, "value": target_leaves[path]})
    return {"base_tick": base.tick, "changes": changes}


def _leaf_map(payload: dict, prefix: str = ""):
    for key in sorted(payload):
        value = payload[key]
        path = f"{prefix}.{key}" if prefix else key
        if isinstance(value, dict):
            yield from _leaf_map(value, path)
        else:
            yield path, value


def diff_states(expected: RoomState, actual: RoomState) -> list[dict]:
    """逐字段比对两个状态，返回所有叶子级差异（字段路径 + 两侧取值）。"""
    left = dict(_leaf_map(expected.canonical_payload()))
    right = dict(_leaf_map(actual.canonical_payload()))
    mismatches = []
    for path in sorted(set(left) | set(right)):
        if left.get(path) != right.get(path):
            mismatches.append({"path": path, "expected": left.get(path), "actual": right.get(path)})
    return mismatches


def assert_byte_identical(states_a: list[RoomState], states_b: list[RoomState]) -> None:
    """调试辅助：两条状态序列规范化字节必须逐字节一致。"""
    for index, (a, b) in enumerate(zip(states_a, states_b)):
        if canonical_dumps(a.canonical_payload()) != canonical_dumps(b.canonical_payload()):
            raise AssertionError(f"state divergence at tick index {index}")
