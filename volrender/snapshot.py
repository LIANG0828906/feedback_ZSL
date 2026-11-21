"""Atomic parameter-group snapshots and verified snapshot restoration."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

from .buffers import BufferStore
from .errors import ErrorCode, VolumeRenderError
from .params import RenderParameters
from .renderer import CancellationToken, RenderResult, render_offscreen
from .volume import VolumeData

SNAPSHOT_SCHEMA = "volrender-snapshot"
SNAPSHOT_VERSION = 1


def _canonical_json(payload: Any) -> bytes:
    return json.dumps(
        payload,
        ensure_ascii=True,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def _atomic_write(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp_path, path)
        try:
            directory_descriptor = os.open(path.parent, os.O_RDONLY)
            try:
                os.fsync(directory_descriptor)
            finally:
                os.close(directory_descriptor)
        except OSError:
            pass
    finally:
        if temp_path.exists():
            temp_path.unlink()


@dataclass(frozen=True)
class Snapshot:
    params: RenderParameters
    color_buffer_hash: str
    color_buffer_name: str
    mask_coverage_hash: str = ""
    snapshot_id: str = ""

    @property
    def version(self) -> int:
        return SNAPSHOT_VERSION

    @property
    def parameter_hash(self) -> str:
        return self.params.parameter_hash

    @property
    def view_matrix(self) -> tuple[float, ...]:
        return self.params.view_matrix

    @property
    def viewport(self) -> tuple[int, int]:
        return self.params.viewport

    @property
    def step_size(self) -> float:
        return self.params.step_size

    @property
    def volume_hash(self) -> str:
        return self.params.volume_hash

    @property
    def mask_enabled(self) -> bool:
        return self.params.mask_enabled

    @property
    def mask_hash(self) -> str:
        return self.params.mask_hash

    def body_dict(self) -> dict[str, Any]:
        return {
            "schema": SNAPSHOT_SCHEMA,
            "version": SNAPSHOT_VERSION,
            "parameter_hash": self.params.parameter_hash,
            "params": self.params.to_snapshot_dict(),
            "view_matrix": [list(self.params.view_matrix[row * 4 : row * 4 + 4]) for row in range(4)],
            "viewport": [self.params.viewport[0], self.params.viewport[1]],
            "step_size": self.params.step_size,
            "mask_enabled": self.params.mask_enabled,
            "mask_hash": self.params.mask_hash,
            "volume_hash": self.params.volume_hash,
            "color_buffer_hash": self.color_buffer_hash,
            "color_buffer_name": self.color_buffer_name,
            "mask_coverage_hash": self.mask_coverage_hash,
        }

    def computed_snapshot_id(self) -> str:
        return hashlib.sha256(_canonical_json(self.body_dict())).hexdigest()

    def to_dict(self) -> dict[str, Any]:
        payload = self.body_dict()
        payload["snapshot_id"] = self.snapshot_id or self.computed_snapshot_id()
        return payload

    def validate(self) -> None:
        if not _is_sha256(self.color_buffer_hash):
            raise VolumeRenderError(
                ErrorCode.E_SNAPSHOT_INVALID,
                "color buffer hash is missing or malformed",
                field="color_buffer_hash",
            )
        if self.mask_coverage_hash and not _is_sha256(self.mask_coverage_hash):
            raise VolumeRenderError(
                ErrorCode.E_SNAPSHOT_INVALID,
                "mask coverage hash is malformed",
                field="mask_coverage_hash",
            )
        if not self.color_buffer_name:
            raise VolumeRenderError(
                ErrorCode.E_SNAPSHOT_INVALID,
                "color buffer name is missing",
                field="color_buffer_name",
            )
        expected = self.computed_snapshot_id()
        if self.snapshot_id and self.snapshot_id != expected:
            raise VolumeRenderError(
                ErrorCode.E_SNAPSHOT_INVALID,
                "snapshot content hash does not match its fields",
                field="snapshot_id",
                details={"expected": expected, "actual": self.snapshot_id},
            )


def _is_sha256(value: object) -> bool:
    return isinstance(value, str) and len(value) == 64 and all(character in "0123456789abcdef" for character in value)


def create_snapshot(
    params: RenderParameters,
    result: RenderResult,
    *,
    color_buffer_name: str | None = None,
    mask_coverage_hash: str = "",
) -> Snapshot:
    result.assert_bound_to(params)
    if params.viewport != (result.width, result.height):
        raise VolumeRenderError(
            ErrorCode.E_SNAPSHOT_INVALID,
            "snapshot viewport does not match the color buffer",
            field="viewport",
        )
    if result.mask_coverage is not None and not mask_coverage_hash:
        import numpy as np

        mask_coverage_hash = hashlib.sha256(
            np.ascontiguousarray(result.mask_coverage, dtype=np.uint8).tobytes()
        ).hexdigest()
    snapshot = Snapshot(
        params=params,
        color_buffer_hash=result.buffer_hash,
        color_buffer_name=color_buffer_name or params.parameter_hash,
        mask_coverage_hash=mask_coverage_hash,
    )
    snapshot = Snapshot(
        params=snapshot.params,
        color_buffer_hash=snapshot.color_buffer_hash,
        color_buffer_name=snapshot.color_buffer_name,
        mask_coverage_hash=snapshot.mask_coverage_hash,
        snapshot_id=snapshot.computed_snapshot_id(),
    )
    snapshot.validate()
    return snapshot


def save_snapshot(snapshot: Snapshot, path: str | os.PathLike[str]) -> Path:
    snapshot.validate()
    target = Path(path)
    _atomic_write(target, _canonical_json(snapshot.to_dict()) + b"\n")
    return target


def load_snapshot(path: str | os.PathLike[str]) -> Snapshot:
    source = Path(path)
    if not source.is_file():
        raise VolumeRenderError(
            ErrorCode.E_SNAPSHOT_INVALID,
            "snapshot file is missing",
            field="path",
        )
    try:
        payload = json.loads(source.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise VolumeRenderError(
            ErrorCode.E_SNAPSHOT_INVALID,
            "snapshot is unreadable or is not valid JSON",
            field="snapshot",
        ) from exc
    if not isinstance(payload, dict):
        raise VolumeRenderError(
            ErrorCode.E_SNAPSHOT_INVALID,
            "snapshot root must be an object",
            field="snapshot",
        )
    required = (
        "schema",
        "version",
        "snapshot_id",
        "parameter_hash",
        "params",
        "view_matrix",
        "viewport",
        "step_size",
        "mask_enabled",
        "mask_hash",
        "volume_hash",
        "color_buffer_hash",
        "color_buffer_name",
    )
    for field in required:
        if field not in payload:
            raise VolumeRenderError(
                ErrorCode.E_SNAPSHOT_INVALID,
                "snapshot field is missing",
                field=field,
            )
    if payload["schema"] != SNAPSHOT_SCHEMA:
        raise VolumeRenderError(
            ErrorCode.E_SNAPSHOT_INVALID,
            "snapshot schema is unsupported",
            field="schema",
            details={"expected": SNAPSHOT_SCHEMA, "actual": payload["schema"]},
        )
    if payload["version"] != SNAPSHOT_VERSION:
        raise VolumeRenderError(
            ErrorCode.E_SNAPSHOT_INVALID,
            "snapshot version is unsupported",
            field="version",
            details={"expected": SNAPSHOT_VERSION, "actual": payload["version"]},
        )
    try:
        params = RenderParameters.from_snapshot_dict(payload["params"])
    except VolumeRenderError as exc:
        raise VolumeRenderError(
            ErrorCode.E_SNAPSHOT_INVALID,
            "snapshot parameter group is invalid",
            field="params",
            details={"cause": exc.to_dict()},
        ) from exc
    if payload["parameter_hash"] != params.parameter_hash:
        raise VolumeRenderError(
            ErrorCode.E_SNAPSHOT_INVALID,
            "snapshot parameter hash does not match its parameter group",
            field="parameter_hash",
            details={"expected": params.parameter_hash, "actual": payload["parameter_hash"]},
        )
    _validate_redundant_field(payload, "volume_hash", params.volume_hash)
    _validate_redundant_field(payload, "step_size", params.step_size)
    _validate_redundant_field(payload, "viewport", list(params.viewport))
    _validate_redundant_field(payload, "mask_enabled", params.mask_enabled)
    _validate_redundant_field(payload, "mask_hash", params.mask_hash)
    expected_view = [list(params.view_matrix[row * 4 : row * 4 + 4]) for row in range(4)]
    _validate_redundant_field(payload, "view_matrix", expected_view)
    snapshot = Snapshot(
        params=params,
        color_buffer_hash=str(payload["color_buffer_hash"]),
        color_buffer_name=str(payload["color_buffer_name"]),
        mask_coverage_hash=str(payload.get("mask_coverage_hash", "")),
        snapshot_id=str(payload["snapshot_id"]),
    )
    _validate_buffer_name(snapshot.color_buffer_name)
    snapshot.validate()
    return snapshot


def _validate_redundant_field(payload: dict[str, Any], field: str, expected: Any) -> None:
    actual = payload[field]
    if isinstance(expected, bool):
        matches = isinstance(actual, bool) and actual == expected
    elif isinstance(expected, float):
        try:
            matches = float(actual) == expected
        except (TypeError, ValueError):
            matches = False
    elif isinstance(expected, list):
        matches = actual == expected
    else:
        matches = actual == expected
    if not matches:
        raise VolumeRenderError(
            ErrorCode.E_SNAPSHOT_INVALID,
            f"snapshot field {field} is inconsistent with its parameter group",
            field=field,
        )


def _validate_buffer_name(name: str) -> None:
    if not name or Path(name).name != name or name in {".", ".."}:
        raise VolumeRenderError(
            ErrorCode.E_SNAPSHOT_INVALID,
            "color buffer name must be a plain file stem",
            field="color_buffer_name",
        )


@dataclass(frozen=True)
class RestoreOutcome:
    result: RenderResult
    snapshot: Snapshot
    reused_buffer: bool
    rerendered: bool
    changed_fields: tuple[str, ...]


class SnapshotManager:
    def __init__(self, buffer_store: BufferStore) -> None:
        self.buffer_store = buffer_store

    def restore(
        self,
        snapshot: Snapshot,
        requested_params: RenderParameters,
        volume: VolumeData,
        target_snapshot_path: str | os.PathLike[str],
        *,
        renderer: Callable[..., RenderResult] = render_offscreen,
        cancel_token: CancellationToken | None = None,
        batch_size: int = 8,
        save_after_rerender: bool = True,
    ) -> RestoreOutcome:
        snapshot.validate()
        _validate_buffer_name(snapshot.color_buffer_name)
        if requested_params.volume_hash != volume.content_sha256:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "requested parameters belong to a different volume",
                field="volume_hash",
            )
        changed = tuple(requested_params.describe_differences(snapshot.params))
        can_try_reuse = not changed and snapshot.parameter_hash == requested_params.parameter_hash
        if can_try_reuse:
            try:
                stored = self.buffer_store.load_verified(
                    snapshot.color_buffer_name,
                    expected_parameter_hash=snapshot.parameter_hash,
                )
            except VolumeRenderError as exc:
                if exc.code != ErrorCode.E_PARAM_STALE:
                    raise
                changed = tuple(dict.fromkeys((*changed, "color_buffer_hash")))
            else:
                if stored.result.buffer_hash != snapshot.color_buffer_hash:
                    changed = tuple(dict.fromkeys((*changed, "color_buffer_hash")))
                elif (
                    snapshot.mask_coverage_hash
                    and stored.manifest.get("mask_coverage_hash", "") != snapshot.mask_coverage_hash
                ):
                    changed = tuple(dict.fromkeys((*changed, "mask_coverage_hash")))
                else:
                    return RestoreOutcome(
                        result=stored.result,
                        snapshot=snapshot,
                        reused_buffer=True,
                        rerendered=False,
                        changed_fields=(),
                    )

        result = renderer(volume, requested_params, cancel_token=cancel_token, batch_size=batch_size)
        buffer_name = requested_params.parameter_hash
        stored = self.buffer_store.publish(result, requested_params, name=buffer_name)
        coverage_hash = str(stored.manifest.get("mask_coverage_hash", ""))
        new_snapshot = create_snapshot(
            requested_params,
            result,
            color_buffer_name=buffer_name,
            mask_coverage_hash=coverage_hash,
        )
        if save_after_rerender:
            save_snapshot(new_snapshot, target_snapshot_path)
        return RestoreOutcome(
            result=result,
            snapshot=new_snapshot,
            reused_buffer=False,
            rerendered=True,
            changed_fields=changed or ("color_buffer_hash",),
        )


def recover_from_snapshot(
    snapshot_path: str | os.PathLike[str],
    volume: VolumeData,
    buffer_store: BufferStore,
    *,
    batch_size: int = 8,
) -> RestoreOutcome:
    """Crash recovery entry point; the disk snapshot is the sole source of truth."""

    snapshot = load_snapshot(snapshot_path)
    if snapshot.params.volume_hash != volume.content_sha256:
        raise VolumeRenderError(
            ErrorCode.E_SNAPSHOT_INVALID,
            "snapshot belongs to a different volume",
            field="volume_hash",
            details={"expected": volume.content_sha256, "actual": snapshot.params.volume_hash},
        )
    manager = SnapshotManager(buffer_store)
    return manager.restore(snapshot, snapshot.params, volume, snapshot_path)
