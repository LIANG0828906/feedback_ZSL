from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parent.parent
QUICK_PLAN = ROOT / "configs" / "quick" / "change-plan.json"
QUICK_SLO = ROOT / "configs" / "quick" / "slo.json"
SUCCESS_INJECTION = ROOT / "configs" / "injections" / "release-success.json"
ROLLBACK_INJECTION = ROOT / "configs" / "injections" / "release-rollback.json"
REFUSAL_INJECTION = ROOT / "configs" / "injections" / "release-failure.json"
ABORT_INJECTION = ROOT / "configs" / "injections" / "release-abort.json"


def _read(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))


def write_json(path: Path, value: dict[str, Any]) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    return path


def quick_plan_copy(directory: Path, **changes: Any) -> Path:
    value = copy.deepcopy(_read(QUICK_PLAN))
    value.update(changes)
    value["storage"] = {
        "database_path": str(directory / "state.sqlite3"),
        "report_path": str(directory / "report.json"),
    }
    return write_json(directory / "plan.json", value)


def quick_slo_copy(directory: Path, **changes: Any) -> Path:
    value = copy.deepcopy(_read(QUICK_SLO))
    value.update(changes)
    return write_json(directory / "slo.json", value)


def write_injection(
    directory: Path,
    base: Path,
    **changes: Any,
) -> Path:
    value = copy.deepcopy(_read(base))
    value.update(changes)
    return write_json(directory / "injections.json", value)


def normalize_timings(value: Any) -> Any:
    if isinstance(value, dict):
        return {
            key: normalize_timings(item)
            for key, item in value.items()
            if key
            not in {
                "release_duration_ms",
                "rollback_duration_ms",
                "duration_ms",
            }
        }
    if isinstance(value, list):
        return [normalize_timings(item) for item in value]
    return value
