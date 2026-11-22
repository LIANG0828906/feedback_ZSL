"""Deterministic local sample scene generator."""

from __future__ import annotations

from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image
from scipy import ndimage

from . import ALGORITHM_VERSION, DATASET_SCHEMA_VERSION
from .color import (
    SRGB_WHITE_XYZ,
    SRGB_XYZ_TO_LINEAR_RGB,
    bands_to_xyz,
    build_spectral_basis,
    linear_rgb_to_preview_rgb8,
    quantize_linear_rgb,
    xyz_to_linear_rgb,
)
from .errors import WorkbenchError
from .io_utils import atomic_save_npy, ensure_directory, sha256_file, value_version, write_json


SCENE_HEIGHT = 96
SCENE_WIDTH = 128
BAND_CENTERS_NM = (460.0, 530.0, 610.0, 680.0)
BAND_FWHMS_NM = (38.0, 42.0, 46.0, 50.0)
EXPOSURE_TIMES = (1.0 / 125.0, 1.0 / 15.0, 1.0 / 2.0)
DETAIL_LEVELS = 65535


@dataclass(frozen=True)
class SceneRecipe:
    scene_id: str
    seed: int
    offsets_px: tuple[tuple[float, float], ...]
    response_gamma: float
    output_bit_depth: int


RECIPES = (
    SceneRecipe(
        scene_id="scene_00_neon_dusk",
        seed=2026112301,
        offsets_px=((0.0, 0.0), (0.75, -1.25), (-1.50, 0.50), (1.25, 1.75)),
        response_gamma=2.2,
        output_bit_depth=10,
    ),
    SceneRecipe(
        scene_id="scene_01_forest_studio",
        seed=2026112302,
        offsets_px=((0.0, 0.0), (-0.50, 1.00), (1.25, 0.75), (0.25, -1.50)),
        response_gamma=2.0,
        output_bit_depth=12,
    ),
    SceneRecipe(
        scene_id="scene_02_spectral_lab",
        seed=2026112303,
        offsets_px=((0.0, 0.0), (1.50, 1.25), (-0.75, -1.00), (-1.25, 2.00)),
        response_gamma=2.4,
        output_bit_depth=10,
    ),
)


def _quantize_unit(values: np.ndarray, bit_depth: int, dtype: np.dtype[Any] = np.uint16) -> np.ndarray:
    levels = (1 << bit_depth) - 1
    return np.rint(np.clip(values, 0.0, 1.0) * levels).astype(dtype)


def _base_band_scene(recipe: SceneRecipe) -> np.ndarray:
    rng = np.random.default_rng(recipe.seed)
    yy, xx = np.mgrid[0:SCENE_HEIGHT, 0:SCENE_WIDTH].astype(np.float64)
    y = yy / max(SCENE_HEIGHT - 1, 1)
    x = xx / max(SCENE_WIDTH - 1, 1)
    radial = np.exp(-1.8 * ((x - 0.48) ** 2 + (y - 0.46) ** 2))
    stripe_phase = 2.0 * np.pi * (x * 3.0 + y * 1.5)
    bands: list[np.ndarray] = []

    for band_index in range(len(BAND_CENTERS_NM)):
        phase = 0.37 * band_index
        low_frequency = (
            0.075
            + 0.18 * np.sin(2.0 * np.pi * (x * (0.75 + 0.08 * band_index) + phase))
            + 0.12 * np.cos(2.0 * np.pi * (y * (0.62 + 0.05 * band_index) - 0.25 * phase))
        )
        texture = 0.012 * np.sin(stripe_phase + phase) * radial
        emissive = np.zeros_like(x)
        for disc_index, (cy, cx, radius, strength) in enumerate(
            (
                (0.30, 0.20, 0.13, 0.40),
                (0.40, 0.72, 0.17, 0.31),
                (0.74, 0.36, 0.11, 0.37),
            )
        ):
            distance = np.sqrt((x - cx) ** 2 + (y - cy) ** 2)
            emissive += strength * np.exp(-((distance / (radius * (1.0 + 0.08 * band_index))) ** 2))
            if (disc_index + band_index) % 2 == 0:
                emissive += 0.10 * np.exp(-(((x - cx) ** 2 + (y - cy) ** 2) / (radius * radius * 0.08)))
        bars = ((xx > 8 + band_index * 3) & (xx < 23 + band_index * 3) & (yy > 62) & (yy < 87)).astype(float)
        value = 0.055 + 0.46 * radial + low_frequency + texture + emissive + 0.12 * bars
        noise = rng.normal(0.0, 0.0015, size=value.shape)
        bands.append(np.clip(value + noise, 0.02, 0.94))

    result = np.stack(bands, axis=0)
    return _place_patch_chart(result)


def _patch_specs() -> list[dict[str, Any]]:
    return [
        {"id": "neutral", "bbox": (12, 12, 26, 28), "radiance": (0.45, 0.45, 0.45, 0.45)},
        {"id": "warm_red", "bbox": (12, 50, 26, 66), "radiance": (0.05, 0.13, 0.66, 0.58)},
        {"id": "leaf_green", "bbox": (12, 91, 26, 107), "radiance": (0.06, 0.63, 0.18, 0.10)},
        {"id": "cyan_blue", "bbox": (38, 12, 52, 28), "radiance": (0.58, 0.40, 0.05, 0.04)},
        {"id": "violet", "bbox": (38, 50, 52, 66), "radiance": (0.46, 0.08, 0.22, 0.30)},
        {"id": "amber", "bbox": (38, 91, 52, 107), "radiance": (0.03, 0.31, 0.72, 0.56)},
        {"id": "deep_blue", "bbox": (66, 12, 80, 28), "radiance": (0.79, 0.11, 0.02, 0.01)},
        {"id": "near_white", "bbox": (66, 50, 80, 66), "radiance": (0.73, 0.73, 0.73, 0.73)},
        {"id": "dark_neutral", "bbox": (66, 91, 80, 107), "radiance": (0.10, 0.10, 0.10, 0.10)},
    ]


def _place_patch_chart(bands: np.ndarray) -> np.ndarray:
    output = bands.copy()
    for patch in _patch_specs():
        y0, x0, y1, x1 = patch["bbox"]
        output[:, y0:y1, x0:x1] = np.asarray(patch["radiance"], dtype=np.float64)[:, None, None]
    # The chart surround provides a stable local boundary for visual validation.
    output[:, 8:84, 8:111] *= 0.78
    for patch in _patch_specs():
        y0, x0, y1, x1 = patch["bbox"]
        output[:, y0:y1, x0:x1] = np.asarray(patch["radiance"], dtype=np.float64)[:, None, None]
    return output


def _shift_band(bands: np.ndarray, offsets_px: tuple[tuple[float, float], ...]) -> np.ndarray:
    shifted = np.empty_like(bands)
    for index, (dy, dx) in enumerate(offsets_px):
        shifted[index] = ndimage.shift(
            bands[index],
            shift=(dy, dx),
            order=3,
            mode="reflect",
            prefilter=True,
        )
    return shifted


def _copy_neighbor_pixels(array: np.ndarray, rows: np.ndarray, cols: np.ndarray) -> None:
    for row, col in zip(rows, cols, strict=True):
        if 1 <= row < array.shape[0] - 1 and 1 <= col < array.shape[1] - 1:
            array[row, col] = 0.5 * (array[row - 1, col] + array[row + 1, col])


def _exposure_stack(recipe: SceneRecipe, luminance: np.ndarray) -> tuple[np.ndarray, dict[str, Any]]:
    rng = np.random.default_rng(recipe.seed + 991)
    black_level = 64
    saturation = (1 << 12) - 1
    response_scale = 1.40
    frames = np.empty((len(EXPOSURE_TIMES),) + luminance.shape, dtype=np.uint16)
    for index, exposure_time in enumerate(EXPOSURE_TIMES):
        linear_signal = np.clip(luminance * exposure_time / response_scale, 0.0, 1.0)
        encoded = np.power(linear_signal, 1.0 / recipe.response_gamma)
        raw = black_level + encoded * (saturation - black_level)
        shot_sigma = 2.0 + 5.0 * np.sqrt(np.maximum(encoded, 0.0)) + index
        raw += rng.normal(0.0, shot_sigma, size=raw.shape)
        raw = np.rint(np.clip(raw, black_level, saturation))
        frames[index] = raw.astype(np.uint16)
    response = {
        "model": "gamma",
        "gamma": float(recipe.response_gamma),
        "black_level": black_level,
        "saturation": saturation,
        "full_well_exposure_radiance": response_scale,
    }
    return frames, response


def _detail_layers(recipe: SceneRecipe) -> tuple[np.ndarray, np.ndarray]:
    rng = np.random.default_rng(recipe.seed + 1777)
    yy, xx = np.mgrid[0:SCENE_HEIGHT, 0:SCENE_WIDTH].astype(np.float64)
    clean = (
        0.5
        + 0.020 * np.sin(2.0 * np.pi * xx / 15.0)
        + 0.017 * np.cos(2.0 * np.pi * yy / 19.0)
        + 0.012 * np.sin(2.0 * np.pi * (xx + yy) / 27.0)
    )
    clean[26:35, 34:57] += 0.035
    clean[52:60, 68:91] -= 0.032
    clean[79:86, 90:116] += 0.027
    for patch in _patch_specs():
        y0, x0, y1, x1 = patch["bbox"]
        clean[y0:y1, x0:x1] = 0.5
    noisy = clean + rng.normal(0.0, 0.035, size=clean.shape)
    noisy = np.clip(noisy, 0.0, 1.0)
    return clean.astype(np.float32), noisy.astype(np.float32)


def _patch_truth(
    clean_band_scene: np.ndarray,
    xyz_weights: np.ndarray,
    source_white_xyz: np.ndarray,
) -> list[dict[str, Any]]:
    filters: list[dict[str, Any]] = []
    for patch in _patch_specs():
        y0, x0, y1, x1 = patch["bbox"]
        mean_bands = np.mean(clean_band_scene[:, y0:y1, x0:x1], axis=(1, 2))
        xyz = bands_to_xyz(mean_bands[:, None, None], xyz_weights)[0, 0]
        expected_rgb = xyz_to_linear_rgb(
            xyz,
            source_white_xyz=source_white_xyz,
            target_white_xyz=SRGB_WHITE_XYZ,
            target_matrix=SRGB_XYZ_TO_LINEAR_RGB,
        )
        filters.append(
            {
                "id": patch["id"],
                "bbox": list(patch["bbox"]),
                "band_radiance": [float(value) for value in mean_bands],
                "expected_xyz": [float(value) for value in xyz],
                "expected_linear_rgb": [float(value) for value in expected_rgb],
                "expected_in_gamut": bool(np.all(expected_rgb >= 0.0) and np.all(expected_rgb <= 1.0)),
            }
        )
    return filters


def _write_preview(path: Path, bands: np.ndarray, xyz_weights: np.ndarray, source_white_xyz: np.ndarray) -> None:
    xyz = bands_to_xyz(bands, xyz_weights)
    rgb = xyz_to_linear_rgb(xyz, source_white_xyz)
    preview = linear_rgb_to_preview_rgb8(rgb)
    Image.fromarray(preview, mode="RGB").save(path)


def generate_dataset(
    output_dir: Path,
    *,
    seed: int = 20261123,
    scene_count: int = 3,
    overwrite: bool = True,
) -> Path:
    """Generate all local samples, metadata, and truth files."""

    if scene_count < 1 or scene_count > len(RECIPES):
        raise WorkbenchError(
            "E_PARA",
            f"scene_count must be in [1, {len(RECIPES)}]",
            field="scene_count",
            value=scene_count,
        )
    root = ensure_directory(output_dir)
    dataset_path = root / "dataset.json"
    if dataset_path.exists() and not overwrite:
        raise FileExistsError(f"Dataset already exists: {dataset_path}")

    scene_records: list[dict[str, Any]] = []
    for recipe_index, base_recipe in enumerate(RECIPES[:scene_count]):
        recipe = replace(base_recipe, seed=int(seed) * 100 + recipe_index + 1)
        scene_root = ensure_directory(root / recipe.scene_id)
        for subdir in ("bands", "exposures", "detail", "truth", "preview"):
            ensure_directory(scene_root / subdir)

        clean_band_scene = _base_band_scene(recipe)
        shifted_band_scene = _shift_band(clean_band_scene, recipe.offsets_px)
        band_arrays: dict[int, np.ndarray] = {}
        band_records: list[dict[str, Any]] = []
        for band_index, center_nm in enumerate(BAND_CENTERS_NM):
            relative_path = Path("bands") / f"band_{band_index:02d}.npy"
            band_data = _quantize_unit(shifted_band_scene[band_index], 12)
            atomic_save_npy(scene_root / relative_path, band_data)
            band_arrays[band_index] = band_data
            band_records.append(
                {
                    "index": band_index,
                    "name": f"band_{center_nm:.0f}nm",
                    "center_nm": float(center_nm),
                    "fwhm_nm": float(BAND_FWHMS_NM[band_index]),
                    "file": relative_path.as_posix(),
                    "dtype": "uint16",
                    "bit_depth": 12,
                    "shape": [SCENE_HEIGHT, SCENE_WIDTH],
                    "sha256": sha256_file(scene_root / relative_path),
                }
            )

        luminance = np.mean(clean_band_scene, axis=0)
        exposure_frames, response = _exposure_stack(recipe, luminance)
        exposure_records: list[dict[str, Any]] = []
        for frame_index, exposure_time in enumerate(EXPOSURE_TIMES):
            relative_path = Path("exposures") / f"exposure_{frame_index:02d}.npy"
            atomic_save_npy(scene_root / relative_path, exposure_frames[frame_index])
            exposure_records.append(
                {
                    "index": frame_index,
                    "file": relative_path.as_posix(),
                    "exposure_time": float(exposure_time),
                    "dtype": "uint16",
                    "shape": [SCENE_HEIGHT, SCENE_WIDTH],
                    "sha256": sha256_file(scene_root / relative_path),
                }
            )

        clean_detail, noisy_detail = _detail_layers(recipe)
        clean_detail_path = scene_root / "truth" / "detail_clean.npy"
        atomic_save_npy(clean_detail_path, clean_detail)
        noisy_detail_path = scene_root / "detail" / "detail_noisy.npy"
        atomic_save_npy(noisy_detail_path, _quantize_unit(noisy_detail, 16))

        spectral = build_spectral_basis(BAND_CENTERS_NM, BAND_FWHMS_NM)
        xyz_weights = np.asarray(spectral["xyz_weights"], dtype=np.float64)
        source_white_xyz = np.asarray(spectral["source_white_xyz"], dtype=np.float64)
        _write_preview(scene_root / "preview" / "spectral_preview.png", clean_band_scene, xyz_weights, source_white_xyz)

        patches = _patch_truth(clean_band_scene, xyz_weights, source_white_xyz)
        metadata: dict[str, Any] = {
            "schema_version": DATASET_SCHEMA_VERSION,
            "scene_id": recipe.scene_id,
            "seed": int(recipe.seed),
            "pixel_size": [SCENE_HEIGHT, SCENE_WIDTH],
            "reference_band": 0,
            "max_registration_shift_px": 6.0,
            "max_interpolation_order": 5,
            "bands": band_records,
            "exposures": {
                "bit_depth": 12,
                "dtype": "uint16",
                "response": response,
                "frames": exposure_records,
            },
            "detail": {
                "file": noisy_detail_path.relative_to(scene_root).as_posix(),
                "dtype": "uint16",
                "bit_depth": 16,
                "scale": DETAIL_LEVELS,
                "shape": [SCENE_HEIGHT, SCENE_WIDTH],
                "sha256": sha256_file(noisy_detail_path),
                "neutral_value": 0.5,
            },
            "spectral": spectral,
            "target": {
                "name": "sRGB",
                "white_xyz": SRGB_WHITE_XYZ.tolist(),
                "xyz_to_linear_rgb": SRGB_XYZ_TO_LINEAR_RGB.tolist(),
                "bit_depth": int(recipe.output_bit_depth),
                "gamut_intent": "perceptual",
            },
        }
        metadata["metadata_version"] = value_version(metadata)
        write_json(scene_root / "metadata.json", metadata)

        noise_roi = [82, 33, 92, 55]
        edge_roi = [22, 36, 48, 62]
        truth = {
            "schema_version": DATASET_SCHEMA_VERSION,
            "scene_id": recipe.scene_id,
            "metadata_version": metadata["metadata_version"],
            "registration_offsets_px": [[float(v) for v in pair] for pair in recipe.offsets_px],
            "patches": patches,
            "quality": {
                "max_residual_std": 0.012,
                "min_edge_fidelity": 0.82,
                "noise_roi": noise_roi,
                "edge_roi": edge_roi,
                "clean_detail_file": clean_detail_path.relative_to(scene_root).as_posix(),
                "clean_detail_sha256": sha256_file(clean_detail_path),
            },
        }
        write_json(scene_root / "truth.json", truth)
        scene_records.append(
            {
                "scene_id": recipe.scene_id,
                "directory": recipe.scene_id,
                "metadata_version": metadata["metadata_version"],
                "band_count": len(band_records),
                "exposure_count": len(exposure_records),
                "pixel_size": [SCENE_HEIGHT, SCENE_WIDTH],
                "band_bit_depth": 12,
                "exposure_bit_depth": 12,
                "target_bit_depth": recipe.output_bit_depth,
            }
        )
    dataset = {
        "schema_version": DATASET_SCHEMA_VERSION,
        "algorithm_version": ALGORITHM_VERSION,
        "seed": int(seed),
        "scenes": scene_records,
    }
    write_json(dataset_path, dataset)
    return dataset_path


def verify_generated_dataset(dataset_path: Path) -> dict[str, Any]:
    from .io_utils import read_json

    dataset_path = Path(dataset_path)
    dataset = read_json(dataset_path, field="dataset")
    summary: dict[str, Any] = {"dataset": str(dataset_path), "scenes": []}
    for record in dataset.get("scenes", []):
        scene_dir = dataset_path.parent / record["directory"]
        metadata = read_json(scene_dir / "metadata.json", field="metadata")
        checks = {
            "band_count": len(metadata["bands"]),
            "exposure_count": len(metadata["exposures"]["frames"]),
            "pixel_size": metadata["pixel_size"],
            "band_bit_depths": [band["bit_depth"] for band in metadata["bands"]],
            "target_bit_depth": metadata["target"]["bit_depth"],
        }
        if checks["band_count"] < 3:
            raise AssertionError(f"{record['scene_id']}: expected at least 3 bands")
        if checks["exposure_count"] < 3:
            raise AssertionError(f"{record['scene_id']}: expected at least 3 exposures")
        for band in metadata["bands"]:
            array = np.load(scene_dir / band["file"], allow_pickle=False)
            if list(array.shape) != band["shape"] or array.dtype != np.uint16:
                raise AssertionError(f"{record['scene_id']}: band {band['index']} declaration mismatch")
            if int(array.max()) > (1 << band["bit_depth"]) - 1:
                raise AssertionError(f"{record['scene_id']}: band {band['index']} exceeds bit depth")
        summary["scenes"].append({"scene_id": record["scene_id"], **checks})
    return summary

