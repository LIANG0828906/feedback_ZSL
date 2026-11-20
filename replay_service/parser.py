"""本地 JSON 演练输入解析与严格校验。

任何非法字段都抛出 :class:`ReplayError`（E_INPUT_INVALID），
错误信息定位首个出错位置（配置字段名或操作记录下标），
绝不以默认值静默补齐。
"""

from __future__ import annotations

import json
from numbers import Real

from .errors import E_INPUT_INVALID, ReplayError
from .models import (
    POLICY_DEGRADE,
    POLICY_REJECT,
    ExerciseConfig,
    Operation,
    PenaltyRule,
)

_POLICIES = (POLICY_REJECT, POLICY_DEGRADE)


def _invalid(location, reason):
    raise ReplayError(
        E_INPUT_INVALID,
        f"非法输入 @ {location}: {reason}",
        location=location,
        reason=reason,
    )


def _require_type(obj, key, expected, location):
    if key not in obj:
        _invalid(f"{location}.{key}", f"缺少必需字段 '{key}'")
    value = obj[key]
    # bool 是 int 的子类，配额场景下显式拒绝布尔值。
    if expected in (int, float) and isinstance(value, bool):
        _invalid(location, f"字段 '{key}' 不能是布尔值")
    if not isinstance(value, expected):
        want = getattr(expected, "__name__", str(expected))
        _invalid(location, f"字段 '{key}' 应为 {want}，实际为 {type(value).__name__}")
    return value


def _non_negative_number(value, location, field_name):
    if not isinstance(value, Real) or isinstance(value, bool):
        _invalid(location, f"字段 '{field_name}' 应为数字")
    if value < 0:
        _invalid(location, f"字段 '{field_name}' 不能为负，实际为 {value}")
    return float(value)


def parse_input_file(path) -> tuple[ExerciseConfig, list[Operation]]:
    with open(path, "r", encoding="utf-8") as handle:
        try:
            payload = json.load(handle)
        except json.JSONDecodeError as exc:
            _invalid(f"文档行{exc.lineno}列{exc.colno}", f"JSON 语法错误: {exc.msg}")
    return parse_input(payload)


def parse_input(payload: dict) -> tuple[ExerciseConfig, list[Operation]]:
    if not isinstance(payload, dict):
        _invalid("根对象", "应为 JSON 对象")

    cfg_raw = _require_type(payload, "config", dict, "根对象")
    config = _parse_config(cfg_raw)
    ops_raw = _require_type(payload, "operations", list, "根对象")
    operations = _parse_operations(ops_raw, config)
    return config, operations


def _parse_config(cfg: dict) -> ExerciseConfig:
    loc = "config"
    exercise_id = _require_type(cfg, "exercise_id", str, loc)

    total_steps = _require_type(cfg, "total_steps", int, loc)
    if isinstance(total_steps, bool) or total_steps <= 0:
        _invalid(f"{loc}.total_steps", f"应为正整数，实际为 {total_steps}")

    quota = _non_negative_number(
        _require_type(cfg, "per_step_quota", Real, loc),
        f"{loc}.per_step_quota",
        "per_step_quota",
    )

    player_ids_raw = _require_type(cfg, "player_ids", list, loc)
    if not player_ids_raw:
        _invalid(f"{loc}.player_ids", "玩家列表不能为空")
    player_ids = []
    for index, pid in enumerate(player_ids_raw):
        if not isinstance(pid, str) or not pid:
            _invalid(f"{loc}.player_ids[{index}]", "玩家标识应为非空字符串")
        if pid in player_ids:
            _invalid(f"{loc}.player_ids[{index}]", f"玩家标识重复: {pid}")
        player_ids.append(pid)

    rule_raw = _require_type(cfg, "penalty_rule", dict, loc)
    rule = _parse_penalty_rule(rule_raw)

    return ExerciseConfig(
        exercise_id=exercise_id,
        total_steps=total_steps,
        per_step_quota=quota,
        player_ids=tuple(player_ids),
        penalty_rule=rule,
    )


def _parse_penalty_rule(rule: dict) -> PenaltyRule:
    loc = "config.penalty_rule"
    policy = _require_type(rule, "policy", str, loc)
    if policy not in _POLICIES:
        _invalid(f"{loc}.policy", f"未知裁决策略 {policy!r}，允许值: {_POLICIES}")
    reject_penalty = _non_negative_number(
        _require_type(rule, "reject_penalty", Real, loc),
        f"{loc}.reject_penalty",
        "reject_penalty",
    )
    degrade_rate = _non_negative_number(
        _require_type(rule, "degrade_rate", Real, loc),
        f"{loc}.degrade_rate",
        "degrade_rate",
    )
    degrade_fixed = _non_negative_number(
        _require_type(rule, "degrade_fixed", Real, loc),
        f"{loc}.degrade_fixed",
        "degrade_fixed",
    )
    return PenaltyRule(
        policy=policy,
        reject_penalty=reject_penalty,
        degrade_rate=degrade_rate,
        degrade_fixed=degrade_fixed,
    )


def _parse_operations(raw_ops: list, config: ExerciseConfig) -> list[Operation]:
    operations: list[Operation] = []
    seen_keys: set[tuple[str, int]] = set()
    valid_players = set(config.player_ids)

    for index, raw in enumerate(raw_ops):
        loc = f"operations[{index}]"
        if not isinstance(raw, dict):
            _invalid(loc, "操作记录应为 JSON 对象")

        op_id = _require_type(raw, "op_id", str, loc)
        if not op_id:
            _invalid(f"{loc}.op_id", "操作标识不能为空")

        player_id = _require_type(raw, "player_id", str, loc)
        if player_id not in valid_players:
            _invalid(f"{loc}.player_id", f"未知玩家标识 {player_id!r}")

        client_seq = _require_type(raw, "client_seq", int, loc)
        if isinstance(client_seq, bool) or client_seq < 0:
            _invalid(f"{loc}.client_seq", f"应为非负整数，实际为 {client_seq}")

        effective_step = _require_type(raw, "effective_step", int, loc)
        if isinstance(effective_step, bool) or not (
            0 <= effective_step < config.total_steps
        ):
            _invalid(
                f"{loc}.effective_step",
                f"步号超出范围 [0, {config.total_steps - 1}]，实际为 {effective_step}",
            )

        required = _non_negative_number(
            _require_type(raw, "required_compute", Real, loc),
            f"{loc}.required_compute",
            "required_compute",
        )

        dedup_key = (player_id, client_seq)
        if dedup_key in seen_keys:
            _invalid(
                loc,
                f"玩家 {player_id!r} 的 client_seq={client_seq} 重复",
            )
        seen_keys.add(dedup_key)

        client_ts = raw.get("client_ts")
        arrival_ts = raw.get("arrival_ts")
        for name, ts in (("client_ts", client_ts), ("arrival_ts", arrival_ts)):
            if ts is not None and (not isinstance(ts, Real) or isinstance(ts, bool)):
                _invalid(f"{loc}.{name}", "时间戳应为数字或 null")

        operations.append(
            Operation(
                op_id=op_id,
                player_id=player_id,
                client_seq=client_seq,
                effective_step=effective_step,
                required_compute=required,
                client_ts=None if client_ts is None else float(client_ts),
                arrival_ts=None if arrival_ts is None else float(arrival_ts),
            )
        )

    return operations
