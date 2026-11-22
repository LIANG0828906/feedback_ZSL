"""End-to-end segmented synthesis, enhancement, persistence, and reporting."""

from __future__ import annotations

import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image

from . import ALGORITHM_VERSION
from .checkpoint import SEGMENT_ARRAY_FIELDS, CheckpointStore
from .color import (
    bands_to_xyz,
    linear_rgb_to_preview_rgb8,
    map_to_gamut,
    quantize_linear_rgb,
    xyz_to_linear_rgb,
)
from .dataset import LoadedScene
from .errors import WorkbenchError, require
from .filtering import denoise_and_enhance_detail, filtering_quality
from .io_utils import atomic_save_npy, atomic_write_text, sha256_file, write_json
from .params import WorkbenchParams
from .radiometry import linearize_and_fuse
from .registration import align_bands
from .segments import (
    OrderedTask,
    PixelBudget,
    SegmentSpec,
    SegmentState,
    SegmentStateMachine,
    build_segments,
    run_ordered_bounded,
)


@dataclass
class BaseContext:
    aligned_bands: np.ndarray
    fused_radiance: np.ndarray
    registration: dict[str, Any]
    fusion: dict[str, Any]
    key: tuple[Any, ...]


@dataclass
class RunResult:
    scene_id: str
    mode: str
    complete: bool
    committed_count: int
    total_count: int
    output_dir: Path | None = None
    stats: dict[str, Any] | None = None
    checkpoint_invalid_reason: str | None = None
    parallelism: dict[str, Any] | None = None


class WorkbenchEngine:
    def __init__(
        self,
        scene: LoadedScene,
        params: WorkbenchParams,
        *,
        checkpoint_root: Path | None = None,
        start_mode: str = "none",
        algorithm_version: str = ALGORITHM_VERSION,
    ):
        self.scene = scene
        self.params = params
        self.algorithm_version = algorithm_version
        params.validate(scene.metadata, frame_count=scene.exposure_frames.shape[0])
        self.segments = build_segments(*scene.pixel_size, params.segment_size)
        self.state_machine = SegmentStateMachine(self.segments)
        self.committed_arrays: dict[int, dict[str, np.ndarray]] = {}
        self._base: BaseContext | None = None
        self.checkpoint_invalid_reason: str | None = None
        self.store: CheckpointStore | None = None
        if checkpoint_root is not None:
            self.store = CheckpointStore(checkpoint_root, scene.scene_id)
            if start_mode == "fresh":
                self.store.reset()
            elif start_mode == "resume":
                loaded = self.store.load(
                    metadata_version=scene.metadata_version,
                    params_version=params.version(),
                    algorithm_version=algorithm_version,
                    segment_size=params.segment_size,
                    total_segments=len(self.segments),
                )
                self.checkpoint_invalid_reason = loaded.invalid_reason
                if loaded.valid:
                    for index, entry in loaded.entries.items():
                        self.committed_arrays[index] = entry["arrays"]
                        self.state_machine.restore_committed(
                            index,
                            metadata_snapshot=entry["metadata_snapshot"],
                            params_snapshot=entry["params_snapshot"],
                            metadata_version=entry["metadata_version"],
                            params_version=entry["params_version"],
                            algorithm_version=entry["algorithm_version"],
                        )
            elif start_mode != "none" and start_mode != "reuse":
                raise WorkbenchError("E_PARA", f"Unknown start mode: {start_mode}", field="start_mode")

    @property
    def committed_count(self) -> int:
        return len(self.committed_arrays)

    @property
    def total_count(self) -> int:
        return len(self.segments)

    @property
    def progress(self) -> str:
        return f"{self.committed_count}/{self.total_count}"

    def update_params(self, params: WorkbenchParams, *, resume: bool = False) -> None:
        params.validate(self.scene.metadata, frame_count=self.scene.exposure_frames.shape[0])
        require(
            params.segment_size == self.params.segment_size,
            "E_PARA",
            "segment_size cannot change after segmentation has been initialized",
            field="segment_size",
        )
        if resume and self.store is not None:
            loaded = self.store.load(
                metadata_version=self.scene.metadata_version,
                params_version=params.version(),
                algorithm_version=self.algorithm_version,
                segment_size=params.segment_size,
                total_segments=len(self.segments),
            )
            if loaded.valid:
                self.params = params
                self.committed_arrays = {index: entry["arrays"] for index, entry in loaded.entries.items()}
                return
            self.checkpoint_invalid_reason = loaded.invalid_reason
            self.committed_arrays = {}
            self.state_machine = SegmentStateMachine(self.segments)
        self.params = params

    def _ensure_base(self) -> BaseContext:
        weights = tuple(float(value) for value in self.params.exposure_weights)
        key = (
            int(self.params.interpolation_order),
            weights,
            self.scene.metadata_version,
        )
        if self._base is not None and self._base.key == key:
            return self._base
        aligned, registration_report = align_bands(
            self.scene.bands,
            self.scene.registration_offsets,
            reference_band=self.scene.reference_band,
            interpolation_order=self.params.interpolation_order,
            max_shift_px=float(self.scene.metadata["max_registration_shift_px"]),
        )
        fused, fusion_stats = linearize_and_fuse(
            self.scene.exposure_frames,
            self.scene.metadata["exposures"],
            weights,
            bit_depth=int(self.scene.metadata["exposures"]["bit_depth"]),
        )
        self._base = BaseContext(
            aligned_bands=aligned,
            fused_radiance=fused,
            registration=registration_report,
            fusion=fusion_stats,
            key=key,
        )
        return self._base

    def _segment_pixel_cost(self, segment: SegmentSpec) -> int:
        halo = 3 * int(self.params.guide_radius) + 4
        y0 = max(0, segment.y0 - halo)
        x0 = max(0, segment.x0 - halo)
        y1 = min(self.scene.pixel_size[0], segment.y1 + halo)
        x1 = min(self.scene.pixel_size[1], segment.x1 + halo)
        return int((y1 - y0) * (x1 - x0))

    def _process_segment(self, segment: SegmentSpec, base: BaseContext) -> dict[str, np.ndarray]:
        radius = int(self.params.guide_radius)
        halo = 3 * radius + 4
        height, width = self.scene.pixel_size
        y0 = max(0, segment.y0 - halo)
        x0 = max(0, segment.x0 - halo)
        y1 = min(height, segment.y1 + halo)
        x1 = min(width, segment.x1 + halo)

        guide_roi = base.fused_radiance[y0:y1, x0:x1]
        detail_roi = self.scene.detail_layer[y0:y1, x0:x1]
        cleaned_roi, enhanced_roi = denoise_and_enhance_detail(
            guide_roi,
            detail_roi,
            radius=radius,
            eps=float(self.params.guide_eps),
            gain=float(self.params.contrast_gain),
            threshold=float(self.params.contrast_threshold),
        )

        target_y0 = segment.y0 - y0
        target_x0 = segment.x0 - x0
        target_y1 = target_y0 + segment.height
        target_x1 = target_x0 + segment.width
        cleaned = cleaned_roi[target_y0:target_y1, target_x0:target_x1]
        enhanced = enhanced_roi[target_y0:target_y1, target_x0:target_x1]

        chroma_xyz_full = bands_to_xyz(base.aligned_bands[:, y0:y1, x0:x1], self.scene.xyz_weights)
        linear_rgb_full = xyz_to_linear_rgb(
            chroma_xyz_full,
            source_white_xyz=self.scene.source_white_xyz,
            target_white_xyz=self.scene.target_white_xyz,
            target_matrix=self.scene.target_matrix,
        )
        chroma_xyz = chroma_xyz_full[target_y0:target_y1, target_x0:target_x1]
        linear_rgb = linear_rgb_full[target_y0:target_y1, target_x0:target_x1].copy()

        neutral = float(self.scene.metadata["detail"].get("neutral_value", 0.5))
        detail_gain = np.clip(enhanced / neutral, 0.25, 4.0)
        linear_rgb *= detail_gain[..., None]
        gamut = map_to_gamut(
            linear_rgb,
            intent=self.params.gamut_intent,
            tolerance=float(self.params.gamut_tolerance),
            max_iterations=int(self.params.max_gamut_iterations),
        )
        synthetic, _levels = quantize_linear_rgb(gamut.mapped, self.scene.target_bit_depth)

        arrays = {
            "fused_radiance": np.asarray(base.fused_radiance[segment.y0 : segment.y1, segment.x0 : segment.x1], dtype=np.float64),
            "cleaned_detail": np.asarray(cleaned, dtype=np.float64),
            "enhanced_detail": np.asarray(enhanced, dtype=np.float64),
            "chroma_xyz": np.asarray(chroma_xyz, dtype=np.float64),
            "linear_rgb": np.asarray(gamut.mapped, dtype=np.float64),
            "gamut_mask": np.asarray(gamut.out_of_gamut_mask, dtype=np.bool_),
            "boundary_rgb": np.asarray(gamut.boundary_rgb, dtype=np.float64),
            "gamut_excess": np.asarray(gamut.original_excess, dtype=np.float64),
            "synthetic_rgb": np.asarray(synthetic),
        }
        return arrays

    def run(
        self,
        *,
        output_dir: Path | None = None,
        commit_limit: int | None = None,
        bad_segment: int | None = None,
        crash_after_commits: int | None = None,
        mode: str = "full",
        verbose: bool = False,
    ) -> RunResult:
        self.params.validate(self.scene.metadata, frame_count=self.scene.exposure_frames.shape[0])
        if bad_segment is not None and not (0 <= bad_segment < len(self.segments)):
            raise WorkbenchError("E_SEG", "Injected bad segment index is out of range", field="bad_segment", index=bad_segment)
        pending = [
            segment
            for segment in self.segments
            if self.state_machine.records[segment.index].state in {SegmentState.PENDING, SegmentState.FAILED}
        ]
        if commit_limit is not None:
            require(commit_limit >= 0, "E_PARA", "commit_limit must be non-negative", field="commit_limit")
            pending = pending[: int(commit_limit)]

        parallelism: dict[str, Any] = {"mode": "idle", "budget": {}}
        failures: list[dict[str, Any]] = []
        if pending:
            for segment in pending:
                cost = self._segment_pixel_cost(segment)
                require(
                    cost <= self.params.max_pixels_per_thread,
                    "E_PARA",
                    "Segment ROI pixel buffer exceeds max_pixels_per_thread",
                    field="max_pixels_per_thread",
                    segment_index=segment.index,
                    required_pixels=cost,
                    limit=self.params.max_pixels_per_thread,
                )
            base = self._ensure_base()
            for segment in pending:
                self.state_machine.begin(segment.index)
            metadata_snapshot = self.scene.metadata_snapshot()
            params_snapshot = self.params.algorithm_snapshot()
            params_version = self.params.version()
            budget = PixelBudget(self.params.max_workers, self.params.max_pixels_per_thread)

            def make_task(segment: SegmentSpec) -> OrderedTask:
                def execute() -> dict[str, np.ndarray]:
                    if bad_segment == segment.index:
                        raise WorkbenchError(
                            "E_SEG",
                            "Injected bad segment failure",
                            field=f"segments[{segment.index}]",
                            details={"segment_index": segment.index, "bbox": list(segment.bbox)},
                        )
                    return self._process_segment(segment, base)

                return OrderedTask(segment.index, execute, self._segment_pixel_cost(segment))

            def on_result(task: OrderedTask, arrays: dict[str, np.ndarray]) -> None:
                segment = self.segments[task.key]
                try:
                    if self.store is not None:
                        self.store.commit_segment(
                            segment_index=segment.index,
                            arrays=arrays,
                            metadata_snapshot=metadata_snapshot,
                            params_snapshot=params_snapshot,
                            metadata_version=self.scene.metadata_version,
                            params_version=params_version,
                            algorithm_version=self.algorithm_version,
                            segment_size=self.params.segment_size,
                        )
                    self.state_machine.mark_committed(
                        segment.index,
                        metadata_snapshot=metadata_snapshot,
                        params_snapshot=params_snapshot,
                        metadata_version=self.scene.metadata_version,
                        params_version=params_version,
                        algorithm_version=self.algorithm_version,
                    )
                    self.committed_arrays[segment.index] = arrays
                except BaseException as exc:
                    error = exc.to_dict() if isinstance(exc, WorkbenchError) else {
                        "ok": False,
                        "code": "E_IO",
                        "message": f"Checkpoint commit failed: {exc}",
                    }
                    self.state_machine.mark_failed(segment.index, error)
                    failures.append({**error, "segment_index": segment.index})
                    return
                if verbose:
                    print(f"[{self.scene.scene_id}] committed segment {segment.index + 1}/{self.total_count} ({self.progress})")
                if crash_after_commits is not None and self.committed_count >= int(crash_after_commits):
                    import sys

                    sys.stdout.flush()
                    sys.stderr.flush()
                    os._exit(91)

            def on_error(task: OrderedTask, exc: BaseException) -> None:
                segment = self.segments[task.key]
                if isinstance(exc, WorkbenchError):
                    error = exc.to_dict()
                else:
                    error = {
                        "ok": False,
                        "code": "E_SEG",
                        "message": f"Unhandled segment failure: {type(exc).__name__}: {exc}",
                        "field": f"segments[{segment.index}]",
                    }
                error["segment_index"] = segment.index
                self.state_machine.mark_failed(segment.index, error)
                failures.append(error)
                if verbose:
                    print(f"[{self.scene.scene_id}] segment {segment.index} failed: {error['code']}")

            parallelism = run_ordered_bounded(
                (make_task(segment) for segment in pending),
                max_workers=int(self.params.max_workers),
                pixel_budget=budget,
                on_result=on_result,
                on_error=on_error,
            )
            parallelism["requested_max_workers"] = int(self.params.max_workers)
            parallelism["segment_count"] = len(pending)

        complete = self.committed_count == self.total_count
        result = RunResult(
            scene_id=self.scene.scene_id,
            mode=mode,
            complete=complete,
            committed_count=self.committed_count,
            total_count=self.total_count,
            checkpoint_invalid_reason=self.checkpoint_invalid_reason,
            parallelism=parallelism,
        )
        if failures:
            codes = {failure["code"] for failure in failures}
            code = next((candidate for candidate in ("E_GAMUT", "E_PARA", "E_SEG") if candidate in codes), "E_SEG")
            raise WorkbenchError(
                code,
                "One or more segments failed; committed segments remain isolated and retryable",
                field="segments",
                details={
                    "failed_segments": failures,
                    "progress": self.progress,
                    "committed_indices": sorted(self.committed_arrays),
                    "parallelism": parallelism,
                },
            )
        if complete and output_dir is not None:
            result.stats = self.write_outputs(Path(output_dir), mode=mode, parallelism=parallelism)
            result.output_dir = Path(output_dir)
        return result

    def assemble(self) -> dict[str, np.ndarray]:
        require(
            self.committed_count == self.total_count,
            "E_STATE",
            "Cannot assemble outputs before every segment is committed",
            field="segments",
            progress=self.progress,
        )
        height, width = self.scene.pixel_size
        output_dtype = np.uint16 if self.scene.target_bit_depth > 8 else np.uint8
        arrays = {
            "fused_radiance": np.empty((height, width), dtype=np.float64),
            "cleaned_detail": np.empty((height, width), dtype=np.float64),
            "enhanced_detail": np.empty((height, width), dtype=np.float64),
            "chroma_xyz": np.empty((height, width, 3), dtype=np.float64),
            "linear_rgb": np.empty((height, width, 3), dtype=np.float64),
            "gamut_mask": np.empty((height, width), dtype=np.bool_),
            "boundary_rgb": np.empty((height, width, 3), dtype=np.float64),
            "gamut_excess": np.empty((height, width), dtype=np.float64),
            "synthetic_rgb": np.empty((height, width, 3), dtype=output_dtype),
        }
        for segment in self.segments:
            if segment.index not in self.committed_arrays:
                raise WorkbenchError("E_STATE", "Committed segment set contains a gap", field="segments", segment_index=segment.index)
            payload = self.committed_arrays[segment.index]
            for name, target in arrays.items():
                target[segment.y0 : segment.y1, segment.x0 : segment.x1] = payload[name]
        return arrays

    def _patch_errors(self, linear_rgb: np.ndarray) -> list[dict[str, Any]]:
        reports: list[dict[str, Any]] = []
        for patch in self.scene.truth["patches"]:
            y0, x0, y1, x1 = (int(value) for value in patch["bbox"])
            expected = np.asarray(patch["expected_linear_rgb"], dtype=np.float64)
            actual = linear_rgb[y0:y1, x0:x1]
            difference = actual - expected[None, None, :]
            mean_actual = np.mean(actual, axis=(0, 1))
            reports.append(
                {
                    "id": patch["id"],
                    "bbox": [y0, x0, y1, x1],
                    "expected_linear_rgb": expected.tolist(),
                    "mean_actual_linear_rgb": mean_actual.tolist(),
                    "mean_delta_rgb": (mean_actual - expected).tolist(),
                    "mae": float(np.mean(np.abs(difference))),
                    "rmse": float(np.sqrt(np.mean(difference * difference))),
                    "max_abs_error": float(np.max(np.abs(difference))),
                    "expected_in_gamut": bool(patch.get("expected_in_gamut", True)),
                }
            )
        return reports

    def _segment_error_report(self, arrays: dict[str, np.ndarray]) -> list[dict[str, Any]]:
        quality = self.scene.truth["quality"]
        noise_roi = [int(value) for value in quality["noise_roi"]]
        edge_roi = [int(value) for value in quality["edge_roi"]]
        residual = arrays["cleaned_detail"] - self.scene.clean_detail
        patch_mask = np.zeros(self.scene.pixel_size, dtype=np.bool_)
        expected_rgb_map = np.zeros((*self.scene.pixel_size, 3), dtype=np.float64)
        for patch in self.scene.truth["patches"]:
            y0, x0, y1, x1 = (int(value) for value in patch["bbox"])
            patch_mask[y0:y1, x0:x1] = True
            expected_rgb_map[y0:y1, x0:x1] = np.asarray(patch["expected_linear_rgb"], dtype=np.float64)
        patch_difference = arrays["linear_rgb"] - expected_rgb_map
        patch_abs = np.abs(patch_difference)
        reports: list[dict[str, Any]] = []
        for segment in self.segments:
            ny0, nx0 = max(segment.y0, noise_roi[0]), max(segment.x0, noise_roi[1])
            ny1, nx1 = min(segment.y1, noise_roi[2]), min(segment.x1, noise_roi[3])
            ey0, ex0 = max(segment.y0, edge_roi[0]), max(segment.x0, edge_roi[1])
            ey1, ex1 = min(segment.y1, edge_roi[2]), min(segment.x1, edge_roi[3])
            residual_std = None
            if ny1 > ny0 and nx1 > nx0:
                residual_std = float(np.std(residual[ny0:ny1, nx0:nx1]))
            edge_fidelity = None
            if ey1 - ey0 >= 3 and ex1 - ex0 >= 3:
                local = filtering_quality(
                    arrays["cleaned_detail"],
                    self.scene.clean_detail,
                    noise_roi=[ey0, ex0, ey1, ex1],
                    edge_roi=[ey0, ex0, ey1, ex1],
                )
                edge_fidelity = float(local["edge_fidelity"])
            report = {
                "segment_index": segment.index,
                "bbox": list(segment.bbox),
                "pixel_count": segment.pixel_count,
                "residual_std": residual_std,
                "edge_fidelity": edge_fidelity,
                "expected_patch_pixel_count": int(
                    np.count_nonzero(patch_mask[segment.y0 : segment.y1, segment.x0 : segment.x1])
                ),
                "patch_mae": None,
                "patch_rmse": None,
                "patch_max_abs_error": None,
                "out_of_gamut_count": int(
                    np.count_nonzero(arrays["gamut_mask"][segment.y0 : segment.y1, segment.x0 : segment.x1])
                ),
            }
            local_patch_mask = patch_mask[segment.y0 : segment.y1, segment.x0 : segment.x1]
            if np.any(local_patch_mask):
                local_difference = patch_difference[segment.y0 : segment.y1, segment.x0 : segment.x1][local_patch_mask]
                local_abs = patch_abs[segment.y0 : segment.y1, segment.x0 : segment.x1][local_patch_mask]
                report["patch_mae"] = float(np.mean(local_abs))
                report["patch_rmse"] = float(np.sqrt(np.mean(local_difference * local_difference)))
                report["patch_max_abs_error"] = float(np.max(local_abs))
            reports.append(report)
        return reports

    def write_outputs(self, output_dir: Path, *, mode: str, parallelism: dict[str, Any] | None = None) -> dict[str, Any]:
        arrays = self.assemble()
        output_dir.mkdir(parents=True, exist_ok=True)
        for name in SEGMENT_ARRAY_FIELDS:
            atomic_save_npy(output_dir / f"{name}.npy", arrays[name])

        preview = linear_rgb_to_preview_rgb8(arrays["linear_rgb"])
        _atomic_save_png(output_dir / "synthetic_preview.png", Image.fromarray(preview, mode="RGB"))

        mask = arrays["gamut_mask"]
        coordinates = np.argwhere(mask)
        boundary = arrays["boundary_rgb"][mask]
        boundary_lines = ["y,x,boundary_r,boundary_g,boundary_b"]
        for (y, x), rgb in zip(coordinates, boundary, strict=True):
            boundary_lines.append(f"{int(y)},{int(x)},{rgb[0]:.17g},{rgb[1]:.17g},{rgb[2]:.17g}")
        boundary_text = "\n".join(boundary_lines) + "\n"
        atomic_write_text(output_dir / "out_of_gamut_boundary_coordinates.csv", boundary_text)

        quality = self.scene.truth["quality"]
        filter_quality = filtering_quality(
            arrays["cleaned_detail"],
            self.scene.clean_detail,
            noise_roi=quality["noise_roi"],
            edge_roi=quality["edge_roi"],
        )
        max_residual = float(quality["max_residual_std"])
        min_edge = float(quality["min_edge_fidelity"])
        filter_quality["max_residual_std"] = max_residual
        filter_quality["min_edge_fidelity"] = min_edge
        filter_quality["residual_pass"] = filter_quality["residual_std"] < max_residual
        filter_quality["edge_pass"] = filter_quality["edge_fidelity"] >= min_edge
        filter_quality["passes_declared_bounds"] = bool(
            filter_quality["residual_pass"] and filter_quality["edge_pass"]
        )

        patch_errors = self._patch_errors(arrays["linear_rgb"])
        segment_errors = self._segment_error_report(arrays)
        gamut_count = int(np.count_nonzero(mask))
        max_excess = float(np.max(arrays["gamut_excess"])) if arrays["gamut_excess"].size else 0.0
        gamut_report = {
            "intent": self.params.gamut_intent,
            "out_of_gamut_count": gamut_count,
            "out_of_gamut_fraction": gamut_count / float(mask.size),
            "max_excess": max_excess,
            "out_of_gamut_mask_file": "gamut_mask.npy",
            "boundary_rgb_file": "boundary_rgb.npy",
            "boundary_coordinates_file": "out_of_gamut_boundary_coordinates.csv",
            "boundary_definition": (
                "Cube intersection of the neutral-to-input ray at constant luminance; "
                "for out-of-range luminance the nearest compressed-luminance cube boundary is recorded."
            ),
            "compression": "desaturation toward the achromatic axis, not channel clipping",
            "converged": True,
        }
        registration = self._ensure_base().registration
        fusion = self._ensure_base().fusion
        stats = {
            "ok": True,
            "scene_id": self.scene.scene_id,
            "mode": mode,
            "algorithm_version": self.algorithm_version,
            "metadata_version": self.scene.metadata_version,
            "params_version": self.params.version(),
            "params_snapshot": self.params.algorithm_snapshot(),
            "pixel_size": list(self.scene.pixel_size),
            "segment_size": list(self.params.segment_size),
            "segment_count": self.total_count,
            "committed_count": self.committed_count,
            "output_bit_depth": self.scene.target_bit_depth,
            "output_dtype": str(arrays["synthetic_rgb"].dtype),
            "filtering_quality": filter_quality,
            "patch_errors": patch_errors,
            "gamut": gamut_report,
            "registration": registration,
            "fusion": fusion,
            "parallelism": parallelism or {},
            "checkpoint_invalid_reason": self.checkpoint_invalid_reason,
            "content_hashes": {
                name: sha256_file(output_dir / f"{name}.npy")
                for name in SEGMENT_ARRAY_FIELDS
            },
        }
        write_json(output_dir / "segment_errors.json", {"segments": segment_errors})
        write_json(output_dir / "gamut_report.json", gamut_report)
        write_json(output_dir / "stats.json", stats)
        write_json(
            output_dir / "manifest.json",
            {
                "scene_id": self.scene.scene_id,
                "mode": mode,
                "algorithm_version": self.algorithm_version,
                "metadata_version": self.scene.metadata_version,
                "params_version": self.params.version(),
                "segment_size": list(self.params.segment_size),
                "output_bit_depth": self.scene.target_bit_depth,
                "files": [
                    f"{name}.npy" for name in SEGMENT_ARRAY_FIELDS
                ]
                + [
                    "synthetic_preview.png",
                    "out_of_gamut_boundary_coordinates.csv",
                    "gamut_report.json",
                    "segment_errors.json",
                    "stats.json",
                ],
            },
        )
        if not filter_quality["passes_declared_bounds"]:
            raise WorkbenchError(
                "E_PARA",
                "Filtered detail layer fails the truth-declared noise or edge bound",
                field="guide_radius",
                details={
                    "residual_std": filter_quality["residual_std"],
                    "max_residual_std": max_residual,
                    "edge_fidelity": filter_quality["edge_fidelity"],
                    "min_edge_fidelity": min_edge,
                },
            )
        return stats


def _atomic_save_png(path: Path, image: Image.Image) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".png", dir=path.parent)
    os.close(fd)
    tmp_path = Path(tmp_name)
    try:
        image.save(tmp_path, format="PNG")
        with tmp_path.open("rb") as handle:
            os.fsync(handle.fileno())
        os.replace(tmp_path, path)
    finally:
        tmp_path.unlink(missing_ok=True)

