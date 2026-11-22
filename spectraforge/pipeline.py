"""Segment processing, deterministic assembly, and analytical truth checks."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .checkpoint import CheckpointManager
from .color import (
    gamut_report,
    map_gamut,
    normalize_chromaticity,
    reconstruct_linear_rgb,
    source_xyz_to_target_rgb,
)
from .errors import WorkbenchError
from .fusion import fuse_exposures
from .metadata import band_radiance
from .models import CompositeOutput, Scene, SegmentResult, SegmentSpec
from .parameters import parameter_version, validate_parameters
from .registration import align_bands
from .tone import display_luminance_histogram, tone_map


@dataclass
class PreparedScene:
    aligned_bands: np.ndarray
    alignment_report: dict[str, Any]


def prepare_scene(scene: Scene, interpolation: str) -> PreparedScene:
    linear = band_radiance(scene)
    aligned, report = align_bands(linear, scene.metadata.registration_offsets, interpolation)
    if not np.all(np.isfinite(aligned)):
        raise WorkbenchError("E_DECODE", "Registration produced non-finite band values")
    return PreparedScene(aligned_bands=aligned, alignment_report=report)


def _basic_stats(radiance: np.ndarray, display: np.ndarray, fusion_result: Any, gamut: dict[str, Any]) -> dict[str, Any]:
    return {
        "peak_radiance": float(np.max(radiance)),
        "mean_radiance": float(np.mean(radiance)),
        "saturated_fraction": float(fusion_result.saturated_fraction),
        "underexposed_fraction": float(fusion_result.underexposed_fraction),
        "fallback_fraction": float(fusion_result.fallback_fraction),
        "gamut_pixel_count": int(gamut["pixel_count"]),
        "gamut_channel_count": int(gamut["channel_count"]),
        "display_clipped_fraction": float(np.mean((display == 0) | (display == 255))),
        "histogram": display_luminance_histogram(radiance, bins=48),
    }


def _final_chroma_from_mapped(
    absolute_rgb: np.ndarray,
    target_gamut: str,
    intent: str,
    offset: tuple[int, int],
) -> tuple[np.ndarray, np.ndarray, dict[str, Any]]:
    chromaticity = normalize_chromaticity(absolute_rgb)
    mapped, mask, report = map_gamut(chromaticity, intent, offset=offset)
    luma = 0.2126 * mapped[..., 0] + 0.7152 * mapped[..., 1] + 0.0722 * mapped[..., 2]
    positive = luma > 1e-12
    safe = np.where(positive, luma, 1.0)
    chroma = mapped / safe[..., np.newaxis]
    if np.any(~positive):
        chroma = np.where(positive[..., np.newaxis], chroma, 1.0)
    return chroma, mask, report


def process_segment(
    scene: Scene,
    prepared: PreparedScene,
    spec: SegmentSpec,
    params: dict[str, Any],
) -> SegmentResult:
    normalized = validate_parameters(params, scene.exposure_count)
    if spec.mode == "tiles":
        band_slice = prepared.aligned_bands[:, spec.y0 : spec.y1, spec.x0 : spec.x1]
        exposure_slice = scene.exposure_raw[:, spec.y0 : spec.y1, spec.x0 : spec.x1]
        fusion = fuse_exposures(
            exposure_slice,
            scene.metadata.exposures,
            normalized,
            scene.metadata.camera_response,
        )
        absolute_rgb, chroma, gamut_mask, gamut = reconstruct_linear_rgb(
            band_slice,
            scene.metadata,
            normalized["target_gamut"],
            intent=normalized["gamut_intent"],
            offset=(spec.x0, spec.y0),
        )
        linear_rgb = chroma * fusion.radiance[..., np.newaxis]
        display, _, _ = tone_map(linear_rgb, normalized)
        stats = _basic_stats(fusion.radiance, display, fusion, gamut)
    elif spec.mode == "bands":
        if spec.band_start is None or spec.band_end is None or spec.band_end - spec.band_start != 1:
            raise WorkbenchError("E_STATE", "Band segment range is invalid", {"segment_index": spec.index})
        band_index = spec.band_start
        band_slice = prepared.aligned_bands[band_index : band_index + 1]
        exposure_slice = scene.exposure_raw
        fusion = fuse_exposures(
            exposure_slice,
            scene.metadata.exposures,
            normalized,
            scene.metadata.camera_response,
        )
        xyz = band_slice[0, :, :, np.newaxis] * scene.metadata.xyz_basis[band_index][np.newaxis, np.newaxis, :]
        absolute_rgb = source_xyz_to_target_rgb(
            xyz,
            scene.metadata.scene_white_xyz,
            normalized["target_gamut"],
        )
        chroma = normalize_chromaticity(absolute_rgb)
        gamut_mask = np.zeros((spec.height, spec.width, 3), dtype=np.uint8)
        gamut = {
            "pixel_count": 0,
            "channel_count": 0,
            "coordinates": [],
            "first": None,
        }
        preview_linear = absolute_rgb * fusion.radiance[..., np.newaxis]
        display, _, _ = tone_map(preview_linear, normalized)
        stats = _basic_stats(fusion.radiance, display, fusion, gamut)
        linear_rgb = absolute_rgb
    else:
        raise WorkbenchError("E_PARA", "Unknown segment mode", {"segment_index": spec.index})
    stats["alignment_residual"] = float(prepared.alignment_report["mean_structural_residual"])
    stats["parameter_version"] = parameter_version(normalized, scene.exposure_count)
    if not np.all(np.isfinite(linear_rgb)):
        raise WorkbenchError(
            "E_INTERNAL",
            "Segment produced non-finite linear RGB",
            {"segment_index": spec.index},
        )
    return SegmentResult(
        spec=spec,
        radiance=np.ascontiguousarray(fusion.radiance, dtype=np.float32),
        absolute_rgb=np.ascontiguousarray(absolute_rgb, dtype=np.float32),
        rgb_chroma=np.ascontiguousarray(chroma, dtype=np.float32),
        linear_rgb=np.ascontiguousarray(linear_rgb, dtype=np.float32),
        display=np.ascontiguousarray(display, dtype=np.uint8),
        gamut_channel_mask=np.ascontiguousarray(gamut_mask, dtype=np.uint8),
        params=normalized,
        param_version=stats["parameter_version"],
        stats=stats,
        alignment_residual=float(prepared.alignment_report["mean_structural_residual"]),
    )


def _empty_output(scene: Scene, segments: list[SegmentSpec], mode: str) -> dict[str, np.ndarray]:
    height, width = scene.height, scene.width
    return {
        "radiance": np.zeros((height, width), dtype=np.float32),
        "absolute_rgb": np.zeros((height, width, 3), dtype=np.float32),
        "rgb_chroma": np.zeros((height, width, 3), dtype=np.float32),
        "linear_rgb": np.zeros((height, width, 3), dtype=np.float32),
        "display": np.zeros((height, width, 3), dtype=np.uint8),
        "gamut_channel_mask": np.zeros((height, width, 3), dtype=np.uint8),
        "segment_index": np.zeros((height, width), dtype=np.int32),
    }


def assemble_output(
    scene: Scene,
    manager: CheckpointManager,
    *,
    require_complete: bool = True,
    assembly_params: dict[str, Any] | None = None,
) -> CompositeOutput:
    mode = manager.segments[0].mode if manager.segments else "tiles"
    arrays = _empty_output(scene, manager.segments, mode)
    committed: list[tuple[Any, dict[str, Any]]] = list(manager.committed_data())
    param_versions: list[str] = []
    segment_stats: dict[int, dict[str, Any]] = {}
    for record, loaded in committed:
        spec = manager.segments[record.index]
        data = loaded["arrays"]
        segment_stats[record.index] = record.stats
        if record.param_version and record.param_version not in param_versions:
            param_versions.append(record.param_version)
        if mode == "tiles":
            region = (slice(spec.y0, spec.y1), slice(spec.x0, spec.x1))
            arrays["radiance"][region] = data["radiance"]
            arrays["absolute_rgb"][region] = data["absolute_rgb"]
            arrays["rgb_chroma"][region] = data["rgb_chroma"]
            arrays["linear_rgb"][region] = data["linear_rgb"]
            arrays["display"][region] = data["display"]
            arrays["gamut_channel_mask"][region] = data["gamut_channel_mask"]
            arrays["segment_index"][region] = spec.index
        else:
            arrays["absolute_rgb"] += data["absolute_rgb"]
            arrays["radiance"] = data["radiance"]
            arrays["segment_index"][:] = spec.index
    if mode == "bands" and committed:
        last_record = committed[-1][0]
        params = assembly_params or last_record.params or scene.metadata.default_parameters
        params = validate_parameters(params, scene.exposure_count)
        chroma, gamut_mask, gamut = _final_chroma_from_mapped(
            arrays["absolute_rgb"].astype(np.float64),
            params["target_gamut"],
            params["gamut_intent"],
            (0, 0),
        )
        arrays["rgb_chroma"] = chroma.astype(np.float32)
        arrays["gamut_channel_mask"] = gamut_mask
        arrays["linear_rgb"] = (chroma * arrays["radiance"][..., np.newaxis]).astype(np.float32)
        display, _, _ = tone_map(arrays["linear_rgb"], params)
        arrays["display"] = display
    else:
        if mode == "tiles":
            gamut = gamut_report(
                arrays["gamut_channel_mask"].astype(bool),
                arrays["rgb_chroma"].astype(np.float64) / np.maximum(
                    np.abs(arrays["rgb_chroma"].astype(np.float64)).max(axis=2, keepdims=True), 1e-12
                ),
                offset=(0, 0),
            )
        else:
            gamut = {"pixel_count": 0, "channel_count": 0, "coordinates": [], "first": None}
    complete = manager.is_complete()
    if require_complete and not complete:
        progress = manager.progress()
        raise WorkbenchError(
            "E_INCOMPLETE",
            "Snapshot is incomplete and cannot be exported as a full result",
            {
                "committed": progress["committed"],
                "total": progress["total"],
                "failed": progress["failed"],
                "pending": progress["pending"],
            },
        )
    radiance = arrays["radiance"]
    if radiance.size:
        peak_y, peak_x = np.unravel_index(int(np.argmax(radiance)), radiance.shape)
        peak_value = float(radiance[peak_y, peak_x])
    else:
        peak_x = peak_y = 0
        peak_value = 0.0
    return CompositeOutput(
        radiance=radiance,
        rgb_chroma=arrays["rgb_chroma"],
        linear_rgb=arrays["linear_rgb"],
        display=arrays["display"],
        gamut_channel_mask=arrays["gamut_channel_mask"],
        segment_index=arrays["segment_index"],
        peak_radiance=peak_value,
        peak_location=(int(peak_x), int(peak_y)),
        gamut_count=int(gamut["pixel_count"]),
        gamut_coordinates=list(gamut["coordinates"]),
        segment_stats=segment_stats,
        complete=complete,
        param_versions=param_versions,
        metadata_version=manager.metadata.metadata_version,
        metadata_hash=manager.metadata.metadata_hash,
        algorithm_version=str(manager.manifest.get("algorithm_version", "")),
    )


def evaluate_color_truth(
    scene: Scene,
    prepared: PreparedScene,
    *,
    target_gamut: str = "sRGB",
) -> dict[str, Any]:
    truth = scene.truth
    if not truth:
        raise WorkbenchError("E_META", "Scene has no analytical truth file")
    tolerance = float(truth.get("tolerance", 0.0))
    patch_results = []
    max_error = 0.0
    for patch in truth.get("patches", []):
        x0 = int(patch["x0"])
        y0 = int(patch["y0"])
        x1 = int(patch["x1"])
        y1 = int(patch["y1"])
        inset = 3
        mean_bands = prepared.aligned_bands[
            :, y0 + inset : max(y0 + inset + 1, y1 - inset), x0 + inset : max(x0 + inset + 1, x1 - inset)
        ].mean(axis=(1, 2))
        absolute_rgb, chroma, _, gamut = reconstruct_linear_rgb(
            mean_bands[:, np.newaxis, np.newaxis],
            scene.metadata,
            target_gamut,
            intent="compress",
        )
        actual = absolute_rgb[0, 0]
        expected = np.asarray(patch["expected_linear_rgb_srgb"], dtype=np.float64)
        error = float(np.max(np.abs(actual - expected)))
        max_error = max(max_error, error)
        patch_results.append(
            {
                "id": patch["id"],
                "expected_linear_rgb": expected.tolist(),
                "actual_linear_rgb": actual.tolist(),
                "max_abs_error": error,
                "within_tolerance": error <= tolerance,
                "out_of_gamut_channels": int(gamut["channel_count"]),
            }
        )
    params = scene.metadata.default_parameters
    fusion = fuse_exposures(
        scene.exposure_raw,
        scene.metadata.exposures,
        params,
        scene.metadata.camera_response,
    )
    expected_peak = float(truth.get("peak_radiance", 0.0))
    actual_peak = float(np.max(fusion.radiance))
    peak_error = abs(actual_peak - expected_peak) / max(expected_peak, 1e-12)
    return {
        "tolerance": tolerance,
        "max_abs_error": max_error,
        "all_patches_within_tolerance": all(item["within_tolerance"] for item in patch_results),
        "patches": patch_results,
        "expected_peak_radiance": expected_peak,
        "fused_peak_radiance": actual_peak,
        "peak_relative_error": peak_error,
        "alignment": prepared.alignment_report,
        "registration_offsets": scene.metadata.registration_offsets.tolist(),
        "truth_offsets": truth.get("registration_offsets", []),
    }
