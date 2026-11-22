"""Strict sequence loader for generated scientific imaging datasets."""

from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path
from typing import Any

import numpy as np

from .errors import SciSeqError
from .models import FeatureSpec, Sequence, SequenceTruth

REQUIRED_MANIFEST_FIELDS = (
    "schema_version",
    "sequence_id",
    "seed",
    "height",
    "width",
    "dtype",
    "frame_count",
    "reference_frame",
    "frame_files",
    "frame_sha256",
    "truth_file",
    "clean_base_file",
    "clean_base_sha256",
)


def _meta(message: str, field: str, *, frame: int | None = None, **details: Any) -> SciSeqError:
    return SciSeqError("E_META", message, frame=frame, field=field, details=details)


def _decode(message: str, field: str, *, frame: int | None = None, **details: Any) -> SciSeqError:
    return SciSeqError("E_DECODE", message, frame=frame, field=field, details=details)


def _read_json(path: Path, field: str) -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8") as handle:
            payload = json.load(handle)
    except FileNotFoundError as exc:
        raise _meta("metadata file is missing", field, path=str(path)) from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise _meta("metadata file cannot be decoded", field, path=str(path), reason=str(exc)) from exc
    if not isinstance(payload, dict):
        raise _meta("metadata root must be an object", field, path=str(path))
    return payload


def _require(mapping: dict[str, Any], key: str, prefix: str) -> Any:
    if key not in mapping:
        raise _meta("required metadata field is missing", f"{prefix}.{key}")
    return mapping[key]


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _validate_frame_file(
    path: Path,
    frame_index: int,
    expected_shape: tuple[int, int],
    expected_dtype: str,
    expected_hash: str,
) -> None:
    if not path.is_file():
        raise _decode("frame file is missing", "frame_file", frame=frame_index, path=str(path))
    try:
        loaded = np.load(path, mmap_mode="r", allow_pickle=False)
    except (OSError, ValueError, EOFError) as exc:
        raise _decode(
            "frame file cannot be decoded",
            "frame_file",
            frame=frame_index,
            path=str(path),
            reason=str(exc),
        ) from exc
    if loaded.shape != expected_shape:
        raise _meta(
            "frame shape does not match manifest",
            "shape",
            frame=frame_index,
            expected=list(expected_shape),
            actual=list(loaded.shape),
        )
    if str(loaded.dtype) != expected_dtype:
        raise _meta(
            "frame dtype does not match manifest",
            "dtype",
            frame=frame_index,
            expected=expected_dtype,
            actual=str(loaded.dtype),
        )
    if not np.issubdtype(loaded.dtype, np.number):
        raise _meta("frame dtype is not numeric", "dtype", frame=frame_index, actual=str(loaded.dtype))
    try:
        actual_hash = _sha256_file(path)
    except OSError as exc:
        raise _decode(
            "frame file cannot be read",
            "frame_file",
            frame=frame_index,
            path=str(path),
            reason=str(exc),
        ) from exc
    if actual_hash != expected_hash:
        raise _decode(
            "frame content hash mismatch",
            "frame_sha256",
            frame=frame_index,
            expected=expected_hash,
            actual=actual_hash,
        )


def load_sequence(
    sequence_dir: Path | str,
    *,
    verify_hashes: bool = True,
) -> Sequence:
    root = Path(sequence_dir).resolve()
    manifest_path = root / "manifest.json"
    manifest = _read_json(manifest_path, "manifest")
    for key in REQUIRED_MANIFEST_FIELDS:
        _require(manifest, key, "manifest")

    if str(manifest["schema_version"]) != "1.0":
        raise _meta(
            "unsupported schema version",
            "schema_version",
            expected="1.0",
            actual=manifest["schema_version"],
        )

    try:
        height = int(manifest["height"])
        width = int(manifest["width"])
        frame_count = int(manifest["frame_count"])
        reference_frame = int(manifest["reference_frame"])
    except (TypeError, ValueError) as exc:
        raise _meta("numeric metadata field has invalid type", "frame_count", reason=str(exc)) from exc
    if height <= 0 or width <= 0:
        raise _meta("frame dimensions must be positive", "height/width", height=height, width=width)
    if frame_count <= 0:
        raise _meta("frame_count must be positive", "frame_count", actual=frame_count)
    if not 0 <= reference_frame < frame_count:
        raise _meta(
            "reference_frame outside sequence",
            "reference_frame",
            allowed=(0, frame_count - 1),
            actual=reference_frame,
        )

    frame_files = _require(manifest, "frame_files", "manifest")
    frame_hashes = _require(manifest, "frame_sha256", "manifest")
    if not isinstance(frame_files, list) or not isinstance(frame_hashes, list):
        raise _meta("frame file metadata must be arrays", "frame_files/frame_sha256")
    if len(frame_files) != frame_count:
        raise _meta(
            "frame file count does not match frame_count",
            "frame_count",
            expected=frame_count,
            actual=len(frame_files),
        )
    if len(frame_hashes) != frame_count:
        raise _meta(
            "frame hash count does not match frame_count",
            "frame_sha256",
            expected=frame_count,
            actual=len(frame_hashes),
        )

    truth_path = root / str(_require(manifest, "truth_file", "manifest"))
    truth_payload = _read_json(truth_path, "truth")
    for key in (
        "schema_version",
        "reference_frame",
        "shifts",
        "noise_sigmas",
        "features",
        "frame_features",
        "clean_base_file",
        "clean_base_sha256",
    ):
        _require(truth_payload, key, "truth")

    if int(truth_payload["reference_frame"]) != reference_frame:
        raise _meta(
            "truth reference_frame does not match manifest",
            "truth.reference_frame",
            expected=reference_frame,
            actual=truth_payload["reference_frame"],
        )
    shifts_payload = truth_payload["shifts"]
    if not isinstance(shifts_payload, list) or len(shifts_payload) != frame_count:
        raise _meta(
            "truth shift count does not match frame_count",
            "truth.shifts",
            expected=frame_count,
            actual=len(shifts_payload) if isinstance(shifts_payload, list) else None,
        )
    shifts = np.zeros((frame_count, 2), dtype=np.float64)
    for index, item in enumerate(shifts_payload):
        if not isinstance(item, dict) or "dx" not in item or "dy" not in item:
            raise _meta(
                "truth shift fields are missing",
                "truth.shifts.dx/dy",
                frame=index,
            )
        try:
            dx = float(item["dx"])
            dy = float(item["dy"])
        except (TypeError, ValueError) as exc:
            raise _meta(
                "truth shift is not numeric",
                "truth.shifts",
                frame=index,
                reason=str(exc),
            ) from exc
        if not math.isfinite(dx) or not math.isfinite(dy):
            raise _meta(
                "truth shift is not finite",
                "truth.shifts",
                frame=index,
                dx=dx,
                dy=dy,
            )
        shifts[index] = (dx, dy)

    noise_payload = truth_payload["noise_sigmas"]
    if not isinstance(noise_payload, list) or len(noise_payload) != frame_count:
        raise _meta("noise sigma count does not match frame_count", "truth.noise_sigmas")
    try:
        noise_sigmas = np.asarray(noise_payload, dtype=np.float64)
    except (TypeError, ValueError) as exc:
        raise _meta("noise sigmas are not numeric", "truth.noise_sigmas", reason=str(exc)) from exc
    if noise_sigmas.shape != (frame_count,) or not np.all(np.isfinite(noise_sigmas)):
        raise _meta(
            "noise sigmas must be finite and match frame_count",
            "truth.noise_sigmas",
            shape=list(noise_sigmas.shape),
        )

    features_payload = truth_payload["features"]
    if not isinstance(features_payload, list):
        raise _meta("features must be an array", "truth.features")
    features: list[FeatureSpec] = []
    for index, item in enumerate(features_payload):
        if not isinstance(item, dict):
            raise _meta("feature entry must be an object", "truth.features", frame=None)
        for key in ("feature_id", "kind", "center", "sigma", "amplitude"):
            if key not in item:
                raise _meta(
                    "feature field is missing",
                    f"truth.features[{index}].{key}",
                )
        center = item["center"]
        if not isinstance(center, list) or len(center) != 2:
            raise _meta("feature center must contain x and y", f"truth.features[{index}].center")
        try:
            cx, cy = float(center[0]), float(center[1])
            sigma = float(item["sigma"])
            amplitude = float(item["amplitude"])
        except (TypeError, ValueError) as exc:
            raise _meta(
                "feature values are not numeric",
                f"truth.features[{index}]",
                reason=str(exc),
            ) from exc
        if not all(math.isfinite(value) for value in (cx, cy, sigma, amplitude)):
            raise _meta(
                "feature values must be finite",
                f"truth.features[{index}]",
            )
        endpoints: tuple[tuple[float, float], tuple[float, float]] | None = None
        if item.get("endpoints") is not None:
            raw_endpoints = item["endpoints"]
            if not isinstance(raw_endpoints, list) or len(raw_endpoints) != 2:
                raise _meta(
                    "line endpoints must contain two points",
                    f"truth.features[{index}].endpoints",
                )
            endpoints = (
                (float(raw_endpoints[0][0]), float(raw_endpoints[0][1])),
                (float(raw_endpoints[1][0]), float(raw_endpoints[1][1])),
            )
        features.append(
            FeatureSpec(
                feature_id=str(item["feature_id"]),
                kind=str(item["kind"]),
                center=(cx, cy),
                sigma=sigma,
                amplitude=amplitude,
                endpoints=endpoints,
            )
        )

    frame_features_payload = truth_payload["frame_features"]
    if not isinstance(frame_features_payload, list) or len(frame_features_payload) != frame_count:
        raise _meta(
            "frame feature count does not match frame_count",
            "truth.frame_features",
        )
    frame_features: list[dict[str, tuple[float, float]]] = []
    for frame_index, frame_map in enumerate(frame_features_payload):
        if not isinstance(frame_map, dict):
            raise _meta(
                "frame feature entry must be an object",
                "truth.frame_features",
                frame=frame_index,
            )
        converted: dict[str, tuple[float, float]] = {}
        for feature_id, center in frame_map.items():
            if not isinstance(center, list) or len(center) != 2:
                raise _meta(
                    "frame feature center must contain x and y",
                    "truth.frame_features",
                    frame=frame_index,
                )
            cx, cy = float(center[0]), float(center[1])
            if not math.isfinite(cx) or not math.isfinite(cy):
                raise _meta(
                    "frame feature center is not finite",
                    "truth.frame_features",
                    frame=frame_index,
                )
            converted[str(feature_id)] = (cx, cy)
        frame_features.append(converted)

    expected_dtype = str(manifest["dtype"])
    shape = (height, width)
    frame_paths: list[Path] = []
    for index, relative in enumerate(frame_files):
        path = (root / str(relative)).resolve()
        try:
            path.relative_to(root)
        except ValueError as exc:
            raise _meta(
                "frame path escapes sequence root",
                "frame_files",
                frame=index,
                path=str(path),
            ) from exc
        frame_paths.append(path)
        if verify_hashes:
            _validate_frame_file(path, index, shape, expected_dtype, str(frame_hashes[index]))
        elif not path.is_file():
            raise _decode("frame file is missing", "frame_file", frame=index, path=str(path))

    clean_base_path = (root / str(truth_payload["clean_base_file"])).resolve()
    if not clean_base_path.is_file():
        raise _decode(
            "clean base file is missing",
            "clean_base_file",
            path=str(clean_base_path),
        )
    try:
        clean_base = np.load(clean_base_path, mmap_mode="r", allow_pickle=False)
    except (OSError, ValueError, EOFError) as exc:
        raise _decode(
            "clean base file cannot be decoded",
            "clean_base_file",
            path=str(clean_base_path),
            reason=str(exc),
        ) from exc
    if clean_base.shape != shape:
        raise _meta(
            "clean base shape does not match manifest",
            "clean_base_shape",
            expected=list(shape),
            actual=list(clean_base.shape),
        )
    clean_base_hash = str(truth_payload["clean_base_sha256"])
    if verify_hashes and _sha256_file(clean_base_path) != clean_base_hash:
        raise _decode(
            "clean base content hash mismatch",
            "clean_base_sha256",
            expected=clean_base_hash,
        )

    truth = SequenceTruth(
        reference_frame=reference_frame,
        shifts=shifts,
        noise_sigmas=noise_sigmas,
        features=tuple(features),
        frame_features=tuple(frame_features),
    )
    return Sequence(
        sequence_id=str(manifest["sequence_id"]),
        root=root,
        shape=shape,
        frame_count=frame_count,
        dtype=expected_dtype,
        reference_frame=reference_frame,
        frame_paths=tuple(frame_paths),
        frame_hashes=tuple(str(value) for value in frame_hashes),
        truth=truth,
        clean_base_path=clean_base_path,
        clean_base_hash=clean_base_hash,
        manifest=manifest,
    )

