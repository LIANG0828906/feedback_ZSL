from __future__ import annotations

import hashlib
import json
import math
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import BusinessError, ErrorCode
from .evaluate import ErrorReport
from .model import BooleanResult, Contour, Operation, PathSpec, Point

PROJECT_FORMAT = "vectorbool-project"
PROJECT_VERSION = 1


@dataclass
class LoadedProject:
    path_a: dict[str, Any]
    path_b: dict[str, Any]
    operation: str
    tolerance: float
    report: dict[str, Any] | None
    result: dict[str, Any] | None
    result_object: BooleanResult | None
    committed: bool
    content_hash: str


def canonical_json(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def content_hash(payload: dict[str, Any]) -> str:
    return hashlib.sha256(canonical_json(payload)).hexdigest()


def save_project(
    filename: str | Path,
    *,
    path_a: PathSpec,
    path_b: PathSpec,
    operation: Operation | str,
    tolerance: float,
    result: BooleanResult | None = None,
) -> str:
    operation_value = Operation(operation).value
    committed = bool(
        result is not None
        and result.report is not None
        and getattr(result.report, "accepted", False)
        and result.operation.value == operation_value
        and math.isclose(result.tolerance, float(tolerance), rel_tol=0.0, abs_tol=0.0)
    )
    payload: dict[str, Any] = {
        "path_a": path_a.to_dict(),
        "path_b": path_b.to_dict(),
        "operation": operation_value,
        "tolerance": float(tolerance),
        "report": (
            result.report.to_dict()
            if result is not None and result.report is not None
            else None
        ),
        "committed": committed,
        "result": result.to_dict() if committed and result is not None else None,
    }
    envelope = {
        "format": PROJECT_FORMAT,
        "version": PROJECT_VERSION,
        "content_hash": content_hash(payload),
        "payload": payload,
    }
    destination = Path(filename)
    destination.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=destination.name + ".",
        suffix=".tmp",
        dir=str(destination.parent),
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(
                json.dumps(
                    envelope,
                    ensure_ascii=False,
                    sort_keys=True,
                    indent=2,
                    allow_nan=False,
                ).encode("utf-8")
            )
            stream.write(b"\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, destination)
        directory_descriptor = os.open(str(destination.parent), os.O_RDONLY)
        try:
            os.fsync(directory_descriptor)
        finally:
            os.close(directory_descriptor)
    except Exception:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise
    return envelope["content_hash"]


def load_project(filename: str | Path) -> LoadedProject:
    try:
        with Path(filename).open("r", encoding="utf-8") as stream:
            envelope = json.load(stream)
    except (OSError, json.JSONDecodeError) as error:
        raise BusinessError(
            ErrorCode.PROJECT_MISMATCH,
            f"cannot load project: {error}",
        ) from error
    if not isinstance(envelope, dict):
        raise BusinessError(ErrorCode.PROJECT_MISMATCH, "project root must be an object")
    if envelope.get("format") != PROJECT_FORMAT:
        raise BusinessError(ErrorCode.PROJECT_MISMATCH, "unknown project format")
    if envelope.get("version") != PROJECT_VERSION:
        raise BusinessError(
            ErrorCode.PROJECT_MISMATCH,
            f"unsupported project version: {envelope.get('version')!r}",
        )
    payload = envelope.get("payload")
    stored_hash = envelope.get("content_hash")
    if not isinstance(payload, dict) or not isinstance(stored_hash, str):
        raise BusinessError(ErrorCode.PROJECT_MISMATCH, "project payload is incomplete")
    actual_hash = content_hash(payload)
    if actual_hash != stored_hash:
        raise BusinessError(
            ErrorCode.PROJECT_MISMATCH,
            "project content hash does not match payload",
            details={"expected": stored_hash, "actual": actual_hash},
        )
    committed = payload.get("committed") is True
    result = payload.get("result") if committed else None
    if committed and not isinstance(result, dict):
        raise BusinessError(
            ErrorCode.PROJECT_MISMATCH,
            "committed project is missing its result",
        )
    if committed and (
        result.get("operation") != payload.get("operation")
        or not math.isclose(
            float(result.get("tolerance")),
            float(payload.get("tolerance")),
            rel_tol=0.0,
            abs_tol=0.0,
        )
    ):
        raise BusinessError(
            ErrorCode.PROJECT_MISMATCH,
            "committed result metadata does not match project input",
        )
    try:
        report_object = None
        if isinstance(payload.get("report"), dict):
            report_fields = set(ErrorReport.__dataclass_fields__)
            report_object = ErrorReport(
                **{
                    key: value
                    for key, value in payload["report"].items()
                    if key in report_fields
                }
            )
        result_object = (
            _restore_result(result, report_object) if committed and result else None
        )
        return LoadedProject(
            path_a=payload["path_a"],
            path_b=payload["path_b"],
            operation=Operation(payload["operation"]).value,
            tolerance=float(payload["tolerance"]),
            report=payload.get("report"),
            result=result,
            result_object=result_object,
            committed=committed,
            content_hash=actual_hash,
        )
    except (KeyError, TypeError, ValueError) as error:
        raise BusinessError(
            ErrorCode.PROJECT_MISMATCH,
            f"project payload has invalid fields: {error}",
        ) from error


def _restore_result(
    payload: dict[str, Any], report: ErrorReport | None
) -> BooleanResult:
    contours: list[Contour] = []
    for raw_contour in payload.get("contours", []):
        if raw_contour.get("segments"):
            points = [
                Point(float(segment["points"][0][0]), float(segment["points"][0][1]))
                for segment in raw_contour["segments"]
            ]
        else:
            points = []
        contours.append(
            Contour(
                points=tuple(points),
                role=str(raw_contour.get("role", "outer")),
            )
        )
    return BooleanResult(
        operation=Operation(payload["operation"]),
        tolerance=float(payload["tolerance"]),
        contours=contours,
        intersection_count=int(payload.get("intersection_count", 0)),
        report=report,
        preview_allowed=bool(payload.get("preview_allowed", False)),
        self_intersections=list(payload.get("self_intersections", [])),
    )

