from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import E_PROJECT_MISMATCH, PipelineError
from .io import atomic_write_json, parse_path, serialize_v2
from .model import PathData
from .boolean import OPERATIONS


PROJECT_FORMAT = "vpipe-project"
PROJECT_VERSION = 1


@dataclass
class ProjectData:
    path_a: PathData
    path_b: PathData
    operation: str
    tolerance: float
    version_settings: dict[str, Any]
    thresholds: dict[str, Any]
    last_committed: dict[str, Any] | None
    source_path: str | None = None


def _canonical_bytes(payload: Any) -> bytes:
    return json.dumps(
        payload,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def _content_hash(payload: Any) -> str:
    return "sha256:" + hashlib.sha256(_canonical_bytes(payload)).hexdigest()


def build_project_payload(
    path_a: PathData,
    path_b: PathData,
    operation: str,
    tolerance: float,
    version_settings: dict[str, Any],
    thresholds: dict[str, Any],
    last_committed: Any | None,
) -> dict[str, Any]:
    if operation not in OPERATIONS:
        raise ValueError(f"unknown operation: {operation}")
    committed_payload: dict[str, Any] | None = None
    if last_committed is not None:
        if not getattr(last_committed, "committed", False):
            raise PipelineError(
                E_PROJECT_MISMATCH,
                "uncommitted intermediate results are not allowed in a project",
                reason="uncommitted_result",
            )
        committed_payload = {
            "committed": True,
            "result_id": last_committed.result_id,
            "result_hash": last_committed.result_hash,
            "operation": last_committed.operation,
            "tolerance": last_committed.tolerance,
            "result": serialize_v2(last_committed.path),
            "metrics": last_committed.metrics.to_dict(),
            "boolean_report": last_committed.boolean_report,
            "thresholds": last_committed.thresholds.to_dict(),
        }
    payload = {
        "path_a": serialize_v2(path_a),
        "path_b": serialize_v2(path_b),
        "operation": operation,
        "tolerance": tolerance,
        "version_settings": version_settings,
        "thresholds": thresholds,
        "last_committed": committed_payload,
    }
    return {
        "project_format": PROJECT_FORMAT,
        "project_version": PROJECT_VERSION,
        "content_hash": _content_hash(payload),
        "payload": payload,
    }


def save_project(
    destination: str | Path,
    path_a: PathData,
    path_b: PathData,
    operation: str,
    tolerance: float,
    version_settings: dict[str, Any],
    thresholds: dict[str, Any],
    last_committed: Any | None = None,
) -> dict[str, Any]:
    document = build_project_payload(
        path_a,
        path_b,
        operation,
        tolerance,
        version_settings,
        thresholds,
        last_committed,
    )
    atomic_write_json(destination, document)
    return document


def load_project(source: str | Path) -> ProjectData:
    source_path = Path(source)
    path = Path(source)
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise PipelineError(
            E_PROJECT_MISMATCH,
            "unable to read project file",
            source=str(source_path),
            error=str(exc),
        ) from exc
    if not isinstance(document, dict):
        raise PipelineError(
            E_PROJECT_MISMATCH,
            "project root must be an object",
            source=str(source_path),
        )
    if document.get("project_format") != PROJECT_FORMAT:
        raise PipelineError(
            E_PROJECT_MISMATCH,
            "project format mismatch",
            expected=PROJECT_FORMAT,
            actual=document.get("project_format"),
        )
    if document.get("project_version") != PROJECT_VERSION:
        raise PipelineError(
            E_PROJECT_MISMATCH,
            "project version mismatch",
            expected=PROJECT_VERSION,
            actual=document.get("project_version"),
        )
    payload = document.get("payload")
    if not isinstance(payload, dict):
        raise PipelineError(E_PROJECT_MISMATCH, "project payload is missing")
    expected_hash = _content_hash(payload)
    if document.get("content_hash") != expected_hash:
        raise PipelineError(
            E_PROJECT_MISMATCH,
            "project content hash mismatch",
            expected=expected_hash,
            actual=document.get("content_hash"),
        )
    try:
        path_a = parse_path(payload["path_a"], f"{source_path}:path_a")
        path_b = parse_path(payload["path_b"], f"{source_path}:path_b")
        operation = payload["operation"]
        tolerance = float(payload["tolerance"])
        version_settings = dict(payload.get("version_settings", {}))
        thresholds = dict(payload.get("thresholds", {}))
        last_committed = payload.get("last_committed")
        if operation not in OPERATIONS:
            raise ValueError(f"invalid operation {operation!r}")
        if tolerance <= 0.0:
            raise ValueError("invalid tolerance")
        if last_committed is not None:
            if not isinstance(last_committed, dict):
                raise ValueError("last_committed must be an object")
            if not last_committed.get("committed", False):
                raise ValueError("last_committed is not marked committed")
            parse_path(last_committed["result"], f"{source_path}:last_result")
    except PipelineError:
        raise
    except (KeyError, TypeError, ValueError) as exc:
        raise PipelineError(
            E_PROJECT_MISMATCH,
            "project payload structure is invalid",
            error=str(exc),
        ) from exc
    return ProjectData(
        path_a=path_a,
        path_b=path_b,
        operation=operation,
        tolerance=tolerance,
        version_settings=version_settings,
        thresholds=thresholds,
        last_committed=last_committed,
        source_path=str(source_path),
    )

