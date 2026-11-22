"""Preview/exact and three-path consistency checks with diagnostics."""

from __future__ import annotations

import json
import hashlib
from pathlib import Path
from typing import Any

import numpy as np

from .errors import DivergenceError, PathConsistencyError
from .model import ConsistencyReport, NprFrame, level_histogram
from .npr import coverage_preview, level_preview, outline_preview
from .tiles import tile_for_pixel

DEFAULT_TOLERANCES = {
    "level_histogram_deviation": 0.035,
    "outline_coverage_delta": 0.040,
    "stroke_coverage_delta": 0.120,
    "outline_iou_min": 0.42,
}


def save_ppm(path: str | Path, rgb: np.ndarray) -> None:
    file_path = Path(path)
    file_path.parent.mkdir(parents=True, exist_ok=True)
    image = np.asarray(rgb, dtype=np.uint8)
    if image.ndim != 3 or image.shape[2] != 3:
        raise ValueError("PPM image must be HxWx3")
    header = f"P6\n{image.shape[1]} {image.shape[0]}\n255\n".encode("ascii")
    file_path.write_bytes(header + np.ascontiguousarray(image).tobytes())


def _block_factor(source_h: int, source_w: int, target_h: int, target_w: int) -> tuple[int, int] | None:
    if source_h % target_h == 0 and source_w % target_w == 0:
        return source_h // target_h, source_w // target_w
    return None


def _area_mean(source: np.ndarray, target_h: int, target_w: int) -> np.ndarray:
    factor = _block_factor(source.shape[0], source.shape[1], target_h, target_w)
    if factor is not None:
        fy, fx = factor
        if source.ndim == 2:
            return source.reshape(target_h, fy, target_w, fx).mean(axis=(1, 3))
        return source.reshape(target_h, fy, target_w, fx, source.shape[2]).mean(axis=(1, 3))
    y_idx = np.minimum((np.arange(target_h) * source.shape[0] // target_h), source.shape[0] - 1)
    x_idx = np.minimum((np.arange(target_w) * source.shape[1] // target_w), source.shape[1] - 1)
    return source[np.ix_(y_idx, x_idx)]


def _area_any(source: np.ndarray, target_h: int, target_w: int) -> np.ndarray:
    factor = _block_factor(source.shape[0], source.shape[1], target_h, target_w)
    if factor is not None:
        fy, fx = factor
        return source.reshape(target_h, fy, target_w, fx).max(axis=(1, 3)).astype(bool)
    return _area_mean(source.astype(np.float64), target_h, target_w) >= 0.5


def _area_levels(source: np.ndarray, target_h: int, target_w: int) -> np.ndarray:
    # Nearest is sufficient for locating a representative mismatch; histogram
    # deviation itself is computed independently at full source resolution.
    y_idx = np.minimum(
        ((np.arange(target_h) + 0.5) * source.shape[0] / target_h).astype(int),
        source.shape[0] - 1,
    )
    x_idx = np.minimum(
        ((np.arange(target_w) + 0.5) * source.shape[1] / target_w).astype(int),
        source.shape[1] - 1,
    )
    return source[np.ix_(y_idx, x_idx)]


def _dilate(mask: np.ndarray) -> np.ndarray:
    out = mask.copy()
    out[:, 1:] |= mask[:, :-1]
    out[:, :-1] |= mask[:, 1:]
    out[1:, :] |= mask[:-1, :]
    out[:-1, :] |= mask[1:, :]
    return out


def _iou_after_dilation(a: np.ndarray, b: np.ndarray) -> float:
    da = _dilate(a)
    db = _dilate(b)
    union = np.count_nonzero(da | db)
    if union == 0:
        return 1.0
    return float(np.count_nonzero(da & db) / union)


def difference_heatmap(preview: NprFrame, exact: NprFrame) -> np.ndarray:
    h, w = preview.height, preview.width
    exact_scaled = _area_mean(exact.styled_rgb.astype(np.float64), h, w)
    preview_float = preview.styled_rgb.astype(np.float64)
    diff = np.mean(np.abs(exact_scaled - preview_float), axis=2)
    normalized = np.clip(diff / 96.0, 0.0, 1.0)
    heat = np.empty((h, w, 3), dtype=np.uint8)
    heat[..., 0] = np.clip(normalized * 255.0, 0, 255).astype(np.uint8)
    heat[..., 1] = np.clip((normalized * 1.8 - 0.25) * 255.0, 0, 255).astype(np.uint8)
    heat[..., 2] = np.clip((0.18 - normalized * 0.18) * 255.0, 0, 255).astype(np.uint8)
    return heat


def _save_frame_tensors(directory: Path, prefix: str, frame: NprFrame) -> None:
    np.savez(
        directory / f"{prefix}_tensors.npz",
        base_rgb=frame.base_rgb,
        poster_rgb=frame.poster_rgb,
        levels=frame.levels,
        outline=frame.outline.astype(np.uint8),
        brush_rgba=frame.brush_rgba,
        styled_rgb=frame.styled_rgb,
        coverage=frame.coverage.astype(np.uint8),
    )
    save_ppm(directory / f"{prefix}_base.ppm", frame.base_rgb)
    save_ppm(directory / f"{prefix}_poster.ppm", frame.poster_rgb)
    save_ppm(
        directory / f"{prefix}_levels.ppm",
        level_preview(frame.levels, frame.snapshot.params.levels),
    )
    save_ppm(directory / f"{prefix}_outline.ppm", outline_preview(frame.outline))
    save_ppm(directory / f"{prefix}_brush.ppm", frame.brush_rgba[..., :3])
    save_ppm(directory / f"{prefix}_coverage.ppm", coverage_preview(frame.coverage))
    save_ppm(directory / f"{prefix}_styled.ppm", frame.styled_rgb)


def save_divergence_diagnostics(
    directory: str | Path,
    preview: NprFrame,
    exact: NprFrame,
    report: ConsistencyReport,
    heatmap: np.ndarray | None = None,
) -> dict[str, str]:
    path = Path(directory)
    path.mkdir(parents=True, exist_ok=True)
    _save_frame_tensors(path, "preview", preview)
    _save_frame_tensors(path, "exact", exact)
    if heatmap is None:
        heatmap = difference_heatmap(preview, exact)
    save_ppm(path / "difference_heatmap.ppm", heatmap)
    report_payload = report.to_dict()
    (path / "consistency_report.json").write_text(
        json.dumps(report_payload, indent=2, sort_keys=True, ensure_ascii=True) + "\n",
        encoding="utf-8",
    )
    return {
        "directory": str(path),
        "report": str(path / "consistency_report.json"),
        "heatmap": str(path / "difference_heatmap.ppm"),
        "preview_tensors": str(path / "preview_tensors.npz"),
        "exact_tensors": str(path / "exact_tensors.npz"),
    }


def compare_preview_exact(
    preview: NprFrame,
    exact: NprFrame,
    *,
    tolerances: dict[str, float] | None = None,
    diagnostic_dir: str | Path | None = None,
    raise_on_diverge: bool = True,
) -> ConsistencyReport:
    tol = dict(DEFAULT_TOLERANCES)
    if tolerances:
        tol.update(tolerances)
    if preview.snapshot.version != exact.snapshot.version:
        # Callers normally enforce this before rendering; this guard prevents a
        # metrics-only comparison from ever silently blending versions.
        from .errors import VersionConflictError

        raise VersionConflictError(
            preview.snapshot.version,
            exact.snapshot.version,
            "preview and exact frames carry different parameter versions",
        )
    comparison_h = min(preview.height, exact.height)
    comparison_w = min(preview.width, exact.width)
    preview_levels = _area_levels(preview.levels, comparison_h, comparison_w)
    exact_levels = _area_levels(exact.levels, comparison_h, comparison_w)
    preview_outline = _area_any(preview.outline, comparison_h, comparison_w)
    exact_outline = _area_any(exact.outline, comparison_h, comparison_w)
    preview_alpha = preview.brush_rgba[..., 3].astype(np.float64) / 255.0
    exact_alpha = _area_mean(
        exact.brush_rgba[..., 3].astype(np.float64) / 255.0,
        comparison_h,
        comparison_w,
    )
    preview_coverage = preview_alpha > (8.0 / 255.0)
    exact_coverage = exact_alpha > (8.0 / 255.0)

    preview_hist = level_histogram(preview.levels, preview.snapshot.params.levels)
    exact_hist = level_histogram(exact.levels, exact.snapshot.params.levels)
    hist_delta = 0.5 * float(np.sum(np.abs(preview_hist - exact_hist)))
    preview_outline_density = float(np.mean(preview_outline))
    exact_outline_density = float(np.mean(exact_outline))
    outline_delta = abs(preview_outline_density - exact_outline_density)
    preview_stroke_density = float(np.mean(preview_alpha))
    exact_stroke_density = float(np.mean(exact_alpha))
    stroke_delta = abs(preview_stroke_density - exact_stroke_density)
    outline_iou = _iou_after_dilation(preview_outline, exact_outline)

    mismatch = (
        (preview_levels != exact_levels)
        | (preview_outline != exact_outline)
        | (preview_coverage != exact_coverage)
    )
    first_pixel: tuple[int, int] | None = None
    first_tile: int | None = None
    if np.any(mismatch):
        y, x = np.argwhere(mismatch)[0]
        exact_x = min(
            exact.width - 1,
            int((x + 0.5) * exact.width / comparison_w),
        )
        exact_y = min(
            exact.height - 1,
            int((y + 0.5) * exact.height / comparison_h),
        )
        first_pixel = (int(exact_x), int(exact_y))
        first_tile = tile_for_pixel(
            exact_x,
            exact_y,
            exact.width,
            exact.height,
            exact.snapshot.params.tile_width,
            exact.snapshot.params.tile_height,
        )

    passed = (
        hist_delta <= tol["level_histogram_deviation"]
        and outline_delta <= tol["outline_coverage_delta"]
        and stroke_delta <= tol["stroke_coverage_delta"]
        and outline_iou >= tol["outline_iou_min"]
    )
    report = ConsistencyReport(
        level_histogram_deviation=hist_delta,
        outline_coverage_delta=outline_delta,
        stroke_coverage_delta=stroke_delta,
        outline_iou=outline_iou,
        first_mismatch_pixel=first_pixel,
        first_mismatch_tile=first_tile,
        preview_version=preview.snapshot.version,
        exact_version=exact.snapshot.version,
        comparison_size=(comparison_w, comparison_h),
        tolerances=tol,
        passed=passed,
    )
    if not passed:
        diagnostics: dict[str, str] = {}
        if diagnostic_dir is not None:
            diagnostics = save_divergence_diagnostics(
                diagnostic_dir, preview, exact, report, difference_heatmap(preview, exact)
            )
        if raise_on_diverge:
            raise DivergenceError(report, preview, exact, diagnostics)
    return report


def _first_array_mismatch(a: np.ndarray, b: np.ndarray) -> tuple[int, int] | None:
    if a.shape != b.shape:
        return (0, 0)
    if a.ndim == 3:
        mask = np.any(a != b, axis=2)
    else:
        mask = a != b
    if not np.any(mask):
        return None
    y, x = np.argwhere(mask)[0]
    return int(x), int(y)


def save_path_diagnostics(
    directory: str | Path,
    frames: dict[str, NprFrame],
    report: dict[str, Any],
) -> dict[str, str]:
    path = Path(directory)
    path.mkdir(parents=True, exist_ok=True)
    saved: dict[str, str] = {"directory": str(path)}
    for label, frame in frames.items():
        _save_frame_tensors(path, label, frame)
        saved[f"{label}_tensors"] = str(path / f"{label}_tensors.npz")
    (path / "path_report.json").write_text(
        json.dumps(report, indent=2, sort_keys=True, ensure_ascii=True) + "\n",
        encoding="utf-8",
    )
    saved["report"] = str(path / "path_report.json")
    return saved


def compare_three_paths(
    interactive: NprFrame,
    resumed: NprFrame,
    one_shot: NprFrame,
    *,
    diagnostic_dir: str | Path | None = None,
    raise_on_mismatch: bool = True,
) -> dict[str, Any]:
    frames = {"interactive": interactive, "resumed": resumed, "one_shot": one_shot}
    labels = list(frames)
    reference = frames[labels[0]]
    mismatches: list[dict[str, Any]] = []
    for label in labels[1:]:
        candidate = frames[label]
        pixel = _first_array_mismatch(reference.styled_rgb, candidate.styled_rgb)
        level_hist_equal = bool(
            np.array_equal(
                level_histogram(reference.levels, reference.snapshot.params.levels),
                level_histogram(candidate.levels, candidate.snapshot.params.levels),
            )
        )
        outline_equal = bool(np.array_equal(reference.outline, candidate.outline))
        coverage_equal = bool(np.array_equal(reference.coverage, candidate.coverage))
        if (
            pixel is not None
            or not level_hist_equal
            or not outline_equal
            or not coverage_equal
            or reference.snapshot.version != candidate.snapshot.version
        ):
            tile_id = None
            if pixel is not None:
                tile_id = tile_for_pixel(
                    pixel[0],
                    pixel[1],
                    candidate.width,
                    candidate.height,
                    candidate.snapshot.params.tile_width,
                    candidate.snapshot.params.tile_height,
                )
            mismatches.append(
                {
                    "pair": [labels[0], label],
                    "first_mismatch_pixel": pixel,
                    "tile_id": tile_id,
                    "level_histogram_equal": level_hist_equal,
                    "outline_equal": outline_equal,
                    "stroke_coverage_equal": coverage_equal,
                    "versions": [reference.version, candidate.version],
                }
            )
    report = {
        "passed": not mismatches,
        "paths": labels,
        "pixel_exact": all(item["first_mismatch_pixel"] is None for item in mismatches),
        "mismatches": mismatches,
        "styles_sha256": {
            label: hashlib.sha256(frame.styled_rgb.tobytes()).hexdigest()
            for label, frame in frames.items()
        },
    }
    if mismatches:
        diagnostics: dict[str, str] = {}
        if diagnostic_dir is not None:
            diagnostics = save_path_diagnostics(diagnostic_dir, frames, report)
        if raise_on_mismatch:
            raise PathConsistencyError(report, diagnostics)
    return report

