"""Versioned, atomic render snapshots and stale-field detection."""

from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
import math
import os
from pathlib import Path
import tempfile
from typing import Any, Mapping

from .budget import BudgetConfig
from .errors import SnapshotMismatchError, TransferFunctionError
from .renderer import RenderResult
from .transfer import validate_control_points
from .view import hash_view_matrix


SNAPSHOT_VERSION = 1
REQUIRED_FIELDS = (
    "version",
    "view_matrix",
    "view_hash",
    "step_size",
    "control_points",
    "precision_mode",
    "budget",
    "degradation_stages",
    "content_hash",
    "snapshot_digest",
)


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    )


def snapshot_digest(payload: Mapping[str, Any]) -> str:
    body = {key: value for key, value in payload.items() if key != "snapshot_digest"}
    return hashlib.sha256(canonical_json(body).encode("utf-8")).hexdigest()


def build_snapshot(result: RenderResult) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "budget": result.budget.to_dict(),
        "content_hash": result.content_hash,
        "control_points": [list(point) for point in result.control_points],
        "degradation_stages": list(result.degradation_stages),
        "precision_mode": result.precision_mode,
        "step_size": result.step_size,
        "version": SNAPSHOT_VERSION,
        "view_hash": result.view_hash,
        "view_matrix": [list(row) for row in result.view_matrix],
    }
    payload["snapshot_digest"] = snapshot_digest(payload)
    return payload


def save_snapshot(path: str | Path, result: RenderResult) -> dict[str, Any]:
    """Persist a snapshot via fsync + atomic replace in the target directory."""

    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    payload = build_snapshot(result)
    encoded = (json.dumps(payload, sort_keys=True, indent=2, ensure_ascii=False, allow_nan=False) + "\n").encode(
        "utf-8"
    )
    temporary_name: str | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="wb",
            prefix=f".{target.name}.",
            suffix=".tmp",
            dir=target.parent,
            delete=False,
        ) as handle:
            temporary_name = handle.name
            handle.write(encoded)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary_name, 0o644)
        os.replace(temporary_name, target)
        temporary_name = None
        try:
            directory_fd = os.open(target.parent, os.O_RDONLY)
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
        except OSError:
            # Directory fsync is not available on every filesystem; the
            # rename itself is still atomic.
            pass
    finally:
        if temporary_name is not None:
            try:
                os.unlink(temporary_name)
            except FileNotFoundError:
                pass
    return payload


def _fail(detail: str, message: str, *, field: str | None = None, offset: int | None = None) -> None:
    raise SnapshotMismatchError(detail, message, field=field, offset=offset)


def validate_snapshot(
    payload: Any,
    *,
    expected_content_hash: str | None = None,
) -> dict[str, Any]:
    if not isinstance(payload, dict):
        _fail("BAD_SNAPSHOT", "快照根节点必须是 JSON 对象")
    for field in REQUIRED_FIELDS:
        if field not in payload:
            _fail("MISSING_FIELD", f"快照缺少必需字段 {field}", field=field)
    if payload["version"] != SNAPSHOT_VERSION:
        _fail(
            "VERSION_MISMATCH",
            f"快照版本 {payload['version']!r} 与当前版本 {SNAPSHOT_VERSION} 不匹配",
            field="version",
        )
    try:
        digest = snapshot_digest(payload)
    except (TypeError, ValueError) as exc:
        _fail("BAD_SNAPSHOT", f"快照包含不可序列化或非有限字段: {exc}")
    if not isinstance(payload["snapshot_digest"], str) or digest != payload["snapshot_digest"]:
        _fail("HASH_MISMATCH", "快照内容哈希校验失败", field="snapshot_digest")
    matrix = payload["view_matrix"]
    if (
        not isinstance(matrix, list)
        or len(matrix) != 4
        or any(not isinstance(row, list) or len(row) != 4 for row in matrix)
    ):
        _fail("BAD_VIEW_MATRIX", "view_matrix 必须为 4x4 数值矩阵", field="view_matrix")
    if any(
        not isinstance(value, (int, float)) or not math.isfinite(float(value))
        for row in matrix
        for value in row
    ):
        _fail("BAD_VIEW_MATRIX", "view_matrix 含非有限值", field="view_matrix")
    if payload["view_hash"] != hash_view_matrix(matrix):
        _fail("VIEW_HASH_MISMATCH", "view_hash 与 view_matrix 不一致", field="view_hash")
    points = payload["control_points"]
    if (
        not isinstance(points, list)
        or len(points) < 2
        or any(not isinstance(point, list) or len(point) != 5 for point in points)
    ):
        _fail("BAD_CONTROL_POINTS", "control_points 格式非法", field="control_points")
    try:
        validate_control_points(points)
    except (TransferFunctionError, TypeError, ValueError) as exc:
        message = getattr(exc, "message", str(exc))
        _fail("BAD_CONTROL_POINTS", f"control_points 非法: {message}", field="control_points")
    if not isinstance(payload["budget"], dict):
        _fail("BAD_BUDGET", "budget 字段必须为对象", field="budget")
    if not isinstance(payload["degradation_stages"], list) or any(
        stage not in ("increase_step", "lower_early_term", "reduce_batch")
        for stage in payload["degradation_stages"]
    ):
        _fail("BAD_DEGRADATION", "degradation_stages 必须为数组", field="degradation_stages")
    if (
        not isinstance(payload["content_hash"], str)
        or len(payload["content_hash"]) != 64
        or any(character not in "0123456789abcdef" for character in payload["content_hash"])
    ):
        _fail("BAD_CONTENT_HASH", "content_hash 必须为 64 位十六进制摘要", field="content_hash")
    step = payload["step_size"]
    if not isinstance(step, (int, float)) or not math.isfinite(float(step)) or float(step) <= 0.0:
        _fail("BAD_STEP", "step_size 必须为有限正数", field="step_size")
    if payload["precision_mode"] not in ("fp32", "fp64"):
        _fail("BAD_PRECISION", "precision_mode 必须为 fp32 或 fp64", field="precision_mode")
    if expected_content_hash is not None and payload["content_hash"] != expected_content_hash:
        _fail(
            "CONTENT_HASH_MISMATCH",
            "快照 content_hash 与当前渲染结果不一致",
            field="content_hash",
        )
    # Reject values that cannot round-trip through the documented budget
    # schema.  This also catches NaN/Infinity through canonical_json.
    try:
        BudgetConfig.from_dict(payload["budget"])
    except (TypeError, ValueError, OverflowError) as exc:
        _fail("BAD_BUDGET", f"budget 字段非法: {exc}", field="budget")
    return payload


def load_snapshot(
    path: str | Path,
    *,
    expected_content_hash: str | None = None,
) -> dict[str, Any]:
    target = Path(path)
    try:
        raw = target.read_bytes()
    except OSError as exc:
        _fail("READ_FAILED", f"无法读取快照 {target}: {exc}", field="snapshot")
    try:
        payload = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        _fail("BAD_JSON", f"快照 JSON 解析失败: {exc}", field="snapshot")
    return validate_snapshot(payload, expected_content_hash=expected_content_hash)


def stale_fields(
    snapshot: Mapping[str, Any],
    current: Mapping[str, Any],
) -> list[str]:
    """Return content-affecting fields that require a re-render."""

    stale: list[str] = []
    if snapshot.get("view_matrix") != current.get("view_matrix"):
        stale.append("view")
    if snapshot.get("control_points") != current.get("control_points"):
        stale.append("lut")
    if snapshot.get("budget") != current.get("budget"):
        stale.append("budget")
    if snapshot.get("step_size") != current.get("step_size"):
        stale.append("step")
    if snapshot.get("precision_mode") != current.get("precision_mode"):
        stale.append("precision")
    if snapshot.get("degradation_stages") != current.get("degradation_stages"):
        stale.append("degradation")
    return stale


@dataclass
class SnapshotLoad:
    payload: dict[str, Any]
    stale_fields: list[str]


class SnapshotManager:
    """Tracks the last successfully committed snapshot and rendered buffers."""

    def __init__(self) -> None:
        self.last_snapshot: dict[str, Any] | None = None
        self.last_snapshot_path: Path | None = None
        self.current_result: RenderResult | None = None

    def commit(self, path: str | Path, result: RenderResult) -> dict[str, Any]:
        payload = save_snapshot(path, result)
        # Validate the bytes that actually landed on disk before advancing the
        # in-memory commit pointer.
        loaded = load_snapshot(path, expected_content_hash=result.content_hash)
        self.last_snapshot = loaded
        self.last_snapshot_path = Path(path)
        self.current_result = result
        return loaded

    def load(self, path: str | Path) -> SnapshotLoad:
        loaded = load_snapshot(path)
        if self.current_result is None:
            stale: list[str] = []
        else:
            stale = stale_fields(loaded, self.current_result.snapshot_fields())
        # On any validation error load_snapshot raises before this assignment.
        self.last_snapshot = loaded
        self.last_snapshot_path = Path(path)
        return SnapshotLoad(payload=loaded, stale_fields=stale)

    def discard_loaded_state(self) -> None:
        """Roll back a non-committed load without touching rendered buffers."""

        self.last_snapshot = None
        self.last_snapshot_path = None


__all__ = [
    "REQUIRED_FIELDS",
    "SNAPSHOT_VERSION",
    "SnapshotLoad",
    "SnapshotManager",
    "build_snapshot",
    "load_snapshot",
    "save_snapshot",
    "snapshot_digest",
    "stale_fields",
    "validate_snapshot",
]

