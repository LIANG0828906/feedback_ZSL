"""Filesystem orchestration shared by CLI, headless mode and the GUI."""

from __future__ import annotations

import json
import shutil
from pathlib import Path
from typing import Any

from .canonical import content_hash, read_json, write_json_atomic
from .errors import E_IO, E_TRACE_INVALID, LevelGenError
from .generator import GenerationResult, generate
from .injection import InjectionResult, run_injection
from .trace import replay_trace, validate_trace


OUTPUT_NAMES = (
    "conflict.json",
    "error.json",
    "level.json",
    "replay_result.json",
    "result.json",
    "trace.json",
)


def clear_output_directory(output_dir: str | Path, *, full: bool = False) -> None:
    destination = Path(output_dir)
    if full and destination.exists():
        shutil.rmtree(destination)
    destination.mkdir(parents=True, exist_ok=True)
    if not full:
        for name in OUTPUT_NAMES:
            (destination / name).unlink(missing_ok=True)


def write_generation_outputs(
    result: GenerationResult,
    output_dir: str | Path,
) -> dict[str, Any]:
    destination = Path(output_dir)
    destination.mkdir(parents=True, exist_ok=True)
    (destination / "level.json").unlink(missing_ok=True)
    if result.trace is not None:
        write_json_atomic(destination / "trace.json", result.trace)
    if result.ok and result.level is not None:
        write_json_atomic(destination / "level.json", result.level.to_dict())
        (destination / "conflict.json").unlink(missing_ok=True)
        (destination / "error.json").unlink(missing_ok=True)
    else:
        if result.conflict is not None:
            write_json_atomic(destination / "conflict.json", result.conflict)
        if result.error is not None:
            write_json_atomic(destination / "error.json", result.error.to_dict())
    write_json_atomic(destination / "result.json", result.to_dict())
    return result.to_dict()


def run_generation_to_directory(
    request: dict[str, Any],
    output_dir: str | Path,
    *,
    clean: bool = True,
) -> GenerationResult:
    clear_output_directory(output_dir)
    result = generate(request)
    write_generation_outputs(result, output_dir)
    return result


def run_replay_to_directory(
    trace_path: str | Path,
    output_dir: str | Path,
    *,
    clean: bool = True,
) -> dict[str, Any]:
    destination = Path(output_dir)
    trace: dict[str, Any] | None = None
    try:
        if clean:
            clear_output_directory(destination)
        else:
            destination.mkdir(parents=True, exist_ok=True)
            (destination / "level.json").unlink(missing_ok=True)
        trace = read_json(trace_path)
        validate_trace(trace)
        replay = replay_trace(trace)
    except LevelGenError as error:
        destination.mkdir(parents=True, exist_ok=True)
        (destination / "level.json").unlink(missing_ok=True)
        payload = error.to_dict()
        write_json_atomic(destination / "error.json", payload)
        write_json_atomic(destination / "replay_result.json", payload)
        raise
    except (json.JSONDecodeError, TypeError, ValueError) as error:
        wrapped = LevelGenError(
            E_TRACE_INVALID,
            f"轨迹 JSON 无法解析: {error}",
            point=str(trace_path),
        )
        destination.mkdir(parents=True, exist_ok=True)
        (destination / "level.json").unlink(missing_ok=True)
        write_json_atomic(destination / "error.json", wrapped.to_dict())
        write_json_atomic(destination / "replay_result.json", wrapped.to_dict())
        raise wrapped from error
    except OSError as error:
        wrapped = LevelGenError(
            E_IO,
            f"读取轨迹失败: {error}",
            point=str(trace_path),
        )
        destination.mkdir(parents=True, exist_ok=True)
        (destination / "level.json").unlink(missing_ok=True)
        write_json_atomic(destination / "error.json", wrapped.to_dict())
        raise wrapped from error

    payload: dict[str, Any] = {
        "level_hash": None,
        "ok": replay.ok,
        "trace_hash": None if trace is None else content_hash(trace),
    }
    if replay.ok and replay.level is not None:
        write_json_atomic(destination / "level.json", replay.level.to_dict())
        payload["level_hash"] = replay.level.content_hash
    elif replay.conflict is not None:
        write_json_atomic(destination / "conflict.json", replay.conflict)
        payload["conflict"] = replay.conflict
    write_json_atomic(destination / "replay_result.json", payload)
    return payload


def run_injection_to_directory(
    request: dict[str, Any],
    injection_type: str,
    output_dir: str | Path,
) -> dict[str, Any]:
    destination = Path(output_dir)
    clear_output_directory(destination)
    try:
        result: InjectionResult = run_injection(request, injection_type)
    except LevelGenError as error:
        (destination / "level.json").unlink(missing_ok=True)
        write_json_atomic(destination / "error.json", error.to_dict())
        raise
    payload = result.to_dict()
    write_json_atomic(destination / "result.json", payload)
    return payload
