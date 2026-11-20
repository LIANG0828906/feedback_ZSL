from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .errors import ErrorCode, fail
from .models import (
    BudgetConfig,
    ProjectConfig,
    TierSpec,
    sha256_json,
    validate_unique_tiers,
)


def _read_object(path: Path, label: str) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise fail(ErrorCode.IO, f"{label} not found", path=str(path)) from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise fail(
            ErrorCode.INVALID_CONFIG,
            f"cannot parse {label}",
            path=str(path),
            error=str(exc),
        ) from exc
    if not isinstance(value, dict):
        raise fail(
            ErrorCode.INVALID_CONFIG,
            f"{label} must be a JSON object",
            path=str(path),
        )
    return value


def _resolve(base: Path, value: str) -> Path:
    candidate = Path(value)
    return candidate if candidate.is_absolute() else (base / candidate).resolve()


def load_project(config_path: Path, budget_path: Path) -> ProjectConfig:
    document = _read_object(config_path, "track configuration")
    budget_document = _read_object(budget_path, "budget configuration")
    source = document.get("source")
    timeline_doc = document.get("timeline")
    tier_values = document.get("tiers")
    if not isinstance(source, dict) or not isinstance(timeline_doc, dict):
        raise fail(
            ErrorCode.INVALID_CONFIG,
            "track configuration requires source and timeline objects",
        )
    if not isinstance(tier_values, list):
        raise fail(ErrorCode.INVALID_CONFIG, "tiers must be an array")
    try:
        audio_rel = str(source["audio"])
        video_rel = str(source["video"])
        slice_duration_ms = int(timeline_doc["slice_duration_ms"])
        tolerance_ms = int(timeline_doc.get("tolerance_ms", 5))
    except (KeyError, TypeError, ValueError) as exc:
        raise fail(
            ErrorCode.INVALID_CONFIG,
            "invalid source or timeline fields",
            error=str(exc),
        ) from exc
    tiers = validate_unique_tiers(TierSpec.from_dict(item) for item in tier_values)
    budget = BudgetConfig.from_dict(budget_document)
    config_version = str(
        document.get(
            "config_version",
            "v1-" + sha256_json({"tiers": [tier.to_dict() for tier in tiers]})[:12],
        )
    )
    return ProjectConfig(
        config_version=config_version,
        config_path=config_path.resolve(),
        audio_path=_resolve(config_path.parent, audio_rel),
        video_path=_resolve(config_path.parent, video_rel),
        slice_duration_ms=slice_duration_ms,
        tolerance_ms=tolerance_ms,
        tiers=tiers,
        budget=budget,
    )
