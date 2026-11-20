"""基线加载与逐跳分叉诊断。"""
from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path

from .diff_codec import diff_states
from .engine import TickReport
from .errors import ErrorCode, ReplayError
from .models import RoomState

BASELINE_MODEL_VERSION = 1


@dataclass
class ForkPoint:
    tick: int
    field_path: str
    expected: object
    actual: object
    applied_ops: list[str]
    water_level: int
    baseline_hash: str
    local_hash: str

    def to_dict(self) -> dict:
        return {
            "tick": self.tick,
            "field_path": self.field_path,
            "expected": self.expected,
            "actual": self.actual,
            "applied_ops": self.applied_ops,
            "water_level": self.water_level,
            "baseline_hash": self.baseline_hash,
            "local_hash": self.local_hash,
        }


def load_baseline(path: str | Path, state_model_version: int) -> dict:
    baseline_path = Path(path)
    if not baseline_path.exists():
        raise ReplayError(
            ErrorCode.E_BASELINE_MISSING,
            f"baseline file not found: {baseline_path}",
            field_path=str(baseline_path),
        )
    try:
        payload = json.loads(baseline_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise ReplayError(
            ErrorCode.E_BASELINE_MISSING,
            f"baseline unreadable: {exc.msg}",
            field_path=str(baseline_path),
        ) from exc
    model_version = payload.get("state_model_version")
    if model_version != state_model_version:
        raise ReplayError(
            ErrorCode.E_BASELINE_VERSION,
            f"baseline model {model_version} != replay model {state_model_version}",
            field_path="$.state_model_version",
        )
    if not isinstance(payload, dict) or "entries" not in payload:
        raise ReplayError(
            ErrorCode.E_BASELINE_MISSING,
            "baseline missing entries",
            field_path="$.entries",
        )
    return payload


def compare(
    baseline: dict,
    states_by_tick: dict[int, RoomState],
    reports_by_tick: dict[int, TickReport],
) -> ForkPoint | None:
    """逐条比对；返回首个分叉点，完全一致返回 None。"""
    entries = {int(item["tick"]): item for item in baseline["entries"]}
    for tick in sorted(entries):
        item = entries[tick]
        if tick not in states_by_tick:
            raise ReplayError(
                ErrorCode.E_BASELINE_MISSING,
                f"local replay missing tick {tick}",
                record_index=tick,
                field_path=f"$.entries[tick={tick}]",
            )
        local_state = states_by_tick[tick]
        local_hash = reports_by_tick[tick].state_hash
        baseline_hash = str(item["state_hash"])
        if local_hash != baseline_hash:
            baseline_state = RoomState.from_dict(item["state"])
            mismatches = diff_states(baseline_state, local_state)
            first = mismatches[0] if mismatches else {
                "path": "<hash-only>",
                "expected": baseline_hash,
                "actual": local_hash,
            }
            report = reports_by_tick[tick]
            return ForkPoint(
                tick=tick,
                field_path=first["path"],
                expected=first["expected"],
                actual=first["actual"],
                applied_ops=report.applied_keys(),
                water_level=report.water_level,
                baseline_hash=baseline_hash,
                local_hash=local_hash,
            )
    return None
