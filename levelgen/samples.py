"""Deterministic sample corpus, baselines and local regression verification."""

from __future__ import annotations

import hashlib
import shutil
from pathlib import Path
from typing import Any

from .canonical import (
    content_hash,
    dump_json_bytes,
    read_json,
    write_json_atomic,
)
from .generator import generate
from .injection import INJECTION_TYPES, run_injection
from .trace import replay_trace, trace_content_hash, validate_trace


SAMPLE_REQUESTS: dict[str, dict[str, Any]] = {
    "s16_easy": {
        "constraints": [
            {
                "hard": True,
                "id": "c_rooms",
                "kind": "room_count",
                "params": {"max": 6, "min": 6},
            },
            {"hard": True, "id": "c_conn", "kind": "connectivity", "params": {}},
            {
                "hard": True,
                "id": "c_chain",
                "kind": "key_chain",
                "params": {"keys": ["K1"]},
            },
            {"hard": True, "id": "c_end", "kind": "end_locked", "params": {}},
            {
                "hard": False,
                "id": "c_area",
                "kind": "area_target",
                "params": {"target": 90, "tolerance": 35},
            },
            {
                "hard": False,
                "id": "c_doors",
                "kind": "door_count_target",
                "params": {"target": 6, "tolerance": 2},
            },
        ],
        "params": {
            "grid_h": 16,
            "grid_w": 16,
            "max_attempts": 32,
            "rewards": 1,
        },
        "seed": 1601,
    },
    "s24_std": {
        "constraints": [
            {
                "hard": True,
                "id": "c_rooms",
                "kind": "room_count",
                "params": {"max": 9, "min": 9},
            },
            {"hard": True, "id": "c_conn", "kind": "connectivity", "params": {}},
            {
                "hard": True,
                "id": "c_chain",
                "kind": "key_chain",
                "params": {"keys": ["K1", "K2"]},
            },
            {"hard": True, "id": "c_end", "kind": "end_locked", "params": {}},
            {
                "hard": False,
                "id": "c_area",
                "kind": "area_target",
                "params": {"target": 120, "tolerance": 60},
            },
            {
                "hard": False,
                "id": "c_doors",
                "kind": "door_count_target",
                "params": {"target": 10, "tolerance": 4},
            },
        ],
        "params": {
            "grid_h": 24,
            "grid_w": 24,
            "max_attempts": 32,
            "rewards": 1,
        },
        "seed": 2401,
    },
    "s32_strict": {
        "constraints": [
            {
                "hard": True,
                "id": "c_rooms",
                "kind": "room_count",
                "params": {"max": 11, "min": 11},
            },
            {"hard": True, "id": "c_conn", "kind": "connectivity", "params": {}},
            {
                "hard": True,
                "id": "c_chain",
                "kind": "key_chain",
                "params": {"keys": ["K1", "K2", "K3"]},
            },
            {"hard": True, "id": "c_end", "kind": "end_locked", "params": {}},
            {
                "hard": True,
                "id": "c_doors",
                "kind": "door_count_target",
                "params": {"target": 13, "tolerance": 2},
            },
            {
                "hard": False,
                "id": "c_area",
                "kind": "area_target",
                "params": {"target": 180, "tolerance": 50},
            },
        ],
        "params": {
            "grid_h": 32,
            "grid_w": 32,
            "max_attempts": 32,
            "rewards": 3,
        },
        "seed": 3201,
    },
}


CONFLICT_REQUESTS: dict[str, dict[str, Any]] = {
    "conflict_room_range": {
        "constraints": [
            {
                "hard": True,
                "id": "c_rooms_a",
                "kind": "room_count",
                "params": {"max": 10, "min": 8},
            },
            {
                "hard": True,
                "id": "c_rooms_b",
                "kind": "room_count",
                "params": {"max": 5, "min": 3},
            },
        ],
        "params": {
            "grid_h": 24,
            "grid_w": 24,
            "max_attempts": 8,
            "rewards": 1,
        },
        "seed": 5,
    },
    "conflict_chain_vs_rooms": {
        "constraints": [
            {
                "hard": True,
                "id": "c_rooms",
                "kind": "room_count",
                "params": {"max": 5, "min": 4},
            },
            {
                "hard": True,
                "id": "c_chain",
                "kind": "key_chain",
                "params": {"keys": ["K1", "K2"]},
            },
            {"hard": True, "id": "c_end", "kind": "end_locked", "params": {}},
        ],
        "params": {
            "grid_h": 24,
            "grid_w": 24,
            "max_attempts": 8,
            "rewards": 1,
        },
        "seed": 6,
    },
    "conflict_end_without_chain": {
        "constraints": [
            {
                "hard": True,
                "id": "c_rooms",
                "kind": "room_count",
                "params": {"max": 6, "min": 4},
            },
            {"hard": True, "id": "c_end", "kind": "end_locked", "params": {}},
        ],
        "params": {
            "grid_h": 24,
            "grid_w": 24,
            "max_attempts": 8,
            "rewards": 0,
        },
        "seed": 8,
    },
}


INJECTION_REQUEST = SAMPLE_REQUESTS["s24_std"]


def _write_request(path: Path, request: dict[str, Any]) -> None:
    write_json_atomic(path, request)


def _sample_baseline(
    result: Any,
    trace_bytes: bytes,
    level_bytes: bytes,
) -> dict[str, Any]:
    assert result.level is not None
    assert result.trace is not None
    return {
        "level_hash": result.level.content_hash,
        "level_bytes_sha256": hashlib.sha256(level_bytes).hexdigest(),
        "trace_hash": trace_content_hash(result.trace),
        "trace_bytes_sha256": hashlib.sha256(trace_bytes).hexdigest(),
    }


def build_artifacts(root: str | Path = "artifacts") -> dict[str, Any]:
    artifacts = Path(root)
    if artifacts.exists():
        shutil.rmtree(artifacts)
    (artifacts / "samples").mkdir(parents=True)
    (artifacts / "traces").mkdir()
    (artifacts / "baselines").mkdir()
    (artifacts / "reports").mkdir()
    (artifacts / "injections").mkdir()
    (artifacts / "requests").mkdir()

    baselines: dict[str, Any] = {}
    checks: list[dict[str, Any]] = []

    for name, request in SAMPLE_REQUESTS.items():
        write_json_atomic(artifacts / "requests" / f"{name}.request.json", request)
        result = generate(request)
        if not result.ok or result.level is None or result.trace is None:
            baselines[name] = {"code": result.code}
            checks.append({"check": f"{name}.generate", "ok": False})
            continue
        level_bytes = dump_json_bytes(result.level.to_dict())
        trace_bytes = dump_json_bytes(result.trace)
        write_json_atomic(artifacts / "samples" / f"{name}.level.json", result.level.to_dict())
        write_json_atomic(artifacts / "traces" / f"{name}.trace.json", result.trace)
        baselines[name] = _sample_baseline(result, trace_bytes, level_bytes)
        replay = replay_trace(result.trace)
        checks.append(
            {
                "check": f"{name}.deterministic_replay",
                "ok": (
                    replay.ok
                    and replay.level is not None
                    and replay.level.content_hash == result.level.content_hash
                ),
            }
        )
        repeated = generate(request)
        checks.append(
            {
                "check": f"{name}.byte_stable",
                "ok": (
                    repeated.ok
                    and repeated.trace is not None
                    and dump_json_bytes(repeated.trace) == trace_bytes
                    and repeated.level is not None
                    and dump_json_bytes(repeated.level.to_dict()) == level_bytes
                ),
            }
        )

    for name, request in CONFLICT_REQUESTS.items():
        write_json_atomic(
            artifacts / "requests" / f"{name}.request.json",
            request,
        )
        result = generate(request)
        report = result.conflict or {}
        payload = {
            "code": result.code,
            "report": report,
        }
        if result.trace is not None:
            write_json_atomic(
                artifacts / "reports" / f"{name}.trace.json",
                result.trace,
            )
            report_directory = artifacts / "reports" / name
            report_directory.mkdir(parents=True, exist_ok=True)
            write_json_atomic(report_directory / "trace.json", result.trace)
            write_json_atomic(report_directory / "report.json", payload)
            replay = replay_trace(result.trace)
            reconstructed = replay.conflict
        else:
            reconstructed = None
        write_json_atomic(artifacts / "reports" / f"{name}.report.json", payload)
        baselines[name] = {
            "code": result.code,
            "constraints": list(report.get("constraint_ids", [])),
            "report_hash": content_hash(report),
        }
        checks.append(
            {
                "check": f"{name}.trace_reconstructs_report",
                "ok": (
                    result.code == "E_CONSTRAINT_CONFLICT"
                    and reconstructed == report
                    and bool(report.get("constraint_ids"))
                    and bool(report.get("stage"))
                ),
            }
        )

    for injection_type in INJECTION_TYPES:
        result = run_injection(INJECTION_REQUEST, injection_type)
        output_dir = artifacts / "injections" / injection_type
        output_dir.mkdir(parents=True)
        payload = result.to_dict()
        write_json_atomic(output_dir / "result.json", payload)
        write_json_atomic(
            artifacts / "injections" / f"{injection_type}.result.json",
            payload,
        )
        baselines[f"inject_{injection_type}"] = {
            "code": result.code,
            "point": result.point,
            "result_hash": content_hash(payload),
        }
        checks.append(
            {
                "check": f"inject_{injection_type}.blocked_before_level",
                "ok": (
                    not (output_dir / "level.json").exists()
                    and payload.get("level_written") is False
                    and bool(result.code)
                    and bool(result.point)
                ),
            }
        )

    headless_request = artifacts / "headless_request.json"
    write_json_atomic(
        headless_request,
        SAMPLE_REQUESTS["s24_std"],
    )
    headless_result = generate(SAMPLE_REQUESTS["s24_std"])
    headless_dir = artifacts / "headless_out"
    headless_dir.mkdir(parents=True)
    if headless_result.level is not None and headless_result.trace is not None:
        write_json_atomic(
            headless_dir / "level.json",
            headless_result.level.to_dict(),
        )
        write_json_atomic(headless_dir / "trace.json", headless_result.trace)
        write_json_atomic(
            headless_dir / "result.json",
            {
                "attempts": headless_result.attempts,
                "level_hash": headless_result.level.content_hash,
                "ok": headless_result.ok,
            },
        )

    write_json_atomic(artifacts / "baselines" / "hashes.json", baselines)
    summary = {
        "checks": checks,
        "format": "levelgen-verification/1",
        "ok": all(check["ok"] for check in checks),
        "sample_hashes": {
            name: baselines[name]
            for name in SAMPLE_REQUESTS
            if name in baselines
        },
    }
    write_json_atomic(artifacts / "verification_report.json", summary)
    return summary


def verify_artifacts(root: str | Path = "artifacts") -> dict[str, Any]:
    artifacts = Path(root)
    baselines = read_json(artifacts / "baselines" / "hashes.json")
    checks: list[dict[str, Any]] = []

    for name, request in SAMPLE_REQUESTS.items():
        baseline = baselines.get(name, {})
        result = generate(request)
        level_path = artifacts / "samples" / f"{name}.level.json"
        trace_path = artifacts / "traces" / f"{name}.trace.json"
        trace_bytes = trace_path.read_bytes() if trace_path.exists() else b""
        level_bytes = level_path.read_bytes() if level_path.exists() else b""
        checks.append(
            {
                "check": f"{name}.level_hash",
                "ok": (
                    result.ok
                    and result.level is not None
                    and result.level.content_hash == baseline.get("level_hash")
                ),
            }
        )
        checks.append(
            {
                "check": f"{name}.trace_bytes",
                "ok": (
                    result.trace is not None
                    and hashlib.sha256(trace_bytes).hexdigest()
                    == baseline.get("trace_bytes_sha256")
                ),
            }
        )
        if trace_bytes:
            trace = read_json(trace_path)
            validate_trace(trace)
            replay = replay_trace(trace)
            checks.append(
                {
                    "check": f"{name}.replay",
                    "ok": (
                        replay.ok
                        and replay.level is not None
                        and replay.level.content_hash == baseline.get("level_hash")
                    ),
                }
            )

    for name in CONFLICT_REQUESTS:
        trace_path = artifacts / "reports" / f"{name}.trace.json"
        report_path = artifacts / "reports" / f"{name}.report.json"
        trace = read_json(trace_path)
        replay = replay_trace(trace)
        stored = read_json(report_path)
        checks.append(
            {
                "check": f"{name}.conflict_replay",
                "ok": (
                    replay.conflict == stored.get("report")
                    and bool(stored.get("report", {}).get("constraint_ids"))
                ),
            }
        )

    for injection_type in INJECTION_TYPES:
        result_path = (
            artifacts
            / "injections"
            / f"{injection_type}.result.json"
        )
        stored = read_json(result_path)
        directory = artifacts / "injections" / injection_type
        rerun = run_injection(INJECTION_REQUEST, injection_type)
        checks.append(
            {
                "check": f"inject_{injection_type}.business_code",
                "ok": (
                    stored.get("code") == rerun.code
                    and stored.get("point") == rerun.point
                    and not (directory / "level.json").exists()
                ),
            }
        )

    summary = {
        "checks": checks,
        "format": "levelgen-verification/1",
        "ok": all(check["ok"] for check in checks),
    }
    return summary
