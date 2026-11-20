"""v1/v2/v3 录像解析与内部归一。

输入为本地 JSON 录像（dict 或 JSON 文本/路径）。任何字段缺失、未知版本号、
差分引用越界都抛出 :class:`ReplayError`（E_FORMAT_INVALID），并定位记录序号与
字段路径；绝不静默降级为部分解析。异常注入在独立的 injector 入口完成，本模块
只做严格解析，不依赖任何注入开关。
"""
from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .errors import ErrorCode, ReplayError
from .models import (
    RoomState,
    Operation,
    NormalizedReplay,
    VALID_OP_TYPES,
)

SUPPORTED_VERSIONS = frozenset({1, 2, 3})


def _err(message: str, index: int | None, path: str, code: ErrorCode = ErrorCode.E_FORMAT_INVALID) -> ReplayError:
    return ReplayError(code, message, record_index=index, field_path=path)


def _require(mapping: dict, key: str, index: int, path: str) -> Any:
    if key not in mapping or mapping[key] is None:
        raise _err(f"missing required field '{key}'", index, f"{path}.{key}")
    return mapping[key]


def _require_header(mapping: dict, key: str) -> Any:
    if key not in mapping or mapping[key] is None:
        raise _err(f"missing header field '{key}'", None, f"$.{key}")
    return mapping[key]


def _parse_operation(data: dict, index: int) -> Operation:
    base = f"$.records[{index}].op"
    player_id = _require(data, "player_id", index, f"{base}")
    client_seq = _require(data, "client_seq", index, f"{base}")
    arrival_seq = _require(data, "arrival_seq", index, f"{base}")
    tick = _require(data, "tick", index, f"{base}")
    op_type = _require(data, "op_type", index, f"{base}")
    if op_type not in VALID_OP_TYPES:
        raise _err(f"unknown op_type '{op_type}'", index, f"{base}.op_type")
    payload = data.get("payload", {})
    if not isinstance(payload, dict):
        raise _err("op.payload must be an object", index, f"{base}.payload")
    if not isinstance(player_id, int) or not isinstance(client_seq, int) \
            or not isinstance(arrival_seq, int) or not isinstance(tick, int):
        raise _err("op integer fields must be int", index, base)
    return Operation(
        player_id=player_id,
        client_seq=client_seq,
        arrival_seq=arrival_seq,
        tick=tick,
        op_type=op_type,
        payload=payload,
    )


def _parse_state(data: dict, index: int, path: str) -> RoomState:
    if not isinstance(data, dict):
        raise _err("state snapshot must be an object", index, path)
    tick = _require(data, "tick", index, path)
    entities = _require(data, "entities", index, path)
    resources = _require(data, "resources", index, path)
    if not isinstance(entities, dict) or not isinstance(resources, dict):
        raise _err("entities/resources must be objects", index, path)
    for entity_id, entity in entities.items():
        epath = f"{path}.entities.{entity_id}"
        for sub in ("pos", "vel", "alive", "owner"):
            if sub not in entity:
                raise _err(f"entity missing field '{sub}'", index, f"{epath}.{sub}")
        if len(entity["pos"]) != 2 or len(entity["vel"]) != 2:
            raise _err("pos/vel must be length-2 vectors", index, epath)
    state = RoomState.from_dict({"tick": tick, "entities": entities, "resources": resources})
    return state


def load_json(source: str | Path | dict) -> dict:
    if isinstance(source, dict):
        return source
    path = Path(source)
    if not path.exists():
        raise _err(f"replay file not found: {path}", None, "$", ErrorCode.E_FORMAT_INVALID)
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise _err(f"invalid JSON: {exc.msg}", None, f"$@{exc.lineno}:{exc.colno}") from exc


def parse_replay(source: str | Path | dict) -> NormalizedReplay:
    """严格解析任意版本录像并归一为内部表示。"""
    data = load_json(source)
    if not isinstance(data, dict):
        raise _err("replay root must be an object", None, "$")

    if "format_version" not in data:
        raise _err("missing header field 'format_version'", None, "$.format_version")
    version = data["format_version"]
    if not isinstance(version, int):
        raise _err("format_version must be int", None, "$.format_version")
    if version not in SUPPORTED_VERSIONS:
        raise ReplayError(
            ErrorCode.E_FORMAT_VERSION_UNKNOWN,
            f"unknown format_version {version}",
            record_index=None,
            field_path="$.format_version",
        )

    room_id = _require_header(data, "room_id")
    tick_rate = _require_header(data, "tick_rate")
    total_ticks = _require_header(data, "total_ticks")
    late_window = _require_header(data, "late_window")
    initial_raw = _require_header(data, "initial_state")
    records = _require_header(data, "records")
    if not isinstance(records, list):
        raise _err("records must be a list", None, "$.records")

    initial_state = _parse_state(initial_raw, 0, "$.initial_state")

    # v3 强制携带字段字典序与协议能力标记；低版本归一为空列表/默认能力。
    field_order: list[str] = []
    capabilities: list[str] = []
    if version == 3:
        field_order = _require_header(data, "field_order")
        if not isinstance(field_order, list) or not all(isinstance(x, str) for x in field_order):
            raise _err("v3 field_order must be list[str]", None, "$.field_order")
        if field_order != sorted(field_order):
            raise ReplayError(
                ErrorCode.E_DIFF_INVALID,
                "field_order is not in lexicographic order",
                field_path="$.field_order",
            )
        capabilities = _require_header(data, "capabilities")
        if not isinstance(capabilities, list):
            raise _err("v3 capabilities must be a list", None, "$.capabilities")

    operations: list[Operation] = []
    snapshots: dict[int, RoomState] = {}
    events: dict[int, list] = {}
    diffs: dict[int, dict] = {}

    for index, record in enumerate(records):
        rpath = f"$.records[{index}]"
        if not isinstance(record, dict):
            raise _err("record must be an object", index, rpath)
        tick = _require(record, "tick", index, rpath)
        if not isinstance(tick, int) or tick < 0 or tick > total_ticks:
            raise _err(f"record tick {tick} out of range [0,{total_ticks}]", index, f"{rpath}.tick")

        op_raw = _require(record, "op", index, rpath)
        operations.append(_parse_operation(op_raw, index))

        if version >= 2 and (version == 2 or tick not in snapshots):
            snap_raw = _require(record, "snapshot", index, rpath)
            state = _parse_state(snap_raw, index, f"{rpath}.snapshot")
            if state.tick != tick:
                raise _err("snapshot tick mismatch", index, f"{rpath}.snapshot.tick")
            snapshots[tick] = state
            events_raw = record.get("events", [])
            if not isinstance(events_raw, list):
                raise _err("events must be a list", index, f"{rpath}.events")
            events[tick] = events_raw

        if version == 3 and tick not in diffs:
            diff_raw = _require(record, "diff", index, rpath)
            _validate_diff_shape(diff_raw, index, f"{rpath}.diff")
            diffs[tick] = diff_raw

    replay = NormalizedReplay(
        version=version,
        room_id=str(room_id),
        tick_rate=int(tick_rate),
        total_ticks=int(total_ticks),
        late_window=int(late_window),
        capabilities=capabilities,
        field_order=field_order,
        initial_state=initial_state,
        operations=operations,
        snapshots=snapshots,
        events=events,
        diffs=diffs,
    )
    return replay


def _validate_diff_shape(diff: dict, index: int, path: str) -> None:
    if not isinstance(diff, dict):
        raise _err("diff must be an object", index, path)
    base_tick = _require(diff, "base_tick", index, path)
    changes = _require(diff, "changes", index, path)
    if not isinstance(base_tick, int) or base_tick < 0:
        raise _err("diff.base_tick must be non-negative int", index, f"{path}.base_tick")
    if not isinstance(changes, list):
        raise _err("diff.changes must be a list", index, f"{path}.changes")
    paths: list[str] = []
    for change_index, change in enumerate(changes):
        cpath = f"{path}.changes[{change_index}]"
        if not isinstance(change, dict) or "path" not in change or "value" not in change:
            raise _err("each change needs path+value", index, cpath)
        paths.append(change["path"])
    if paths != sorted(paths):
        raise ReplayError(
            ErrorCode.E_DIFF_INVALID,
            "diff change paths not in lexicographic order",
            record_index=index,
            field_path=path,
        )
