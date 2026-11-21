"""Content-addressed color-buffer persistence and verification."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import ErrorCode, VolumeRenderError
from .params import RenderParameters
from .renderer import RenderResult


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
    finally:
        if temp_path.exists():
            temp_path.unlink()


@dataclass(frozen=True)
class StoredBuffer:
    result: RenderResult
    manifest: dict[str, Any]
    pixels_path: Path
    manifest_path: Path


class BufferStore:
    def __init__(self, root: str | os.PathLike[str]) -> None:
        self.root = Path(root)

    def publish(
        self,
        result: RenderResult,
        params: RenderParameters | None = None,
        *,
        name: str | None = None,
    ) -> StoredBuffer:
        if params is None:
            params = result.parameters
        if params is None:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "cannot publish a buffer without its parameter group",
                field="parameters",
            )
        result.assert_bound_to(params)
        if params.viewport != (result.width, result.height):
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "buffer viewport differs from its parameter group",
                field="viewport",
            )
        buffer_name = name or params.parameter_hash
        _validate_buffer_name(buffer_name)
        pixels_path = self.root / f"{buffer_name}.rgba"
        manifest_path = self.root / f"{buffer_name}.json"
        manifest = result.to_manifest_dict(params)
        _atomic_write(pixels_path, result.rgba_bytes)
        if result.mask_coverage is not None:
            coverage_path = self.root / f"{buffer_name}.mask"
            _atomic_write(
                coverage_path,
                np.ascontiguousarray(result.mask_coverage, dtype=np.uint8).tobytes(),
            )
        _atomic_write(
            manifest_path,
            json.dumps(manifest, ensure_ascii=True, sort_keys=True, separators=(",", ":")).encode("utf-8"),
        )
        return StoredBuffer(result, manifest, pixels_path, manifest_path)

    def load_verified(
        self,
        name: str,
        *,
        expected_parameter_hash: str | None = None,
    ) -> StoredBuffer:
        _validate_buffer_name(name)
        manifest_path = self.root / f"{name}.json"
        pixels_path = self.root / f"{name}.rgba"
        if not manifest_path.is_file():
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "buffer manifest is missing",
                field="manifest",
            )
        try:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "buffer manifest is unreadable",
                field="manifest",
            ) from exc
        required = (
            "schema",
            "version",
            "parameter_hash",
            "buffer_hash",
            "width",
            "height",
            "channels",
            "parameters",
        )
        for field in required:
            if field not in manifest:
                raise VolumeRenderError(
                    ErrorCode.E_PARAM_STALE,
                    "buffer manifest field is missing",
                    field=field,
                )
        if manifest["schema"] != "volrender-buffer-v1" or manifest["version"] != 1:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "buffer manifest schema/version is unsupported",
                field="version",
            )
        if manifest["channels"] != 4:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "only four-channel color buffers are supported",
                field="channels",
            )
        try:
            params = RenderParameters.from_snapshot_dict(manifest["parameters"])
        except VolumeRenderError as exc:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "buffer manifest parameter group is invalid",
                field="parameters",
                details={"cause": exc.to_dict()},
            ) from exc
        stored_param_hash = str(manifest["parameter_hash"])
        if params.parameter_hash != stored_param_hash:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "manifest parameter hash is not the content hash of its parameters",
                field="parameter_hash",
                details={"expected": params.parameter_hash, "actual": stored_param_hash},
            )
        if expected_parameter_hash is not None and stored_param_hash != expected_parameter_hash:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "buffer is not bound to the requested parameter hash",
                field="parameter_hash",
                details={"expected": expected_parameter_hash, "actual": stored_param_hash},
            )
        if not pixels_path.is_file():
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "color buffer file is missing",
                field="color_buffer",
            )
        try:
            color_bytes = pixels_path.read_bytes()
        except OSError as exc:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "color buffer file is unreadable",
                field="color_buffer",
            ) from exc
        expected_size = int(manifest["width"]) * int(manifest["height"]) * 4
        if len(color_bytes) != expected_size:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "color buffer byte count does not match the manifest viewport",
                field="color_buffer_bytes",
                details={"expected": expected_size, "actual": len(color_bytes)},
            )
        actual_hash = hashlib.sha256(color_bytes).hexdigest()
        if actual_hash != manifest["buffer_hash"]:
            raise VolumeRenderError(
                ErrorCode.E_PARAM_STALE,
                "color buffer content hash mismatch",
                field="buffer_hash",
                details={"expected": manifest["buffer_hash"], "actual": actual_hash},
            )
        height = int(manifest["height"])
        width = int(manifest["width"])
        color = np.frombuffer(color_bytes, dtype=np.uint8).reshape(height, width, 4).copy()
        coverage = None
        coverage_hash = str(manifest.get("mask_coverage_hash", ""))
        if coverage_hash:
            coverage_path = self.root / f"{name}.mask"
            if not coverage_path.is_file():
                raise VolumeRenderError(
                    ErrorCode.E_PARAM_STALE,
                    "mask coverage sidecar is missing",
                    field="mask_coverage",
                )
            coverage_bytes = coverage_path.read_bytes()
            if hashlib.sha256(coverage_bytes).hexdigest() != coverage_hash:
                raise VolumeRenderError(
                    ErrorCode.E_PARAM_STALE,
                    "mask coverage hash mismatch",
                    field="mask_coverage",
                )
            coverage = np.frombuffer(coverage_bytes, dtype=np.uint8).reshape(height, width).astype(bool)
        result = RenderResult(
            color=color,
            parameter_hash=stored_param_hash,
            buffer_hash=actual_hash,
            width=width,
            height=height,
            mask_enabled=bool(manifest.get("mask_enabled", False)),
            mask_coverage=coverage,
            parameters=params,
        )
        return StoredBuffer(result, manifest, pixels_path, manifest_path)


def _validate_buffer_name(name: str) -> None:
    if not name or Path(name).name != name or name in {".", ".."}:
        raise VolumeRenderError(
            ErrorCode.E_PARAM_STALE,
            "buffer name must be a plain file stem",
            field="buffer_name",
        )
