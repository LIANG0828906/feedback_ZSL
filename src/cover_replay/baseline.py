"""Authoritative baseline loading and first-difference attribution."""

from __future__ import annotations

import json
from decimal import Decimal
from pathlib import Path
from typing import Any

from .errors import E_BASELINE_VERSION, ReplayError
from .model import Scenario

REQUIRED_FRAME_FIELDS = (
    "state_hash",
    "crack_events",
    "budget_usage",
    "buffer_watermark_bytes",
)


def load_baseline(path: Path) -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8") as handle:
            data = json.load(handle, parse_float=Decimal)
    except FileNotFoundError:
        raise ReplayError(
            E_BASELINE_VERSION,
            "baseline file not found",
            None,
            "baseline",
            {"path": path.name},
        ) from None
    except json.JSONDecodeError as error:
        raise ReplayError(
            E_BASELINE_VERSION,
            "baseline is not valid JSON",
            None,
            "baseline",
            {"line": error.lineno, "column": error.colno},
        ) from None
    if not isinstance(data, dict):
        raise ReplayError(
            E_BASELINE_VERSION,
            "baseline root must be an object",
            None,
            "baseline",
        )
    return data


class BaselineComparator:
    def __init__(self, scenario: Scenario, baseline: dict[str, Any]) -> None:
        self.scenario = scenario
        self.baseline = baseline
        self._validate_versions()
        frames = baseline.get("frames")
        if not isinstance(frames, list):
            raise ReplayError(
                E_BASELINE_VERSION,
                "baseline frames must be an array",
                None,
                "baseline.frames",
            )
        self.frames = frames
        self.first_difference: dict[str, Any] | None = None

    def _validate_versions(self) -> None:
        schema_version = self.baseline.get("schema_version")
        if schema_version is not None and schema_version != 1:
            raise ReplayError(
                E_BASELINE_VERSION,
                "unsupported baseline schema version",
                None,
                "baseline.schema_version",
                {"expected": 1, "actual": schema_version},
            )
        baseline_version = self.baseline.get("baseline_version")
        if baseline_version != self.scenario.config.baseline_version:
            raise ReplayError(
                E_BASELINE_VERSION,
                "baseline version does not match scenario configuration",
                None,
                "baseline.baseline_version",
                {
                    "expected": self.scenario.config.baseline_version,
                    "actual": baseline_version,
                },
            )
        damage_model_version = self.baseline.get("damage_model_version")
        if damage_model_version != self.scenario.config.damage_model_version:
            raise ReplayError(
                E_BASELINE_VERSION,
                "damage model version does not match scenario configuration",
                None,
                "baseline.damage_model_version",
                {
                    "expected": self.scenario.config.damage_model_version,
                    "actual": damage_model_version,
                },
            )

    def compare(self, frame: dict[str, Any]) -> dict[str, Any] | None:
        if self.first_difference is not None:
            return self.first_difference
        step = frame["step"]
        if step >= len(self.frames):
            difference = {
                "step": step,
                "field_path": f"frames[{step}]",
                "expected": "<missing frame>",
                "actual": "present",
                "effective_shot_client_sequence": frame.get(
                    "effective_shot_client_sequence", []
                ),
                "buffer_watermark_bytes": frame.get("buffer_watermark_bytes"),
            }
            self.first_difference = difference
            return difference
        expected = self.frames[step]
        if not isinstance(expected, dict):
            difference = {
                "step": step,
                "field_path": f"frames[{step}]",
                "expected": "<object>",
                "actual": type(expected).__name__,
                "effective_shot_client_sequence": frame.get(
                    "effective_shot_client_sequence", []
                ),
                "buffer_watermark_bytes": frame.get("buffer_watermark_bytes"),
            }
            self.first_difference = difference
            return difference
        for field_name in REQUIRED_FRAME_FIELDS:
            if field_name not in expected:
                difference = {
                    "step": step,
                    "field_path": f"frames[{step}].{field_name}",
                    "expected": "<missing>",
                    "actual": frame.get(field_name),
                    "effective_shot_client_sequence": frame.get(
                        "effective_shot_client_sequence", []
                    ),
                    "buffer_watermark_bytes": frame.get("buffer_watermark_bytes"),
                }
                self.first_difference = difference
                return difference
            relative_path, expected_value, actual_value = _first_value_difference(
                expected[field_name],
                frame.get(field_name),
                field_name,
            )
            if relative_path is not None:
                difference = {
                    "step": step,
                    "field_path": f"frames[{step}].{relative_path}",
                    "expected": expected_value,
                    "actual": actual_value,
                    "effective_shot_client_sequence": frame.get(
                        "effective_shot_client_sequence", []
                    ),
                    "buffer_watermark_bytes": frame.get("buffer_watermark_bytes"),
                }
                self.first_difference = difference
                return difference
        return None

    def report(self) -> dict[str, Any]:
        return {
            "baseline_version": self.baseline.get("baseline_version"),
            "damage_model_version": self.baseline.get("damage_model_version"),
            "status": "match" if self.first_difference is None else "mismatch",
            "first_difference": self.first_difference,
        }


def _first_value_difference(
    expected: Any,
    actual: Any,
    path: str,
) -> tuple[str | None, Any, Any]:
    if isinstance(expected, dict) and isinstance(actual, dict):
        for key in expected:
            if key not in actual:
                return f"{path}.{key}", expected[key], "<missing>"
            child_path, expected_value, actual_value = _first_value_difference(
                expected[key], actual[key], f"{path}.{key}"
            )
            if child_path is not None:
                return child_path, expected_value, actual_value
        for key in actual:
            if key not in expected:
                return f"{path}.{key}", "<missing>", actual[key]
        return None, None, None
    if isinstance(expected, list) and isinstance(actual, list):
        for index in range(min(len(expected), len(actual))):
            child_path, expected_value, actual_value = _first_value_difference(
                expected[index], actual[index], f"{path}[{index}]"
            )
            if child_path is not None:
                return child_path, expected_value, actual_value
        if len(expected) != len(actual):
            return f"{path}.length", len(expected), len(actual)
        return None, None, None
    if expected != actual:
        return path, expected, actual
    return None, None, None

