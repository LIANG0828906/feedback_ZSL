"""Dataset index loading and strict scene validation."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from . import ALGORITHM_VERSION, DATASET_SCHEMA_VERSION
from .color import SRGB_WHITE_XYZ, SRGB_XYZ_TO_LINEAR_RGB
from .errors import WorkbenchError, require
from .io_utils import read_json, sha256_file, value_version


REQUIRED_METADATA_FIELDS = (
    "schema_version",
    "scene_id",
    "pixel_size",
    "reference_band",
    "bands",
    "exposures",
    "detail",
    "spectral",
    "target",
    "max_registration_shift_px",
    "max_interpolation_order",
    "metadata_version",
)
REQUIRED_BAND_FIELDS = (
    "index",
    "name",
    "center_nm",
    "fwhm_nm",
    "file",
    "dtype",
    "bit_depth",
    "shape",
    "sha256",
)
REQUIRED_TRUTH_FIELDS = ("schema_version", "registration_offsets_px", "patches", "quality", "metadata_version")


def _missing_field(mapping: Any, field: str, prefix: str = "") -> None:
    if not isinstance(mapping, dict) or field not in mapping:
        full_name = f"{prefix}.{field}" if prefix else field
        raise WorkbenchError("E_META", f"Missing required metadata field: {full_name}", field=full_name)


def _preflight_structure(metadata: dict[str, Any], dataset_record: dict[str, Any] | None) -> None:
    """Resolve structural/order errors before stale-version errors.

    This keeps the public anomaly contract stable: missing bands and bad
    exposure order are reported as E_META/E_ORDER even when an operator has
    edited metadata without re-signing its version string. Bit-depth drift is
    intentionally left to the version guard below so it reports E_VER.
    """

    pixel_size = metadata["pixel_size"]
    require(
        isinstance(pixel_size, list)
        and len(pixel_size) == 2
        and all(isinstance(value, int) and value > 0 for value in pixel_size),
        "E_META",
        "pixel_size must be [height, width]",
        field="pixel_size",
    )
    bands = metadata["bands"]
    require(isinstance(bands, list), "E_META", "bands must be a list", field="bands")
    if dataset_record is not None:
        require(
            len(bands) == int(dataset_record.get("band_count", len(bands))),
            "E_META",
            "Declared band count does not match dataset index",
            field="bands",
            expected=int(dataset_record.get("band_count", -1)),
            actual=len(bands),
        )
    require(len(bands) >= 3, "E_META", "At least three spectral bands are required", field="bands")
    for position, band in enumerate(bands):
        if not isinstance(band, dict):
            raise WorkbenchError("E_META", "Band entry must be an object", field=f"bands[{position}]")
        for field in REQUIRED_BAND_FIELDS:
            _missing_field(band, field, f"bands[{position}]")

    exposures = metadata["exposures"]
    for field in ("bit_depth", "dtype", "response", "frames"):
        _missing_field(exposures, field, "exposures")
    frames = exposures["frames"]
    require(isinstance(frames, list), "E_META", "exposures.frames must be a list", field="exposures.frames")
    if dataset_record is not None:
        require(
            len(frames) == int(dataset_record.get("exposure_count", len(frames))),
            "E_META",
            "Declared exposure count does not match dataset index",
            field="exposures.frames",
            expected=int(dataset_record.get("exposure_count", -1)),
            actual=len(frames),
        )
    require(len(frames) >= 3, "E_META", "At least three exposure frames are required", field="exposures.frames")
    times: list[float] = []
    for position, frame in enumerate(frames):
        if not isinstance(frame, dict):
            raise WorkbenchError("E_META", "Exposure frame entry must be an object", field=f"exposures.frames[{position}]")
        for field in ("file", "exposure_time", "dtype", "shape", "sha256"):
            _missing_field(frame, field, f"exposures.frames[{position}]")
        try:
            value = float(frame["exposure_time"])
        except (TypeError, ValueError) as exc:
            raise WorkbenchError(
                "E_ORDER",
                "Exposure time must be numeric",
                field=f"exposures.frames[{position}].exposure_time",
            ) from exc
        if not np.isfinite(value) or value <= 0.0:
            raise WorkbenchError(
                "E_ORDER",
                "Exposure times must be finite and positive",
                field=f"exposures.frames[{position}].exposure_time",
                details={"index": position, "exposure_time": value},
            )
        times.append(value)
    for position in range(1, len(times)):
        if times[position] <= times[position - 1]:
            raise WorkbenchError(
                "E_ORDER",
                "Exposure times must be strictly increasing in frame order",
                field=f"exposures.frames[{position}].exposure_time",
                details={
                    "previous_index": position - 1,
                    "previous_exposure_time": times[position - 1],
                    "index": position,
                    "exposure_time": times[position],
                },
            )


def _load_array(
    path: Path,
    *,
    field: str,
    expected_sha256: str,
    expected_dtype: str,
    expected_shape: list[int] | tuple[int, ...],
    bit_depth: int,
    decode_scale: float = 1.0,
) -> np.ndarray:
    if not path.exists():
        raise WorkbenchError("E_META", f"Missing image file for {field}: {path}", field=field, path=str(path))
    actual_hash = sha256_file(path)
    if actual_hash != expected_sha256:
        raise WorkbenchError(
            "E_DECODE",
            f"Pixel payload hash mismatch for {field}; refusing to use or fill corrupted data",
            field=field,
            details={"path": str(path), "expected_sha256": expected_sha256, "actual_sha256": actual_hash},
        )
    try:
        array = np.load(path, allow_pickle=False)
    except Exception as exc:
        raise WorkbenchError(
            "E_DECODE",
            f"Cannot decode pixel payload for {field}: {exc}",
            field=field,
            details={"path": str(path)},
        ) from exc
    if not isinstance(array, np.ndarray):
        raise WorkbenchError("E_DECODE", f"Decoded object is not an ndarray: {path}", field=field)
    if array.dtype.name != expected_dtype:
        raise WorkbenchError(
            "E_DECODE",
            f"Decoded dtype does not match declaration for {field}",
            field=field,
            details={"expected": expected_dtype, "actual": array.dtype.name},
        )
    if tuple(array.shape) != tuple(expected_shape):
        raise WorkbenchError(
            "E_DECODE",
            f"Decoded shape does not match declaration for {field}",
            field=field,
            details={"expected": list(expected_shape), "actual": list(array.shape)},
        )
    maximum = (1 << int(bit_depth)) - 1
    if array.size and (int(array.min()) < 0 or int(array.max()) > maximum):
        raise WorkbenchError(
            "E_DECODE",
            f"Decoded pixel values exceed declared {bit_depth}-bit range for {field}",
            field=field,
            details={"maximum_allowed": maximum, "actual_minimum": int(array.min()), "actual_maximum": int(array.max())},
        )
    return array.astype(np.float64) / float(decode_scale)


@dataclass
class LoadedScene:
    scene_root: Path
    metadata: dict[str, Any]
    truth: dict[str, Any]
    bands: np.ndarray
    exposure_frames: np.ndarray
    detail_layer: np.ndarray
    clean_detail: np.ndarray
    registration_offsets: np.ndarray
    xyz_weights: np.ndarray
    source_white_xyz: np.ndarray
    target_white_xyz: np.ndarray
    target_matrix: np.ndarray
    metadata_version: str

    @property
    def scene_id(self) -> str:
        return str(self.metadata["scene_id"])

    @property
    def pixel_size(self) -> tuple[int, int]:
        height, width = self.metadata["pixel_size"]
        return int(height), int(width)

    @property
    def target_bit_depth(self) -> int:
        return int(self.metadata["target"]["bit_depth"])

    @property
    def reference_band(self) -> int:
        return int(self.metadata["reference_band"])

    @property
    def exposure_times(self) -> np.ndarray:
        return np.asarray([frame["exposure_time"] for frame in self.metadata["exposures"]["frames"]], dtype=np.float64)

    def metadata_snapshot(self) -> dict[str, Any]:
        return {
            "metadata_version": self.metadata_version,
            "scene_id": self.scene_id,
            "pixel_size": list(self.pixel_size),
            "reference_band": self.reference_band,
            "bands": [
                {
                    "index": int(band["index"]),
                    "center_nm": float(band["center_nm"]),
                    "fwhm_nm": float(band["fwhm_nm"]),
                    "bit_depth": int(band["bit_depth"]),
                }
                for band in self.metadata["bands"]
            ],
            "exposures": {
                "frame_count": len(self.metadata["exposures"]["frames"]),
                "exposure_times": self.exposure_times.tolist(),
                "response": self.metadata["exposures"]["response"],
            },
            "detail": {
                "bit_depth": int(self.metadata["detail"]["bit_depth"]),
                "neutral_value": float(self.metadata["detail"].get("neutral_value", 0.5)),
            },
            "spectral": {
                "xyz_weights": self.xyz_weights.tolist(),
                "source_white_xyz": self.source_white_xyz.tolist(),
            },
            "target": {
                "name": self.metadata["target"]["name"],
                "white_xyz": self.target_white_xyz.tolist(),
                "bit_depth": self.target_bit_depth,
                "gamut_intent": self.metadata["target"].get("gamut_intent", "perceptual"),
            },
        }


@dataclass
class DatasetIndex:
    path: Path
    data: dict[str, Any]

    @classmethod
    def load(cls, path: Path) -> "DatasetIndex":
        path = Path(path)
        data = read_json(path, field="dataset")
        _missing_field(data, "schema_version", "dataset")
        _missing_field(data, "algorithm_version", "dataset")
        _missing_field(data, "scenes", "dataset")
        if data["schema_version"] != DATASET_SCHEMA_VERSION:
            raise WorkbenchError(
                "E_VER",
                "Dataset schema version is not supported",
                field="dataset.schema_version",
                details={"expected": DATASET_SCHEMA_VERSION, "actual": data["schema_version"]},
            )
        if data["algorithm_version"] != ALGORITHM_VERSION:
            raise WorkbenchError(
                "E_VER",
                "Dataset algorithm version is not supported",
                field="dataset.algorithm_version",
                details={"expected": ALGORITHM_VERSION, "actual": data["algorithm_version"]},
            )
        if not isinstance(data["scenes"], list) or not data["scenes"]:
            raise WorkbenchError("E_META", "Dataset index contains no scenes", field="dataset.scenes")
        return cls(path=path, data=data)

    @property
    def scene_records(self) -> list[dict[str, Any]]:
        return list(self.data["scenes"])

    def scene_root(self, scene_id: str) -> Path:
        for record in self.scene_records:
            if record.get("scene_id") == scene_id:
                return self.path.parent / str(record["directory"])
        raise WorkbenchError("E_META", f"Scene not found in dataset index: {scene_id}", field="scene_id")

    def record(self, scene_id: str) -> dict[str, Any]:
        for record in self.scene_records:
            if record.get("scene_id") == scene_id:
                return record
        raise WorkbenchError("E_META", f"Scene not found in dataset index: {scene_id}", field="scene_id")

    def load_scene(self, scene_id: str) -> LoadedScene:
        record = self.record(scene_id)
        return load_scene(self.scene_root(scene_id), dataset_record=record)


def load_scene(scene_root: Path, *, dataset_record: dict[str, Any] | None = None) -> LoadedScene:
    """Load and validate one scene without filling, repairing, or guessing data."""

    scene_root = Path(scene_root)
    metadata_path = scene_root / "metadata.json"
    truth_path = scene_root / "truth.json"
    metadata = read_json(metadata_path, field="metadata")
    truth = read_json(truth_path, field="truth")

    for field in REQUIRED_METADATA_FIELDS:
        _missing_field(metadata, field)
    for field in REQUIRED_TRUTH_FIELDS:
        _missing_field(truth, field)
    if metadata["schema_version"] != DATASET_SCHEMA_VERSION:
        raise WorkbenchError(
            "E_VER",
            "Scene metadata schema version is not supported",
            field="metadata.schema_version",
            details={"expected": DATASET_SCHEMA_VERSION, "actual": metadata["schema_version"]},
        )
    if truth["schema_version"] != DATASET_SCHEMA_VERSION:
        raise WorkbenchError(
            "E_VER",
            "Scene truth schema version is not supported",
            field="truth.schema_version",
            details={"expected": DATASET_SCHEMA_VERSION, "actual": truth["schema_version"]},
        )

    _preflight_structure(metadata, dataset_record)

    metadata_without_version = dict(metadata)
    declared_metadata_version = metadata_without_version.pop("metadata_version", None)
    calculated_metadata_version = value_version(metadata_without_version)
    if calculated_metadata_version != declared_metadata_version:
        raise WorkbenchError(
            "E_VER",
            "Metadata content does not match metadata_version; treating the scene snapshot as stale",
            field="metadata_version",
            details={
                "declared_metadata_version": declared_metadata_version,
                "calculated_metadata_version": calculated_metadata_version,
            },
        )
    if truth["metadata_version"] != declared_metadata_version:
        raise WorkbenchError(
            "E_VER",
            "Truth metadata version does not match scene metadata version",
            field="truth.metadata_version",
            details={"metadata": declared_metadata_version, "truth": truth["metadata_version"]},
        )
    if dataset_record is not None and dataset_record.get("metadata_version") != declared_metadata_version:
        raise WorkbenchError(
            "E_VER",
            "Dataset index metadata version does not match scene metadata",
            field="dataset.metadata_version",
            details={
                "dataset": dataset_record.get("metadata_version"),
                "scene": declared_metadata_version,
            },
        )

    pixel_size = metadata["pixel_size"]
    require(
        isinstance(pixel_size, list)
        and len(pixel_size) == 2
        and all(isinstance(value, int) and value > 0 for value in pixel_size),
        "E_META",
        "pixel_size must be [height, width]",
        field="pixel_size",
    )
    height, width = int(pixel_size[0]), int(pixel_size[1])
    if dataset_record is not None and dataset_record.get("pixel_size") is not None:
        require(
            list(dataset_record["pixel_size"]) == [height, width],
            "E_META",
            "Scene pixel_size does not match dataset index",
            field="pixel_size",
            expected=dataset_record["pixel_size"],
            actual=[height, width],
        )
    bands_raw = metadata["bands"]
    require(isinstance(bands_raw, list), "E_META", "bands must be a list", field="bands")
    if dataset_record is not None:
        require(
            len(bands_raw) == int(dataset_record.get("band_count", len(bands_raw))),
            "E_META",
            "Declared band count does not match dataset index",
            field="bands",
            expected=int(dataset_record.get("band_count", -1)),
            actual=len(bands_raw),
        )
        expected_band_depth = dataset_record.get("band_bit_depth")
        if (
            expected_band_depth is not None
            and all(isinstance(band, dict) and "bit_depth" in band for band in bands_raw)
        ):
            for position, band in enumerate(bands_raw):
                if int(band.get("bit_depth", -1)) != int(expected_band_depth):
                    raise WorkbenchError(
                        "E_VER",
                        "Band bit depth no longer matches the dataset snapshot version declaration",
                        field=f"bands[{position}].bit_depth",
                        details={
                            "expected": int(expected_band_depth),
                            "actual": band.get("bit_depth"),
                        },
                    )
    require(len(bands_raw) >= 3, "E_META", "At least three spectral bands are required", field="bands")
    reference_band = metadata["reference_band"]
    require(
        isinstance(reference_band, int) and 0 <= reference_band < len(bands_raw),
        "E_META",
        "reference_band is out of range",
        field="reference_band",
    )

    band_arrays: list[np.ndarray] = []
    band_indices: list[int] = []
    for position, band in enumerate(bands_raw):
        for field in REQUIRED_BAND_FIELDS:
            _missing_field(band, field, f"bands[{position}]")
        require(
            isinstance(band["index"], int),
            "E_META",
            "Band index must be an integer",
            field=f"bands[{position}].index",
        )
        band_indices.append(int(band["index"]))
        require(
            list(band["shape"]) == [height, width],
            "E_META",
            "Band declaration pixel size does not match scene pixel_size",
            field=f"bands[{position}].shape",
            expected=[height, width],
            actual=band["shape"],
        )
        require(
            int(band["bit_depth"]) in range(1, 17),
            "E_META",
            "Band bit depth must be in [1, 16]",
            field=f"bands[{position}].bit_depth",
        )
        band_file = scene_root / str(band["file"])
        levels = float((1 << int(band["bit_depth"])) - 1)
        array = _load_array(
            band_file,
            field=f"bands[{position}]",
            expected_sha256=str(band["sha256"]),
            expected_dtype=str(band["dtype"]),
            expected_shape=[height, width],
            bit_depth=int(band["bit_depth"]),
            decode_scale=levels,
        )
        band_arrays.append(array)
    require(
        band_indices == list(range(len(band_indices))),
        "E_META",
        "Band indices must be contiguous and zero-based",
        field="bands.index",
        actual=band_indices,
    )

    exposures = metadata["exposures"]
    for field in ("bit_depth", "dtype", "response", "frames"):
        _missing_field(exposures, field, "exposures")
    require(
        isinstance(exposures["bit_depth"], int) and 1 <= exposures["bit_depth"] <= 16,
        "E_META",
        "exposures.bit_depth must be an integer in [1, 16]",
        field="exposures.bit_depth",
    )
    frames_raw = exposures["frames"]
    require(isinstance(frames_raw, list), "E_META", "exposures.frames must be a list", field="exposures.frames")
    require(len(frames_raw) >= 3, "E_META", "At least three exposure frames are required", field="exposures.frames")
    if dataset_record is not None:
        require(
            len(frames_raw) == int(dataset_record.get("exposure_count", len(frames_raw))),
            "E_META",
            "Declared exposure count does not match dataset index",
            field="exposures.frames",
            expected=int(dataset_record.get("exposure_count", -1)),
            actual=len(frames_raw),
        )
    exposure_arrays: list[np.ndarray] = []
    exposure_times: list[float] = []
    if dataset_record is not None and dataset_record.get("exposure_bit_depth") is not None:
        if int(exposures["bit_depth"]) != int(dataset_record["exposure_bit_depth"]):
            raise WorkbenchError(
                "E_VER",
                "Exposure bit depth no longer matches the dataset snapshot version declaration",
                field="exposures.bit_depth",
                details={
                    "expected": int(dataset_record["exposure_bit_depth"]),
                    "actual": exposures["bit_depth"],
                },
            )
    for position, frame in enumerate(frames_raw):
        for field in ("file", "exposure_time", "dtype", "shape", "sha256"):
            _missing_field(frame, field, f"exposures.frames[{position}]")
        exposure_time = float(frame["exposure_time"])
        require(
            np.isfinite(exposure_time) and exposure_time > 0.0,
            "E_ORDER",
            "Exposure times must be finite and positive",
            field=f"exposures.frames[{position}].exposure_time",
            value=exposure_time,
        )
        exposure_times.append(exposure_time)
        require(
            list(frame["shape"]) == [height, width],
            "E_META",
            "Exposure declaration pixel size does not match scene pixel_size",
            field=f"exposures.frames[{position}].shape",
            expected=[height, width],
            actual=frame["shape"],
        )
        levels = float((1 << int(exposures["bit_depth"])) - 1)
        exposure_arrays.append(
            _load_array(
                scene_root / str(frame["file"]),
                field=f"exposures.frames[{position}]",
                expected_sha256=str(frame["sha256"]),
                expected_dtype=str(frame["dtype"]),
                expected_shape=[height, width],
                bit_depth=int(exposures["bit_depth"]),
                decode_scale=levels,
            )
        )
    for position in range(1, len(exposure_times)):
        if exposure_times[position] <= exposure_times[position - 1]:
            raise WorkbenchError(
                "E_ORDER",
                "Exposure times must be strictly increasing in frame order",
                field=f"exposures.frames[{position}].exposure_time",
                details={
                    "previous_index": position - 1,
                    "previous_exposure_time": exposure_times[position - 1],
                    "index": position,
                    "exposure_time": exposure_times[position],
                },
            )

    detail = metadata["detail"]
    for field in ("file", "dtype", "bit_depth", "scale", "shape", "sha256"):
        _missing_field(detail, field, "detail")
    require(
        isinstance(detail["bit_depth"], int) and 1 <= detail["bit_depth"] <= 16,
        "E_META",
        "detail.bit_depth must be an integer in [1, 16]",
        field="detail.bit_depth",
    )
    require(
        np.isfinite(float(detail["scale"])) and float(detail["scale"]) > 0.0,
        "E_META",
        "detail.scale must be finite and positive",
        field="detail.scale",
    )
    require(
        list(detail["shape"]) == [height, width],
        "E_META",
        "Detail layer declaration pixel size does not match scene pixel_size",
        field="detail.shape",
        expected=[height, width],
        actual=detail["shape"],
    )
    detail_layer = _load_array(
        scene_root / str(detail["file"]),
        field="detail",
        expected_sha256=str(detail["sha256"]),
        expected_dtype=str(detail["dtype"]),
        expected_shape=[height, width],
        bit_depth=int(detail["bit_depth"]),
        decode_scale=float(detail["scale"]),
    )

    quality = truth["quality"]
    for field in ("max_residual_std", "min_edge_fidelity", "noise_roi", "edge_roi", "clean_detail_file", "clean_detail_sha256"):
        _missing_field(quality, field, "truth.quality")
    require(
        np.isfinite(float(quality["max_residual_std"])) and float(quality["max_residual_std"]) >= 0.0,
        "E_META",
        "truth.quality.max_residual_std must be finite and non-negative",
        field="truth.quality.max_residual_std",
    )
    require(
        np.isfinite(float(quality["min_edge_fidelity"]))
        and 0.0 <= float(quality["min_edge_fidelity"]) <= 1.0,
        "E_META",
        "truth.quality.min_edge_fidelity must be in [0, 1]",
        field="truth.quality.min_edge_fidelity",
    )
    for roi_name in ("noise_roi", "edge_roi"):
        roi = quality[roi_name]
        require(
            isinstance(roi, list)
            and len(roi) == 4
            and all(isinstance(value, int) for value in roi)
            and 0 <= roi[0] < roi[2] <= height
            and 0 <= roi[1] < roi[3] <= width,
            "E_META",
            f"truth.quality.{roi_name} must be a valid [y0, x0, y1, x1] ROI",
            field=f"truth.quality.{roi_name}",
        )
    clean_detail = _load_array(
        scene_root / str(quality["clean_detail_file"]),
        field="truth.quality.clean_detail_file",
        expected_sha256=str(quality["clean_detail_sha256"]),
        expected_dtype="float32",
        expected_shape=[height, width],
        bit_depth=32,
        decode_scale=1.0,
    )

    offsets_raw = truth["registration_offsets_px"]
    require(
        isinstance(offsets_raw, list) and len(offsets_raw) == len(bands_raw),
        "E_META",
        "Registration offsets must contain one (dy, dx) pair per band",
        field="truth.registration_offsets_px",
        expected=len(bands_raw),
        actual=len(offsets_raw) if isinstance(offsets_raw, list) else None,
    )
    offsets = np.asarray(offsets_raw, dtype=np.float64)
    require(
        offsets.shape == (len(bands_raw), 2) and np.all(np.isfinite(offsets)),
        "E_META",
        "Registration offsets must be finite (dy, dx) pairs",
        field="truth.registration_offsets_px",
    )
    require(
        np.allclose(offsets[reference_band], 0.0),
        "E_META",
        "Reference band registration offset must be [0, 0]",
        field="truth.registration_offsets_px",
    )

    spectral = metadata["spectral"]
    for field in ("wavelengths_nm", "cmf_xyz", "band_responses", "xyz_weights", "source_white_xyz"):
        _missing_field(spectral, field, "spectral")
    weights = np.asarray(spectral["xyz_weights"], dtype=np.float64)
    require(
        weights.shape == (len(bands_raw), 3) and np.all(np.isfinite(weights)),
        "E_META",
        "Spectral XYZ weights are incompatible with the declared bands",
        field="spectral.xyz_weights",
    )
    source_white = np.asarray(spectral["source_white_xyz"], dtype=np.float64)
    require(
        source_white.shape == (3,) and np.all(np.isfinite(source_white)) and np.all(source_white > 0.0),
        "E_META",
        "spectral.source_white_xyz must be a finite positive XYZ triple",
        field="spectral.source_white_xyz",
    )

    target = metadata["target"]
    for field in ("name", "white_xyz", "xyz_to_linear_rgb", "bit_depth", "gamut_intent"):
        _missing_field(target, field, "target")
    target_white = np.asarray(target.get("white_xyz", SRGB_WHITE_XYZ), dtype=np.float64)
    target_matrix = np.asarray(target.get("xyz_to_linear_rgb", SRGB_XYZ_TO_LINEAR_RGB), dtype=np.float64)
    require(
        target_white.shape == (3,) and np.all(np.isfinite(target_white)) and np.all(target_white > 0.0),
        "E_META",
        "target.white_xyz must be a finite positive XYZ triple",
        field="target.white_xyz",
    )
    require(
        target_matrix.shape == (3, 3) and np.all(np.isfinite(target_matrix)),
        "E_META",
        "target.xyz_to_linear_rgb must be a finite 3x3 matrix",
        field="target.xyz_to_linear_rgb",
    )
    require(
        isinstance(target["bit_depth"], int) and 1 <= target["bit_depth"] <= 16,
        "E_META",
        "target.bit_depth must be an integer in [1, 16]",
        field="target.bit_depth",
    )
    if dataset_record is not None and dataset_record.get("target_bit_depth") is not None:
        if int(target["bit_depth"]) != int(dataset_record["target_bit_depth"]):
            raise WorkbenchError(
                "E_VER",
                "Target output bit depth no longer matches the dataset snapshot version declaration",
                field="target.bit_depth",
                details={
                    "expected": int(dataset_record["target_bit_depth"]),
                    "actual": target["bit_depth"],
                },
            )

    patches = truth["patches"]
    require(
        isinstance(patches, list) and len(patches) >= 1,
        "E_META",
        "truth.patches must contain at least one known color patch",
        field="truth.patches",
    )
    for position, patch in enumerate(patches):
        for field in ("id", "bbox", "expected_linear_rgb"):
            _missing_field(patch, field, f"truth.patches[{position}]")
        bbox = patch["bbox"]
        require(
            isinstance(bbox, list)
            and len(bbox) == 4
            and 0 <= bbox[0] < bbox[2] <= height
            and 0 <= bbox[1] < bbox[3] <= width,
            "E_META",
            "Patch bbox must be a valid [y0, x0, y1, x1]",
            field=f"truth.patches[{position}].bbox",
        )
        rgb = np.asarray(patch["expected_linear_rgb"], dtype=np.float64)
        require(
            rgb.shape == (3,) and np.all(np.isfinite(rgb)),
            "E_META",
            "Patch expected_linear_rgb must be a finite RGB triple",
            field=f"truth.patches[{position}].expected_linear_rgb",
        )

    return LoadedScene(
        scene_root=scene_root,
        metadata=metadata,
        truth=truth,
        bands=np.stack(band_arrays, axis=0),
        exposure_frames=np.stack(exposure_arrays, axis=0),
        detail_layer=detail_layer,
        clean_detail=clean_detail,
        registration_offsets=offsets,
        xyz_weights=weights,
        source_white_xyz=source_white,
        target_white_xyz=target_white,
        target_matrix=target_matrix,
        metadata_version=str(declared_metadata_version),
    )

