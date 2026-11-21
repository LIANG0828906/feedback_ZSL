from __future__ import annotations

import json
import math
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any

from .errors import E_CONFIG_INVALID, AppError
from .util import canonical_json_bytes, sha256_bytes

REQUIRED_FIELDS = (
    "max_parallel",
    "per_task_memory_bytes",
    "global_memory_bytes",
    "default_segment_samples",
    "short_term_window_samples",
    "short_term_hop_samples",
    "target_lufs",
    "v1_true_peak_limit_dbtp",
    "quantization_tolerance_db",
    "true_peak_oversample",
    "analysis_algorithm_version",
)


@dataclass(frozen=True)
class Config:
    max_parallel: int
    per_task_memory_bytes: int
    global_memory_bytes: int
    default_segment_samples: int
    short_term_window_samples: int
    short_term_hop_samples: int
    target_lufs: float
    v1_true_peak_limit_dbtp: float
    quantization_tolerance_db: float
    true_peak_oversample: int
    analysis_algorithm_version: str

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @property
    def fingerprint(self) -> str:
        return sha256_bytes(canonical_json_bytes(self.to_dict()))


def _invalid(field: str, message: str) -> AppError:
    return AppError(E_CONFIG_INVALID, message, field=field)


def load_config(path: str | Path | None = None) -> Config:
    if path is None:
        path = Path(__file__).resolve().parent.parent / "config" / "workbench.json"
    try:
        with open(path, "r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except FileNotFoundError as exc:
        raise _invalid("config", f"configuration file not found: {path}") from exc
    except json.JSONDecodeError as exc:
        raise _invalid("config", f"invalid JSON: {exc}") from exc
    return parse_config(raw)


def parse_config(raw: Any) -> Config:
    if not isinstance(raw, dict):
        raise _invalid("$", "configuration must be a JSON object")
    for key in REQUIRED_FIELDS:
        if key not in raw:
            raise _invalid(key, "required configuration field is missing")

    def integer(key: str, minimum: int) -> int:
        value = raw[key]
        if isinstance(value, bool) or not isinstance(value, int):
            raise _invalid(key, "must be an integer")
        if value < minimum:
            raise _invalid(key, f"must be >= {minimum}")
        return value

    def number(key: str, minimum: float, maximum: float) -> float:
        value = raw[key]
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            raise _invalid(key, "must be a finite number")
        result = float(value)
        if not math.isfinite(result) or result < minimum or result > maximum:
            raise _invalid(key, f"must be within [{minimum}, {maximum}]")
        return result

    max_parallel = integer("max_parallel", 1)
    per_task_memory_bytes = integer("per_task_memory_bytes", 1)
    global_memory_bytes = integer("global_memory_bytes", 1)
    if per_task_memory_bytes > global_memory_bytes:
        raise _invalid(
            "per_task_memory_bytes",
            "single-task memory budget exceeds the global memory budget",
        )
    default_segment_samples = integer("default_segment_samples", 1)
    short_term_window_samples = integer("short_term_window_samples", 1)
    short_term_hop_samples = integer("short_term_hop_samples", 1)
    if short_term_hop_samples > short_term_window_samples:
        raise _invalid(
            "short_term_hop_samples",
            "short-term hop must not exceed the short-term window",
        )
    algorithm = raw["analysis_algorithm_version"]
    if not isinstance(algorithm, str) or not algorithm.strip():
        raise _invalid("analysis_algorithm_version", "must be a non-empty string")

    return Config(
        max_parallel=max_parallel,
        per_task_memory_bytes=per_task_memory_bytes,
        global_memory_bytes=global_memory_bytes,
        default_segment_samples=default_segment_samples,
        short_term_window_samples=short_term_window_samples,
        short_term_hop_samples=short_term_hop_samples,
        target_lufs=number("target_lufs", -100.0, 20.0),
        v1_true_peak_limit_dbtp=number("v1_true_peak_limit_dbtp", -100.0, 20.0),
        quantization_tolerance_db=number("quantization_tolerance_db", 0.0, 10.0),
        true_peak_oversample=integer("true_peak_oversample", 1),
        analysis_algorithm_version=algorithm,
    )

