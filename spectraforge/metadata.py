"""Scene loading, metadata validation, and integrity checks."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import numpy as np

from .errors import WorkbenchError
from .imageio import read_raster
from .models import BandInfo, ExposureInfo, Scene, SceneMetadata
from .parameters import default_parameters
from .util import hash_object, read_json
from .versions import METADATA_SCHEMA_VERSION


def _require_key(value: dict[str, Any], key: str, *, location: str = "metadata") -> Any:
    if key not in value:
        raise WorkbenchError("E_META", "Required metadata field is missing", {"field": f"{location}.{key}"})
    return value[key]


def _array(value: Any, field: str, shape: tuple[int, ...] | None = None) -> np.ndarray:
    try:
        array = np.asarray(value, dtype=np.float64)
    except (TypeError, ValueError) as exc:
        raise WorkbenchError("E_META", "Metadata array is invalid", {"field": field}) from exc
    if not np.all(np.isfinite(array)):
        raise WorkbenchError("E_META", "Metadata array contains non-finite values", {"field": field})
    if shape is not None and array.shape != shape:
        raise WorkbenchError(
            "E_META",
            "Metadata array has an unexpected shape",
            {"field": field, "expected": list(shape), "actual": list(array.shape)},
        )
    return array


def _safe_scene_path(root: Path, relative: str) -> Path:
    candidate = (root / relative).resolve()
    try:
        candidate.relative_to(root.resolve())
    except ValueError as exc:
        raise WorkbenchError("E_META", "Metadata path escapes the scene directory", {"path": relative}) from exc
    return candidate


def _load_truth(path: Path) -> dict[str, Any] | None:
    if not path.exists():
        raise WorkbenchError("E_META", "Analytical truth file is missing", {"path": str(path)})
    truth = read_json(path)
    patches = truth.get("patches")
    if not isinstance(patches, list):
        raise WorkbenchError("E_META", "Truth file has no patch list", {"path": str(path)})
    return truth


def load_scene(scene_dir: str | Path, *, load_truth: bool = True) -> Scene:
    root = Path(scene_dir).resolve()
    metadata_path = root / "scene.json"
    raw = read_json(metadata_path, code="E_META", label="scene metadata")
    schema_version = str(_require_key(raw, "schema_version"))
    if schema_version != METADATA_SCHEMA_VERSION:
        raise WorkbenchError(
            "E_META",
            "Unsupported metadata schema version",
            {"field": "schema_version", "value": schema_version, "supported": METADATA_SCHEMA_VERSION},
        )
    declared_hash = str(_require_key(raw, "metadata_hash"))
    hash_input = {key: value for key, value in raw.items() if key != "metadata_hash"}
    actual_hash = hash_object(hash_input)
    if declared_hash != actual_hash:
        raise WorkbenchError(
            "E_META",
            "Metadata hash does not match its contents",
            {"field": "metadata_hash", "declared": declared_hash, "actual": actual_hash},
        )
    try:
        width = int(_require_key(raw, "width"))
        height = int(_require_key(raw, "height"))
        pixel_pitch_um = float(_require_key(raw, "pixel_pitch_um"))
        band_count = int(_require_key(raw, "band_count"))
        exposure_count = int(_require_key(raw, "exposure_count"))
        seed = int(_require_key(raw, "seed"))
        scene_id = str(_require_key(raw, "scene_id"))
        title = str(_require_key(raw, "title"))
        metadata_version = str(_require_key(raw, "metadata_version"))
        bands_raw = _require_key(raw, "bands")
        exposures_raw = _require_key(raw, "exposures")
    except (TypeError, ValueError) as exc:
        raise WorkbenchError("E_META", "Metadata scalar field has the wrong type", {"reason": str(exc)}) from exc
    if width <= 0 or height <= 0:
        raise WorkbenchError("E_META", "Scene dimensions must be positive", {"width": width, "height": height})
    if pixel_pitch_um <= 0:
        raise WorkbenchError("E_META", "Pixel pitch must be positive", {"field": "pixel_pitch_um"})
    if not isinstance(bands_raw, list) or len(bands_raw) != band_count:
        raise WorkbenchError(
            "E_META",
            "Declared band count does not match the band list",
            {"band_count": band_count, "list_length": len(bands_raw) if isinstance(bands_raw, list) else None},
        )
    if not isinstance(exposures_raw, list) or len(exposures_raw) != exposure_count:
        raise WorkbenchError(
            "E_META",
            "Declared exposure count does not match the exposure list",
            {
                "exposure_count": exposure_count,
                "list_length": len(exposures_raw) if isinstance(exposures_raw, list) else None,
            },
        )
    bands: list[BandInfo] = []
    for index, item in enumerate(bands_raw):
        if not isinstance(item, dict):
            raise WorkbenchError("E_META", "Band entry must be an object", {"field": f"bands[{index}]"})
        try:
            band = BandInfo(
                index=int(item.get("index", index)),
                name=str(item["name"]),
                file=str(item["file"]),
                center_nm=float(item["center_nm"]),
                sigma_nm=float(item["sigma_nm"]),
                bit_depth=int(item["bit_depth"]),
                radiometric_scale=float(item["radiometric_scale"]),
                sha256=str(item["sha256"]),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise WorkbenchError("E_META", "Band metadata entry is invalid", {"field": f"bands[{index}]", "reason": str(exc)}) from exc
        if band.index != index:
            raise WorkbenchError("E_META", "Band indexes must be contiguous", {"field": f"bands[{index}].index"})
        if band.bit_depth < 1 or band.bit_depth > 16:
            raise WorkbenchError("E_META", "Band bit depth is unsupported", {"field": f"bands[{index}].bit_depth"})
        if band.radiometric_scale <= 0:
            raise WorkbenchError("E_META", "Band radiometric scale must be positive", {"field": f"bands[{index}].radiometric_scale"})
        bands.append(band)
    exposures: list[ExposureInfo] = []
    for index, item in enumerate(exposures_raw):
        if not isinstance(item, dict):
            raise WorkbenchError("E_META", "Exposure entry must be an object", {"field": f"exposures[{index}]"})
        try:
            exposure = ExposureInfo(
                index=int(item.get("index", index)),
                name=str(item["name"]),
                file=str(item["file"]),
                exposure_time=float(item["exposure_time"]),
                bit_depth=int(item["bit_depth"]),
                sha256=str(item["sha256"]),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise WorkbenchError("E_META", "Exposure metadata entry is invalid", {"field": f"exposures[{index}]", "reason": str(exc)}) from exc
        if exposure.index != index:
            raise WorkbenchError("E_META", "Exposure indexes must be contiguous", {"field": f"exposures[{index}].index"})
        if exposure.bit_depth < 1 or exposure.bit_depth > 16:
            raise WorkbenchError("E_META", "Exposure bit depth is unsupported", {"field": f"exposures[{index}].bit_depth"})
        if exposure.exposure_time <= 0 or not np.isfinite(exposure.exposure_time):
            raise WorkbenchError("E_META", "Exposure time must be positive", {"field": f"exposures[{index}].exposure_time"})
        exposures.append(exposure)
    wave_grid = _array(_require_key(raw, "wave_grid_nm"), "wave_grid_nm")
    if wave_grid.ndim != 1 or wave_grid.size < band_count:
        raise WorkbenchError("E_META", "Wavelength grid is too small", {"field": "wave_grid_nm"})
    responses = _array(_require_key(raw, "responses"), "responses", (wave_grid.size, band_count))
    xyz_basis = _array(_require_key(raw, "xyz_basis"), "xyz_basis", (band_count, 3))
    scene_white = _array(_require_key(raw, "scene_white_xyz"), "scene_white_xyz", (3,))
    target_white = _array(_require_key(raw, "target_white_xyz"), "target_white_xyz", (3,))
    offsets = _array(_require_key(raw, "registration_offsets"), "registration_offsets", (band_count, 2))
    band_cube = np.empty((band_count, height, width), dtype=np.uint16)
    exposure_cube = np.empty((exposure_count, height, width), dtype=np.uint16)
    for band in bands:
        path = _safe_scene_path(root, band.file)
        raster = read_raster(path)
        if raster.header.get("payload_sha256") != band.sha256:
            raise WorkbenchError(
                "E_DECODE",
                "Band payload hash differs from metadata",
                {"file": band.file, "declared": band.sha256, "actual": raster.header.get("payload_sha256")},
            )
        if raster.data.shape != (height, width):
            raise WorkbenchError(
                "E_DIM",
                "Band raster dimensions differ from metadata",
                {
                    "file": band.file,
                    "expected": [height, width],
                    "actual": list(raster.data.shape),
                },
            )
        if int(raster.header.get("bit_depth", -1)) != band.bit_depth:
            raise WorkbenchError(
                "E_META",
                "Band bit depth differs from metadata",
                {"file": band.file, "declared": band.bit_depth, "actual": raster.header.get("bit_depth")},
            )
        band_cube[band.index] = raster.data.astype(np.uint16, copy=False)
    for exposure in exposures:
        path = _safe_scene_path(root, exposure.file)
        raster = read_raster(path)
        if raster.header.get("payload_sha256") != exposure.sha256:
            raise WorkbenchError(
                "E_DECODE",
                "Exposure payload hash differs from metadata",
                {"file": exposure.file, "declared": exposure.sha256, "actual": raster.header.get("payload_sha256")},
            )
        if raster.data.shape != (height, width):
            raise WorkbenchError(
                "E_DIM",
                "Exposure raster dimensions differ from metadata",
                {
                    "file": exposure.file,
                    "expected": [height, width],
                    "actual": list(raster.data.shape),
                },
            )
        if int(raster.header.get("bit_depth", -1)) != exposure.bit_depth:
            raise WorkbenchError(
                "E_META",
                "Exposure bit depth differs from metadata",
                {"file": exposure.file, "declared": exposure.bit_depth, "actual": raster.header.get("bit_depth")},
            )
        exposure_cube[exposure.index] = raster.data.astype(np.uint16, copy=False)
    camera_response = raw.get("camera_response", {"gamma": 2.2})
    if not isinstance(camera_response, dict):
        raise WorkbenchError("E_META", "Camera response metadata is invalid", {"field": "camera_response"})
    truth_file = str(_require_key(raw, "truth_file"))
    truth = _load_truth(_safe_scene_path(root, truth_file)) if load_truth else None
    params = default_parameters(exposure_count)
    supplied_defaults = raw.get("default_parameters", {})
    if isinstance(supplied_defaults, dict):
        params.update(supplied_defaults)
    metadata = SceneMetadata(
        root=root,
        raw=raw,
        scene_id=scene_id,
        title=title,
        seed=seed,
        width=width,
        height=height,
        pixel_pitch_um=pixel_pitch_um,
        metadata_version=metadata_version,
        metadata_hash=declared_hash,
        bands=bands,
        exposures=exposures,
        wave_grid_nm=wave_grid,
        responses=responses,
        xyz_basis=xyz_basis,
        scene_white_xyz=scene_white,
        target_white_xyz=target_white,
        camera_response={
            key: float(value)
            for key, value in camera_response.items()
            if isinstance(value, (int, float)) and not isinstance(value, bool)
        },
        registration_offsets=offsets,
        truth_file=truth_file,
        default_parameters=params,
    )
    return Scene(
        metadata=metadata,
        band_raw=band_cube,
        exposure_raw=exposure_cube,
        truth=truth,
    )


def band_radiance(scene: Scene, aligned: np.ndarray | None = None) -> np.ndarray:
    source = scene.band_raw if aligned is None else aligned
    if source is scene.band_raw:
        values = np.empty_like(source, dtype=np.float32)
        for band in scene.metadata.bands:
            max_code = float((1 << band.bit_depth) - 1)
            values[band.index] = (
                source[band.index].astype(np.float32) / max_code * band.radiometric_scale
            )
        return values
    return np.asarray(source, dtype=np.float32)
