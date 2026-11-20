"""Versioned snapshots with content validation and transactional restore."""

from __future__ import annotations

import base64
import binascii
import copy
import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any, TYPE_CHECKING

import numpy as np

from .errors import E_SNAPSHOT_INVALID, E_SNAPSHOT_STALE, VolRenderError
from .render import (
    ErrorSummary,
    RenderResult,
    canonical_json_bytes,
    compute_state_hash,
)
from .transfer import PRECISION_MODES, TransferFunction

if TYPE_CHECKING:
    from .render import VolumeRenderer


SNAPSHOT_SCHEMA = "volrender.snapshot"
SNAPSHOT_VERSION = 1

REQUIRED_STATE_FIELDS = {
    "volume_id",
    "mapping_fingerprint",
    "view_matrix",
    "step",
    "fov_degrees",
    "image_size",
    "transfer_points",
    "precision_mode",
    "window_width",
    "window_level",
    "spacing",
    "error_threshold",
    "lut_resolution",
    "fixed_bits",
    "fixed_color_bits",
    "lighting_tolerance",
}
REQUIRED_RESULT_FIELDS = {
    "width",
    "height",
    "rgba8",
    "error_map",
    "untrusted",
    "content_hash",
    "error_summary",
    "mode",
}


def _b64encode(data: bytes) -> str:
    return base64.b64encode(data).decode("ascii")


def _b64decode(value: str, field: str) -> bytes:
    try:
        return base64.b64decode(value.encode("ascii"), validate=True)
    except (ValueError, UnicodeEncodeError, binascii.Error) as exc:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            f"snapshot field {field} is not valid base64",
        ) from exc


def result_content_hash(
    rgba8: np.ndarray,
    error_map: np.ndarray,
    untrusted: np.ndarray,
) -> str:
    digest = hashlib.sha256()
    digest.update(np.ascontiguousarray(rgba8).tobytes())
    digest.update(np.ascontiguousarray(error_map, dtype="<f4").tobytes())
    digest.update(np.packbits(untrusted, axis=None).tobytes())
    return digest.hexdigest()


@dataclass(frozen=True)
class Snapshot:
    state: dict[str, Any]
    state_hash: str
    rgba8: np.ndarray
    error_map: np.ndarray
    untrusted: np.ndarray
    error_summary: ErrorSummary
    content_hash: str
    mode: str
    snapshot_hash: str
    raw: dict[str, Any]

    @classmethod
    def from_result(cls, result: RenderResult) -> "Snapshot":
        state = copy.deepcopy(result.state)
        state_hash = compute_state_hash(state)
        if state_hash != result.state_hash:
            raise VolRenderError(
                E_SNAPSHOT_STALE,
                "render result state hash is inconsistent with its state",
                details={
                    "result_state_hash": result.state_hash,
                    "computed_state_hash": state_hash,
                },
            )
        payload = {
            "schema": SNAPSHOT_SCHEMA,
            "schema_version": SNAPSHOT_VERSION,
            "state": state,
            "state_hash": state_hash,
            "result": {
                "width": int(result.rgba8.shape[1]),
                "height": int(result.rgba8.shape[0]),
                "rgba8": _b64encode(np.ascontiguousarray(result.rgba8).tobytes()),
                "error_map": _b64encode(
                    np.ascontiguousarray(result.error_map, dtype="<f4").tobytes()
                ),
                "untrusted": _b64encode(
                    np.packbits(result.untrusted, axis=None).tobytes()
                ),
                "content_hash": result.content_hash,
                "error_summary": result.error_summary.as_dict(),
                "mode": result.mode,
            },
        }
        snapshot_hash = hashlib.sha256(canonical_json_bytes(payload)).hexdigest()
        payload["snapshot_hash"] = snapshot_hash
        return cls(
            state=state,
            state_hash=state_hash,
            rgba8=result.rgba8.copy(),
            error_map=result.error_map.copy(),
            untrusted=result.untrusted.copy(),
            error_summary=result.error_summary,
            content_hash=result.content_hash,
            mode=result.mode,
            snapshot_hash=snapshot_hash,
            raw=payload,
        )

    def as_dict(self) -> dict[str, Any]:
        return copy.deepcopy(self.raw)


def _validate_state_structure(state: Any) -> dict[str, Any]:
    if not isinstance(state, dict):
        raise VolRenderError(E_SNAPSHOT_INVALID, "snapshot state must be an object")
    missing = REQUIRED_STATE_FIELDS.difference(state)
    if missing:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot state is missing required fields",
            details={"missing": sorted(missing)},
        )
    if state["precision_mode"] not in PRECISION_MODES:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot precision mode is invalid",
            details={"mode": state["precision_mode"]},
        )
    try:
        view = np.asarray(state["view_matrix"], dtype=np.float64)
        if view.shape != (4, 4) or not np.all(np.isfinite(view)):
            raise ValueError("view matrix")
        TransferFunction(state["transfer_points"])
        step = float(state["step"])
        threshold = float(state["error_threshold"])
        fov = float(state["fov_degrees"])
        tolerance = float(state["lighting_tolerance"])
        width = float(state["window_width"])
        level = float(state["window_level"])
        image_size = tuple(int(v) for v in state["image_size"])
        spacing = tuple(float(v) for v in state["spacing"])
        lut_resolution = int(state["lut_resolution"])
        fixed_bits = int(state["fixed_bits"])
        fixed_color_bits = int(state["fixed_color_bits"])
        if len(image_size) != 2 or any(v <= 0 for v in image_size):
            raise ValueError("image size")
        if len(spacing) != 3 or any(not np.isfinite(v) or v <= 0.0 for v in spacing):
            raise ValueError("spacing")
        if not np.isfinite(step) or step <= 0.0:
            raise ValueError("step")
        if not np.isfinite(threshold) or threshold <= 0.0:
            raise ValueError("threshold")
        if not np.isfinite(fov) or not 1.0 < fov < 179.0:
            raise ValueError("fov")
        if not np.isfinite(tolerance) or tolerance < 0.0:
            raise ValueError("tolerance")
        if not np.isfinite(width) or width <= 0.0 or not np.isfinite(level):
            raise ValueError("window")
        if lut_resolution < 16 or not 1 <= fixed_bits <= 24 or not 1 <= fixed_color_bits <= 24:
            raise ValueError("lut settings")
        if not isinstance(state["mapping_fingerprint"], str) or not isinstance(
            state["volume_id"], str
        ):
            raise ValueError("identifiers")
    except Exception as exc:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot renderer state is invalid",
        ) from exc
    return state


def _decode_result(result: Any) -> tuple[
    np.ndarray,
    np.ndarray,
    np.ndarray,
    ErrorSummary,
    str,
    str,
]:
    if not isinstance(result, dict):
        raise VolRenderError(E_SNAPSHOT_INVALID, "snapshot result must be an object")
    missing = REQUIRED_RESULT_FIELDS.difference(result)
    if missing:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot result is missing required fields",
            details={"missing": sorted(missing)},
        )
    try:
        width = int(result["width"])
        height = int(result["height"])
    except (TypeError, ValueError) as exc:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot image dimensions are invalid",
        ) from exc
    if width <= 0 or height <= 0:
        raise VolRenderError(E_SNAPSHOT_INVALID, "snapshot image dimensions are invalid")

    rgba_bytes = _b64decode(result["rgba8"], "rgba8")
    expected_rgba = width * height * 4
    if len(rgba_bytes) != expected_rgba:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot RGBA buffer has the wrong size",
            details={"expected": expected_rgba, "actual": len(rgba_bytes)},
        )
    rgba8 = np.frombuffer(rgba_bytes, dtype=np.uint8).reshape((height, width, 4)).copy()

    error_bytes = _b64decode(result["error_map"], "error_map")
    expected_error = width * height * 4
    if len(error_bytes) != expected_error:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot error map has the wrong size",
            details={"expected": expected_error, "actual": len(error_bytes)},
        )
    error_map = (
        np.frombuffer(error_bytes, dtype="<f4").reshape((height, width)).astype(np.float64)
    )
    if not np.all(np.isfinite(error_map)):
        raise VolRenderError(E_SNAPSHOT_INVALID, "snapshot error map is non-finite")

    mask_bytes = _b64decode(result["untrusted"], "untrusted")
    expected_mask = (width * height + 7) // 8
    if len(mask_bytes) != expected_mask:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot untrusted mask has the wrong size",
            details={"expected": expected_mask, "actual": len(mask_bytes)},
        )
    untrusted = (
        np.unpackbits(np.frombuffer(mask_bytes, dtype=np.uint8))[: width * height]
        .reshape((height, width))
        .astype(bool)
    )

    summary_dict = result["error_summary"]
    if not isinstance(summary_dict, dict):
        raise VolRenderError(E_SNAPSHOT_INVALID, "snapshot error summary is invalid")
    required_summary = {
        "threshold",
        "mean_error",
        "max_error",
        "p95_error",
        "untrusted_pixels",
        "total_pixels",
        "trusted_fraction",
        "converged",
        "status",
    }
    if required_summary.difference(summary_dict):
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot error summary is incomplete",
        )
    try:
        summary = ErrorSummary(
            threshold=float(summary_dict["threshold"]),
            mean_error=float(summary_dict["mean_error"]),
            max_error=float(summary_dict["max_error"]),
            p95_error=float(summary_dict["p95_error"]),
            untrusted_pixels=int(summary_dict["untrusted_pixels"]),
            total_pixels=int(summary_dict["total_pixels"]),
            trusted_fraction=float(summary_dict["trusted_fraction"]),
            converged=bool(summary_dict["converged"]),
            status=str(summary_dict["status"]),
        )
        numeric_summary = (
            summary.threshold,
            summary.mean_error,
            summary.max_error,
            summary.p95_error,
            summary.trusted_fraction,
        )
        if not all(np.isfinite(value) for value in numeric_summary):
            raise ValueError("non-finite summary")
        if summary.threshold <= 0.0 or summary.total_pixels != width * height:
            raise ValueError("summary dimensions")
        if summary.untrusted_pixels != int(np.count_nonzero(untrusted)):
            raise ValueError("summary untrusted count")
        expected_fraction = 1.0 - summary.untrusted_pixels / summary.total_pixels
        if not np.isclose(summary.trusted_fraction, expected_fraction, atol=1.0e-12):
            raise ValueError("summary trusted fraction")
        if not np.isclose(
            summary.max_error,
            float(np.max(error_map)),
            rtol=1.0e-5,
            atol=1.0e-7,
        ):
            raise ValueError("summary maximum")
    except (TypeError, ValueError) as exc:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot error summary is invalid",
        ) from exc
    mode = result["mode"]
    if mode not in PRECISION_MODES:
        raise VolRenderError(E_SNAPSHOT_INVALID, "snapshot result mode is invalid")
    content_hash = str(result["content_hash"])
    actual_hash = result_content_hash(rgba8, error_map, untrusted)
    if actual_hash != content_hash:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot content hash mismatch",
            details={"stored": content_hash, "actual": actual_hash},
        )
    return rgba8, error_map, untrusted, summary, content_hash, mode


def snapshot_to_dict(snapshot: Snapshot) -> dict[str, Any]:
    return copy.deepcopy(snapshot.raw)


def parse_snapshot(data: bytes | str | dict[str, Any]) -> Snapshot:
    if isinstance(data, bytes):
        try:
            document = json.loads(data.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise VolRenderError(E_SNAPSHOT_INVALID, "snapshot JSON is corrupt") from exc
    elif isinstance(data, str):
        try:
            document = json.loads(data)
        except json.JSONDecodeError as exc:
            raise VolRenderError(E_SNAPSHOT_INVALID, "snapshot JSON is corrupt") from exc
    elif isinstance(data, dict):
        document = copy.deepcopy(data)
    else:
        raise VolRenderError(E_SNAPSHOT_INVALID, "snapshot must be JSON bytes or an object")

    if not isinstance(document, dict):
        raise VolRenderError(E_SNAPSHOT_INVALID, "snapshot root must be an object")
    if document.get("schema") != SNAPSHOT_SCHEMA:
        raise VolRenderError(E_SNAPSHOT_INVALID, "snapshot schema is not recognized")
    try:
        schema_version = int(document.get("schema_version", -1))
    except (TypeError, ValueError) as exc:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot schema version is invalid",
        ) from exc
    if schema_version != SNAPSHOT_VERSION:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot schema version is not supported",
        )
    for field in ("state", "state_hash", "result", "snapshot_hash"):
        if field not in document:
            raise VolRenderError(
                E_SNAPSHOT_INVALID,
                "snapshot is missing a required field",
                details={"missing": field},
            )
    state = _validate_state_structure(document["state"])
    try:
        computed_state_hash = compute_state_hash(state)
    except (TypeError, ValueError) as exc:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot state cannot be canonicalized",
        ) from exc
    if computed_state_hash != document["state_hash"]:
        raise VolRenderError(
            E_SNAPSHOT_STALE,
            "snapshot state version changed without a new state hash",
            details={
                "stored_state_hash": document["state_hash"],
                "computed_state_hash": computed_state_hash,
            },
        )

    snapshot_hash = str(document["snapshot_hash"])
    unhashed = copy.deepcopy(document)
    del unhashed["snapshot_hash"]
    try:
        computed_snapshot_hash = hashlib.sha256(
            canonical_json_bytes(unhashed)
        ).hexdigest()
    except (TypeError, ValueError) as exc:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot document cannot be canonicalized",
        ) from exc
    if computed_snapshot_hash != snapshot_hash:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot integrity hash mismatch",
            details={
                "stored": snapshot_hash,
                "actual": computed_snapshot_hash,
            },
        )

    rgba8, error_map, untrusted, summary, content_hash, mode = _decode_result(
        document["result"]
    )
    if mode != state["precision_mode"]:
        raise VolRenderError(
            E_SNAPSHOT_STALE,
            "snapshot result mode does not match snapshot state",
        )
    if not np.isclose(
        summary.threshold,
        float(state["error_threshold"]),
        rtol=0.0,
        atol=1.0e-15,
    ):
        raise VolRenderError(
            E_SNAPSHOT_STALE,
            "snapshot error threshold does not match snapshot state",
        )
    return Snapshot(
        state=state,
        state_hash=computed_state_hash,
        rgba8=rgba8,
        error_map=error_map,
        untrusted=untrusted,
        error_summary=summary,
        content_hash=content_hash,
        mode=mode,
        snapshot_hash=snapshot_hash,
        raw=document,
    )


def load_snapshot(path: str | Path) -> Snapshot:
    try:
        data = Path(path).read_bytes()
    except OSError as exc:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            f"failed to read snapshot: {path}",
        ) from exc
    return parse_snapshot(data)


def save_snapshot(
    path: str | Path,
    result: RenderResult,
    *,
    expected_state_hash: str | None = None,
) -> Path:
    if expected_state_hash is not None and result.state_hash != expected_state_hash:
        raise VolRenderError(
            E_SNAPSHOT_STALE,
            "current renderer state does not match the expected snapshot version",
            details={
                "expected_state_hash": expected_state_hash,
                "actual_state_hash": result.state_hash,
            },
        )
    snapshot = Snapshot.from_result(result)
    out_path = Path(path)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    data = canonical_json_bytes(snapshot.raw) + b"\n"
    tmp_path = out_path.with_name(out_path.name + ".tmp")
    try:
        with tmp_path.open("wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp_path, out_path)
    except OSError as exc:
        try:
            tmp_path.unlink(missing_ok=True)
        except OSError:
            pass
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            f"failed to write snapshot: {out_path}",
        ) from exc
    return out_path


def _result_from_snapshot(snapshot: Snapshot) -> RenderResult:
    alpha = snapshot.rgba8[..., 3].astype(np.float64) / 255.0
    linear_rgb = snapshot.rgba8[..., :3].astype(np.float64) / 255.0
    return RenderResult(
        rgba8=snapshot.rgba8.copy(),
        linear_rgb=linear_rgb,
        alpha=alpha,
        error_map=snapshot.error_map.copy(),
        untrusted=snapshot.untrusted.copy(),
        error_summary=snapshot.error_summary,
        content_hash=snapshot.content_hash,
        state_hash=snapshot.state_hash,
        mode=snapshot.mode,
        state=copy.deepcopy(snapshot.state),
        elapsed_ms=0.0,
    )


def restore_snapshot(renderer: "VolumeRenderer", snapshot: Snapshot) -> Snapshot:
    """Validate all snapshot state before mutating the renderer.

    A successful restore leaves the renderer ready for ``resume_render()``.
    """

    state = copy.deepcopy(snapshot.state)
    if state["volume_id"] != renderer.volume.volume_id:
        raise VolRenderError(
            E_SNAPSHOT_STALE,
            "snapshot belongs to a different volume",
            details={
                "snapshot_volume_id": state["volume_id"],
                "renderer_volume_id": renderer.volume.volume_id,
            },
        )
    if snapshot.mode != state["precision_mode"]:
        raise VolRenderError(
            E_SNAPSHOT_STALE,
            "snapshot result mode does not match snapshot state",
        )
    try:
        candidate_transfer = TransferFunction(state["transfer_points"])
        candidate_lut = candidate_transfer.build_lut(
            state["precision_mode"],
            resolution=int(state["lut_resolution"]),
            fixed_bits=int(state["fixed_bits"]),
            fixed_color_bits=int(state["fixed_color_bits"]),
        )
        candidate_view = np.asarray(state["view_matrix"], dtype=np.float64)
        from .camera import validate_view_matrix

        candidate_view = validate_view_matrix(candidate_view)
        candidate_step = float(state["step"])
        candidate_threshold = float(state["error_threshold"])
        candidate_fov = float(state["fov_degrees"])
        candidate_image_size = tuple(int(v) for v in state["image_size"])
        candidate_tolerance = float(state["lighting_tolerance"])
        candidate_spacing = tuple(float(v) for v in state["spacing"])
        candidate_width = float(state["window_width"])
        candidate_level = float(state["window_level"])
        if (
            candidate_step <= 0.0
            or candidate_threshold <= 0.0
            or candidate_tolerance < 0.0
        ):
            raise ValueError("snapshot numeric controls are out of range")
        candidate_image_size = renderer._validate_image_size(candidate_image_size)
        candidate_fov = renderer._validate_fov(candidate_fov)
    except VolRenderError as exc:
        if exc.code == E_SNAPSHOT_STALE:
            raise
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot renderer state is invalid",
            details={"cause": exc.code, "message": exc.message},
        ) from exc
    except Exception as exc:
        raise VolRenderError(
            E_SNAPSHOT_INVALID,
            "snapshot renderer state is invalid",
        ) from exc

    old_window = (renderer.volume.window_width, renderer.volume.window_level)
    old_spacing = renderer.volume.spacing
    try:
        renderer.volume.set_spacing(candidate_spacing)
        renderer.volume.set_window(candidate_width, candidate_level)
        if renderer.volume.mapping_fingerprint() != state["mapping_fingerprint"]:
            raise VolRenderError(
                E_SNAPSHOT_STALE,
                "snapshot mapping fingerprint does not match the restored volume",
            )
        renderer.transfer = candidate_transfer
        renderer._lut = candidate_lut
        renderer.mode = state["precision_mode"]
        renderer.view_matrix = candidate_view
        renderer.step = candidate_step
        renderer.error_threshold = candidate_threshold
        renderer.fov_degrees = candidate_fov
        renderer.image_size = candidate_image_size
        renderer.lighting_tolerance = candidate_tolerance
        renderer.lut_resolution = int(state["lut_resolution"])
        renderer.fixed_bits = int(state["fixed_bits"])
        renderer.fixed_color_bits = int(state["fixed_color_bits"])
    except Exception:
        renderer.volume.set_spacing(old_spacing)
        renderer.volume.set_window(*old_window)
        raise

    restored_result = _result_from_snapshot(snapshot)
    renderer._publish(restored_result)
    return snapshot
