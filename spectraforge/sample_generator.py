"""Deterministic local multispectral/HDR sample generation.

No network, external assets, or random state outside the declared seed is used.
"""

from __future__ import annotations

import json
import math
import shutil
from pathlib import Path
from typing import Any

import numpy as np

from .color import (
    CIE_1931_2DEG_400_700_10,
    D50_XYZ,
    WAVELENGTHS_400_700_10,
    compute_xyz_basis,
    source_xyz_to_target_rgb,
    target_white_xyz,
)
from .errors import WorkbenchError
from .imageio import write_raster
from .parameters import default_parameters
from .registration import warp_shift
from .util import hash_object, relpath_posix, atomic_write_json
from .versions import METADATA_SCHEMA_VERSION

SCENE_TITLES = [
    ("neon_forge", "Neon Forge"),
    ("spectral_garden", "Spectral Garden"),
    ("chrome_orbit", "Chrome Orbit"),
    ("lumen_archive", "Lumen Archive"),
]

PATCH_PROFILES = [
    ("calibration_white", [1.00, 1.00, 0.98, 0.97, 0.98, 1.00, 1.00, 0.98], 2.0),
    ("amber_emitter", [0.12, 0.16, 0.28, 0.56, 0.95, 1.00, 0.42, 0.10], 2.5),
    ("cyan_crystal", [0.62, 1.00, 0.92, 0.56, 0.25, 0.11, 0.06, 0.03], 2.2),
    ("cobalt_signal", [1.00, 0.66, 0.28, 0.12, 0.08, 0.06, 0.05, 0.04], 2.1),
    ("magenta_gel", [0.62, 0.29, 0.13, 0.18, 0.42, 0.72, 0.96, 0.50], 2.0),
    ("neutral_18", [0.34, 0.34, 0.34, 0.34, 0.34, 0.34, 0.34, 0.34], 1.2),
    ("olive_alloy", [0.13, 0.20, 0.33, 0.48, 0.55, 0.35, 0.16, 0.07], 2.3),
    ("rose_plasma", [0.46, 0.22, 0.18, 0.30, 0.54, 0.70, 0.82, 0.44], 2.4),
]


def _response_matrix(band_count: int) -> tuple[np.ndarray, np.ndarray]:
    centers = np.linspace(425.0, 700.0, band_count)
    sigma = 20.0 if band_count >= 8 else 24.0
    distances = WAVELENGTHS_400_700_10[:, np.newaxis] - centers[np.newaxis, :]
    responses = np.exp(-0.5 * (distances / sigma) ** 2)
    responses /= np.maximum(responses.sum(axis=1, keepdims=True), 1e-12)
    return centers, responses


def _smooth_background(seed: int, band_count: int, width: int, height: int) -> np.ndarray:
    rng = np.random.default_rng(seed)
    yy, xx = np.mgrid[0:height, 0:width]
    x = xx.astype(np.float64) / max(width - 1, 1)
    y = yy.astype(np.float64) / max(height - 1, 1)
    coefficients = np.empty((band_count, height, width), dtype=np.float64)
    for band in range(band_count):
        phase = rng.uniform(0.0, 2.0 * math.pi)
        phase2 = rng.uniform(0.0, 2.0 * math.pi)
        base = rng.uniform(0.025, 0.085)
        waves = (
            np.sin(2.0 * math.pi * (1.2 * x + 0.27 * y) + phase)
            + 0.62 * np.cos(2.0 * math.pi * (0.42 * x - 1.35 * y) + phase2)
            + 0.35 * np.sin(2.0 * math.pi * (2.1 * x + 1.7 * y) + phase * 0.5)
        )
        grad = 0.35 + 0.65 * np.clip(0.75 * x + 0.25 * (1.0 - y), 0.0, 1.0)
        coefficients[band] = np.maximum(base + 0.075 * grad + 0.030 * waves, 0.008)
    radius = np.sqrt((x - 0.72) ** 2 + (y - 0.30) ** 2)
    glow = np.exp(-(radius / 0.17) ** 2)
    coefficients *= 0.55 + 1.45 * glow[np.newaxis, :, :]
    bar = ((x > 0.60) & (x < 0.66) & (y > 0.58) & (y < 0.92)).astype(np.float64)
    coefficients += bar[np.newaxis, :, :] * np.linspace(0.02, 0.14, band_count)[:, np.newaxis, np.newaxis]
    return coefficients


def _patch_rects(width: int, height: int) -> list[tuple[int, int, int, int]]:
    patch_w = max(16, width // 6)
    patch_h = max(11, height // 6)
    gap_x = max(6, width // 18)
    gap_y = max(8, height // 8)
    left = max(6, (width - (4 * patch_w + 3 * gap_x)) // 2)
    top = max(6, (height - (2 * patch_h + gap_y)) // 2)
    rects = []
    for row in range(2):
        for column in range(4):
            x0 = left + column * (patch_w + gap_x)
            y0 = top + row * (patch_h + gap_y)
            rects.append((x0, y0, x0 + patch_w, y0 + patch_h))
    return rects


def _build_scene(
    seed: int,
    width: int,
    height: int,
    band_count: int,
    exposure_count: int,
) -> dict[str, Any]:
    centers, responses = _response_matrix(band_count)
    xyz_basis = compute_xyz_basis(responses)
    coefficients = _smooth_background(seed, band_count, width, height)
    rects = _patch_rects(width, height)
    patches = []
    for index, (name, profile, amplitude) in enumerate(PATCH_PROFILES[: len(rects)]):
        if len(profile) != band_count:
            source = np.interp(
                np.linspace(0.0, 1.0, band_count),
                np.linspace(0.0, 1.0, len(profile)),
                profile,
            )
        else:
            source = np.asarray(profile, dtype=np.float64)
        patch_coeff = source * amplitude
        x0, y0, x1, y1 = rects[index]
        coefficients[:, y0:y1, x0:x1] = patch_coeff[:, np.newaxis, np.newaxis]
        patches.append(
            {
                "id": name,
                "x0": x0,
                "y0": y0,
                "x1": x1,
                "y1": y1,
                "spectral_coefficients": patch_coeff.tolist(),
            }
        )
    coefficients *= 0.005
    for patch in patches:
        patch["spectral_coefficients"] = (
            np.asarray(patch["spectral_coefficients"], dtype=np.float64) * 0.005
        ).tolist()
    design = responses.T * 10.0
    band_matrix = design @ responses
    band_truth = np.einsum("ij,jhw->ihw", band_matrix, coefficients)
    spectrum = np.einsum("lb,bhw->hw", responses, coefficients)
    xyz = np.einsum("hw,lc->hwc", spectrum, CIE_1931_2DEG_400_700_10) * 10.0
    luminance = np.maximum(xyz[..., 1], 0.0)
    rng = np.random.default_rng(seed + 7919)
    registration = np.zeros((band_count, 2), dtype=np.float64)
    for band in range(1, band_count):
        registration[band, 0] = rng.uniform(-1.25, 1.25)
        registration[band, 1] = rng.uniform(-1.25, 1.25)
    shifted_bands = np.stack(
        [
            warp_shift(band_truth[band], registration[band, 0], registration[band, 1], "bilinear")
            for band in range(band_count)
        ],
        axis=0,
    )
    positive_lum = luminance[luminance > 0]
    min_luminance = float(np.percentile(positive_lum, 2.0)) if positive_lum.size else 0.01
    max_luminance = float(np.max(luminance))
    shortest = 0.32 / max(max_luminance, 1e-6)
    exposure_times = shortest * np.power(4.0, np.arange(exposure_count, dtype=np.float64))
    if float(exposure_times[-1]) < 0.35 / max(min_luminance, 1e-6):
        exposure_times *= (0.35 / max(min_luminance, 1e-6)) / float(exposure_times[-1])
    gamma = 2.2
    exposure_stack = []
    for exposure_time in exposure_times:
        sensor = np.clip(luminance * exposure_time, 0.0, 1.0)
        response = np.power(sensor, 1.0 / gamma)
        for bit_depth in (12,):
            max_code = (1 << bit_depth) - 1
            exposure_stack.append(np.rint(response * max_code).astype(np.uint16))
    truth_patches = []
    for patch in patches:
        coeff = np.asarray(patch["spectral_coefficients"])
        patch_spectrum = responses @ coeff
        patch_xyz = CIE_1931_2DEG_400_700_10.T @ patch_spectrum * 10.0
        patch_rgb = source_xyz_to_target_rgb(patch_xyz, D50_XYZ, "sRGB")
        truth_patches.append(
            {
                **{key: patch[key] for key in ("id", "x0", "y0", "x1", "y1")},
                "expected_xyz_d50": patch_xyz.tolist(),
                "expected_linear_rgb_srgb": patch_rgb.tolist(),
                "expected_luminance": float(patch_xyz[1]),
            }
        )
    return {
        "seed": seed,
        "centers": centers,
        "responses": responses,
        "xyz_basis": xyz_basis,
        "band_truth": band_truth,
        "shifted_bands": shifted_bands,
        "exposure_stack": exposure_stack,
        "exposure_times": exposure_times,
        "luminance": luminance,
        "registration": registration,
        "patches": truth_patches,
        "gamma": gamma,
    }


def generate_scene(
    root: Path,
    *,
    seed: int,
    scene_id: str,
    title: str,
    width: int = 128,
    height: int = 96,
    band_count: int = 8,
    exposure_count: int = 6,
) -> dict[str, Any]:
    root.mkdir(parents=True, exist_ok=True)
    (root / "bands").mkdir(exist_ok=True)
    (root / "exposures").mkdir(exist_ok=True)
    built = _build_scene(seed, width, height, band_count, exposure_count)
    band_entries = []
    for index in range(band_count):
        values = built["shifted_bands"][index]
        max_value = max(float(values.max()), 1e-6)
        radiometric_scale = max_value * 1.04
        bit_depth = 16
        max_code = (1 << bit_depth) - 1
        raw = np.rint(np.clip(values / radiometric_scale, 0.0, 1.0) * max_code).astype(np.uint16)
        relative = f"bands/band_{index:02d}.msi"
        payload_hash = write_raster(
            root / relative,
            raw,
            bit_depth=bit_depth,
            role="band",
            index=index,
        )
        band_entries.append(
            {
                "index": index,
                "name": f"B{int(round(built['centers'][index]))}",
                "file": relative,
                "center_nm": float(built["centers"][index]),
                "sigma_nm": 20.0 if band_count >= 8 else 24.0,
                "bit_depth": bit_depth,
                "radiometric_scale": radiometric_scale,
                "sha256": payload_hash,
            }
        )
    exposure_entries = []
    for index in range(exposure_count):
        relative = f"exposures/exposure_{index:02d}.msi"
        payload_hash = write_raster(
            root / relative,
            built["exposure_stack"][index],
            bit_depth=12,
            role="exposure",
            index=index,
        )
        exposure_entries.append(
            {
                "index": index,
                "name": f"EXP {index + 1}",
                "file": relative,
                "exposure_time": float(built["exposure_times"][index]),
                "bit_depth": 12,
                "sha256": payload_hash,
            }
        )
    default_params = default_parameters(exposure_count)
    default_params.update(
        {
            "target_gamut": "sRGB",
            "gamut_intent": "compress",
            "tone_curve": "reinhard",
            "tone_key": 0.68,
            "tone_white": 8.0,
        }
    )
    metadata = {
        "schema_version": METADATA_SCHEMA_VERSION,
        "metadata_version": "meta-1.0",
        "scene_id": scene_id,
        "title": title,
        "seed": int(seed),
        "width": int(width),
        "height": int(height),
        "pixel_pitch_um": 3.76,
        "band_count": int(band_count),
        "exposure_count": int(exposure_count),
        "bands": band_entries,
        "exposures": exposure_entries,
        "wave_grid_nm": WAVELENGTHS_400_700_10.tolist(),
        "responses": np.round(built["responses"], 9).tolist(),
        "xyz_basis": built["xyz_basis"].tolist(),
        "scene_white_xyz": D50_XYZ.tolist(),
        "target_white_xyz": target_white_xyz("sRGB").tolist(),
        "camera_response": {"gamma": float(built["gamma"]), "model": "power-law inverse response"},
        "registration_offsets": np.round(built["registration"], 6).tolist(),
        "truth_file": "truth.json",
        "default_parameters": default_params,
    }
    metadata_hash = hash_object(metadata)
    metadata["metadata_hash"] = metadata_hash
    atomic_write_json(root / "scene.json", metadata)
    peak_index = np.unravel_index(int(np.argmax(built["luminance"])), built["luminance"].shape)
    truth = {
        "schema_version": "1.0",
        "scene_id": scene_id,
        "seed": int(seed),
        "width": int(width),
        "height": int(height),
        "target_gamut": "sRGB",
        "target_white_xyz": target_white_xyz("sRGB").tolist(),
        "source_white_xyz": D50_XYZ.tolist(),
        "tolerance": 0.045,
        "peak_radiance": float(np.max(built["luminance"])),
        "peak_location": [int(peak_index[1]), int(peak_index[0])],
        "registration_offsets": np.round(built["registration"], 6).tolist(),
        "exposure_times": built["exposure_times"].tolist(),
        "patches": built["patches"],
        "notes": (
            "Expected RGB is analytical absolute linear sRGB from the D50 scene white and "
            "the same CIE quadrature grid; no tolerance is used for three-path equality."
        ),
    }
    atomic_write_json(root / "truth.json", truth)
    return {
        "scene_id": scene_id,
        "title": title,
        "path": root.as_posix(),
        "seed": int(seed),
        "width": int(width),
        "height": int(height),
        "bands": int(band_count),
        "exposures": int(exposure_count),
        "peak_radiance": truth["peak_radiance"],
        "patch_count": len(truth["patches"]),
        "metadata_hash": metadata_hash,
    }


def _copy_without_metadata(source: Path, target: Path) -> None:
    if target.exists():
        shutil.rmtree(target)
    shutil.copytree(source, target)
    (target / "scene.json").unlink(missing_ok=True)


def _make_dim_fault(source: Path, target: Path) -> None:
    if target.exists():
        shutil.rmtree(target)
    shutil.copytree(source, target)
    scene_path = target / "scene.json"
    with scene_path.open("r", encoding="utf-8") as handle:
        metadata = json.load(handle)
    metadata.pop("metadata_hash", None)
    metadata["width"] = int(metadata["width"]) + 2
    metadata["metadata_hash"] = hash_object(metadata)
    atomic_write_json(scene_path, metadata)


def _make_decode_fault(source: Path, target: Path) -> None:
    if target.exists():
        shutil.rmtree(target)
    shutil.copytree(source, target)
    band_path = sorted((target / "bands").glob("*.msi"))[0]
    blob = bytearray(band_path.read_bytes())
    blob[-7] ^= 0xFF
    band_path.write_bytes(blob)


def generate_all(
    root: str | Path,
    *,
    seed: int = 424242,
    scene_count: int = 3,
    width: int = 128,
    height: int = 96,
    band_count: int = 8,
    exposure_count: int = 6,
    with_faults: bool = True,
) -> dict[str, Any]:
    base = Path(root)
    scenes_dir = base / "scenes"
    scenes_dir.mkdir(parents=True, exist_ok=True)
    entries = []
    for index in range(scene_count):
        title_slug, title = SCENE_TITLES[index % len(SCENE_TITLES)]
        scene_seed = seed + index * 101
        scene_id = f"{title_slug}_{scene_seed}"
        entry = generate_scene(
            scenes_dir / scene_id,
            seed=scene_seed,
            scene_id=scene_id,
            title=title,
            width=width,
            height=height,
            band_count=band_count,
            exposure_count=exposure_count,
        )
        entry["path"] = relpath_posix(scenes_dir / scene_id, base)
        entries.append(entry)
    faults = {}
    if with_faults and entries:
        source = base / entries[0]["path"]
        fault_root = base / "faults"
        fault_root.mkdir(exist_ok=True)
        _copy_without_metadata(source, fault_root / "missing_meta")
        _make_dim_fault(source, fault_root / "dim_mismatch")
        _make_decode_fault(source, fault_root / "corrupt_image")
        faults = {
            "missing_meta": {"path": "faults/missing_meta", "expected_code": "E_META"},
            "dim_mismatch": {"path": "faults/dim_mismatch", "expected_code": "E_DIM"},
            "corrupt_image": {"path": "faults/corrupt_image", "expected_code": "E_DECODE"},
        }
    catalog = {
        "schema_version": "1.0",
        "generator": "spectraforge-sample-generator",
        "generator_version": "1.0.0",
        "seed": int(seed),
        "scenes": entries,
        "faults": faults,
        "generated_by_seed_only": True,
    }
    atomic_write_json(base / "catalog.json", catalog)
    return catalog


def catalog_path(samples_root: str | Path) -> Path:
    path = Path(samples_root) / "catalog.json"
    if not path.exists():
        raise WorkbenchError("E_META", "Sample catalog is missing", {"path": str(path)})
    return path
