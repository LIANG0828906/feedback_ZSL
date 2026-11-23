"""End-to-end registration, checkpointing, and panorama composition."""

from __future__ import annotations

import hashlib
import json
import os
import threading
import time
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import cv2
import numpy as np

from .blending import BlendResult, compose_panorama
from .checkpoint import CheckpointStore
from .config import ALGORITHM_VERSION, PipelineConfig
from .dataset import Dataset, pair_content_hash
from .diagnostics import registration_error_heatmap
from .errors import ErrorCode, WorkbenchError
from .exposure import ExposureModel, estimate_exposure
from .features import FeatureSet, detect_features, match_features
from .geometry import CanvasGeometry, compute_canvas_geometry, invert_homography
from .homography import HomographyEstimate, estimate_homography
from .report import (
    GlobalRegistration,
    build_global_registration,
    make_report,
    truth_pair_homography,
)
from .state import PairRegistry, PairResult, PairState, PanoramaRecord, PanoramaState
from .strategy import (
    CandidatePair,
    StrategyDecision,
    feature_config_for_plan,
    select_strategy,
)


ProgressCallback = Callable[[dict[str, Any]], None]


@dataclass
class RunResult:
    status: str
    report: dict[str, Any]
    output_dir: Path
    checkpoint_path: Path
    panorama: np.ndarray | None
    owner: np.ndarray | None
    seams: tuple[Any, ...]
    resumed_pairs: int
    checkpoint_invalid_reason: str | None
    duration_seconds: float

    @property
    def complete(self) -> bool:
        return self.status == "completed"


@dataclass(frozen=True)
class _PairOutcome:
    pair: CandidatePair
    result: PairResult | None
    error: WorkbenchError | None
    matches: int
    feature_counts: tuple[int, int]


def _atomic_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    data = (
        json.dumps(payload, indent=2, sort_keys=True, allow_nan=False) + "\n"
    ).encode("utf-8")
    temporary = path.parent / f".{path.name}.{os.getpid()}.{threading.get_ident()}.tmp"
    with open(temporary, "wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def _atomic_png(path: Path, image: np.ndarray) -> tuple[str, str]:
    ok, encoded = cv2.imencode(".png", image, [cv2.IMWRITE_PNG_COMPRESSION, 3])
    if not ok:
        raise WorkbenchError(
            ErrorCode.IO, "failed to encode panorama PNG", filename=str(path)
        )
    data = encoded.tobytes()
    temporary = path.parent / f".{path.name}.{os.getpid()}.tmp"
    with open(temporary, "wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)
    digest = hashlib.sha256(data).hexdigest()
    return digest, "sha256(png-bytes)"


def _atomic_npz(path: Path, owner: np.ndarray, coverage: np.ndarray) -> None:
    temporary = path.parent / f".{path.name}.{os.getpid()}.tmp.npz"
    with open(temporary, "wb") as handle:
        np.savez_compressed(handle, owner=owner, coverage=coverage)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


class StitchPipeline:
    """Deterministic, restartable pipeline over one validated dataset."""

    def __init__(
        self,
        dataset: Dataset,
        config: PipelineConfig,
        output_dir: str | Path,
        *,
        checkpoint_path: str | Path | None = None,
        resume: bool = True,
    ) -> None:
        self.dataset = dataset
        self.config = config.validate()
        self.output_dir = Path(output_dir)
        self.output_dir.mkdir(parents=True, exist_ok=True)
        self.checkpoint_path = Path(
            checkpoint_path
            if checkpoint_path is not None
            else self.output_dir / "checkpoint.json"
        )
        self.run_state_path = self.output_dir / "run_state.json"
        self.resume = resume
        self.decision: StrategyDecision = select_strategy(dataset, config)
        self.feature_config = feature_config_for_plan(config.features, self.decision)
        self.parameter_version = self.config.parameter_version(
            self.decision.plan_fingerprint()
        )
        self.registry = PairRegistry()
        for pair in self.decision.candidate_pairs:
            self.registry.add(pair.pair_id, pair.image_a, pair.image_b)
        self.panorama_record = PanoramaRecord()
        self.checkpoint = CheckpointStore(
            self.checkpoint_path,
            dataset=dataset,
            parameter_version=self.parameter_version,
            plan_fingerprint=self.decision.plan_fingerprint(),
        )
        self.resumed_pairs = 0
        self.checkpoint_invalid_reason: str | None = None
        self.feature_counts: dict[int, int] = {
            index: 0 for index in self.dataset.images
        }
        self._cancel_event: threading.Event | None = None
        self._pause_event: threading.Event | None = None
        self._progress: ProgressCallback | None = None

    def _emit(self, **event: Any) -> None:
        if self._progress is None:
            return
        payload = {
            "committed": self.registry.committed_count,
            "total": self.registry.total_count,
            "panorama_state": self.panorama_record.state.value,
            **event,
        }
        self._progress(payload)

    def _restore_checkpoint(self) -> None:
        if not self.resume:
            if self.checkpoint_path.exists():
                self.checkpoint.invalidate("full recomputation requested")
            return
        loaded = self.checkpoint.load()
        if not loaded.existed:
            return
        if not loaded.valid:
            self.checkpoint_invalid_reason = loaded.invalid_reason
            self.checkpoint.invalidate(loaded.invalid_reason or "checkpoint invalid")
            self._emit(
                stage="checkpoint",
                pair_id=None,
                state="invalidated",
                message=self.checkpoint_invalid_reason,
            )
            return
        for pair_id in sorted(loaded.restored_pairs):
            record = self.registry.records.get(pair_id)
            if record is None:
                self.checkpoint_invalid_reason = "checkpoint pair is not in current plan"
                self.checkpoint.invalidate(self.checkpoint_invalid_reason)
                self._clear_committed_records()
                return
            result = loaded.restored_pairs[pair_id]
            record.begin()
            record.commit(result)
            record.matches = result.matches
            self.resumed_pairs += 1
        self._emit(
            stage="checkpoint",
            pair_id=None,
            state="restored",
            message=f"{self.resumed_pairs} committed pairs restored",
        )

    def _clear_committed_records(self) -> None:
        for record in self.registry.records.values():
            if record.state is PairState.COMMITTED:
                record.state = PairState.PENDING
                record.result = None
                record.error = None
        self.resumed_pairs = 0

    def _detect_features(self) -> tuple[dict[int, FeatureSet], dict[int, WorkbenchError]]:
        # Detect for all valid images so resumed and uninterrupted reports are
        # byte-for-byte comparable, even when every pair touching an image was
        # restored from the checkpoint.
        indices_needed = sorted(self.dataset.images)
        features: dict[int, FeatureSet] = {}
        errors: dict[int, WorkbenchError] = {}
        if not indices_needed:
            return features, errors

        def detect(index: int) -> FeatureSet:
            image = self.dataset.read_image(index, color=True)
            return detect_features(image, index, self.feature_config)

        workers = min(
            self.config.runtime.max_workers,
            self.config.runtime.max_cached_images,
            len(indices_needed),
        )
        with ThreadPoolExecutor(max_workers=max(1, workers)) as executor:
            futures = {index: executor.submit(detect, index) for index in indices_needed}
            for index in indices_needed:
                try:
                    feature_set = futures[index].result()
                    features[index] = feature_set
                    self.feature_counts[index] = feature_set.count
                except WorkbenchError as exc:
                    errors[index] = exc
                except Exception as exc:  # pragma: no cover - defensive boundary
                    errors[index] = WorkbenchError(
                        ErrorCode.MATCH,
                        f"feature extraction failed: {exc}",
                        filename=self.dataset.images[index].filename,
                        field="features",
                    )
        return features, errors

    def _compute_pair(
        self,
        pair: CandidatePair,
        features: dict[int, FeatureSet],
        feature_errors: dict[int, WorkbenchError],
    ) -> _PairOutcome:
        try:
            for index in (pair.image_a, pair.image_b):
                if index in feature_errors:
                    raise WorkbenchError(
                        feature_errors[index].code,
                        feature_errors[index].message,
                        filename=self.dataset.images[index].filename,
                        field=feature_errors[index].field,
                        details=feature_errors[index].details,
                    )
                if index not in features:
                    raise WorkbenchError(
                        ErrorCode.MATCH,
                        "features are unavailable",
                        filename=self.dataset.images[index].filename,
                        field="features",
                    )
            feature_a = features[pair.image_a]
            feature_b = features[pair.image_b]
            matches = match_features(feature_a, feature_b, self.feature_config)
            truth = truth_pair_homography(
                self.dataset, pair.image_a, pair.image_b
            )
            estimate: HomographyEstimate = estimate_homography(
                matches,
                (feature_a.image_size[0], feature_a.image_size[1]),
                self.config.homography,
                pair_key=f"{pair.pair_id}|{self.dataset.content_hash}",
                truth_homography=truth,
                truth_uses_forward_direction=True,
            )
            content_hash = pair_content_hash(
                self.dataset,
                pair.image_a,
                pair.image_b,
                self.parameter_version,
            )
            diagnostic_limit = 96
            all_points_a = estimate.inlier_points_a
            all_points_b = estimate.inlier_points_b
            all_residuals = estimate.residuals[estimate.inlier_mask]
            if len(all_points_a) > diagnostic_limit:
                diagnostic_indices = np.linspace(
                    0,
                    len(all_points_a) - 1,
                    num=diagnostic_limit,
                    dtype=np.int64,
                )
                all_points_a = all_points_a[diagnostic_indices]
                all_points_b = all_points_b[diagnostic_indices]
                all_residuals = all_residuals[diagnostic_indices]
            result = PairResult(
                pair_id=pair.pair_id,
                image_a=pair.image_a,
                image_b=pair.image_b,
                homography_a_to_b=tuple(
                    float(value) for value in estimate.homography_a_to_b.reshape(-1)
                ),
                matches=matches.count,
                inliers=estimate.inliers,
                inlier_ratio=estimate.inliers / matches.count,
                residual_median=estimate.residual_median,
                residual_p95=estimate.residual_p95,
                covariance_condition=estimate.covariance_condition,
                content_hash=content_hash,
                parameter_version=self.parameter_version,
                algorithm_version=ALGORITHM_VERSION,
                truth_corner_error=estimate.truth_corner_error,
                truth_ok=estimate.truth_ok,
                inlier_points_a=tuple(
                    (float(point[0]), float(point[1])) for point in all_points_a
                ),
                inlier_points_b=tuple(
                    (float(point[0]), float(point[1])) for point in all_points_b
                ),
                inlier_residuals=tuple(
                    float(value) for value in all_residuals
                ),
            )
            return _PairOutcome(
                pair=pair,
                result=result,
                error=None,
                matches=matches.count,
                feature_counts=(feature_a.count, feature_b.count),
            )
        except WorkbenchError as exc:
            return _PairOutcome(
                pair=pair,
                result=None,
                error=exc,
                matches=int(exc.details.get("matches", 0)),
                feature_counts=(
                    features[pair.image_a].count if pair.image_a in features else 0,
                    features[pair.image_b].count if pair.image_b in features else 0,
                ),
            )
        except Exception as exc:  # pragma: no cover - defensive pair boundary
            return _PairOutcome(
                pair=pair,
                result=None,
                error=WorkbenchError(
                    ErrorCode.HOMO,
                    f"pair registration failed: {exc}",
                    filename=f"{pair.image_a},{pair.image_b}",
                    field="pair",
                ),
                matches=0,
                feature_counts=(
                    features[pair.image_a].count if pair.image_a in features else 0,
                    features[pair.image_b].count if pair.image_b in features else 0,
                ),
            )

    def _pair_phase(self, pause_after_pairs: int | None) -> tuple[str, int]:
        if pause_after_pairs == 0:
            return "paused", 0
        pending = [
            record
            for record in self.registry.ordered()
            if record.state is not PairState.COMMITTED
        ]
        if not pending:
            return "continue", 0
        self._emit(stage="features", message="extracting features")
        features, feature_errors = self._detect_features()
        newly_committed = 0
        max_workers = self.config.runtime.max_workers
        window = max(
            max_workers,
            min(self.config.runtime.max_buffered_pairs, len(pending)),
        )
        process_position = 0
        futures: dict[str, Future[_PairOutcome]] = {}
        stop_status = "continue"

        with ThreadPoolExecutor(max_workers=max_workers) as executor:
            while process_position < len(pending) or futures:
                while (
                    process_position < len(pending)
                    and len(futures) < window
                    and stop_status == "continue"
                ):
                    record = pending[process_position]
                    if record.state is PairState.PENDING:
                        record.begin()
                    elif record.state is PairState.FAILED:
                        record.begin()
                    elif record.state is not PairState.REGISTERING:
                        raise WorkbenchError(
                            ErrorCode.STATE,
                            f"pair {record.pair_id} cannot be scheduled from {record.state.value}",
                        )
                    pair = next(
                        candidate
                        for candidate in self.decision.candidate_pairs
                        if candidate.pair_id == record.pair_id
                    )
                    futures[record.pair_id] = executor.submit(
                        self._compute_pair, pair, features, feature_errors
                    )
                    process_position += 1

                if not futures:
                    break
                next_record = pending[
                    next(
                        index
                        for index, record in enumerate(pending)
                        if record.pair_id in futures
                    )
                ]
                future = futures.pop(next_record.pair_id)
                try:
                    outcome = future.result()
                except Exception as exc:  # pragma: no cover - future boundary
                    outcome = _PairOutcome(
                        pair=CandidatePair(
                            next_record.pair_id,
                            next_record.image_a,
                            next_record.image_b,
                        ),
                        result=None,
                        error=WorkbenchError(
                            ErrorCode.HOMO,
                            f"worker failed: {exc}",
                            field="pair.worker",
                        ),
                        matches=0,
                        feature_counts=(0, 0),
                    )
                next_record.matches = outcome.matches
                if outcome.result is not None:
                    next_record.commit(outcome.result)
                    self.checkpoint.commit_pair(outcome.result)
                    newly_committed += 1
                    state = "committed"
                else:
                    error = outcome.error or WorkbenchError(
                        ErrorCode.HOMO, "pair failed without an error"
                    )
                    next_record.fail(error)
                    state = "failed"
                self._emit(
                    stage="pair",
                    pair_id=next_record.pair_id,
                    image_a=next_record.image_a,
                    image_b=next_record.image_b,
                    state=state,
                    error=error.to_dict() if outcome.error else None,
                )
                if outcome.result is not None and outcome.result.truth_ok is False:
                    self._emit(
                        stage="warning",
                        pair_id=next_record.pair_id,
                        state="truth_tolerance",
                        message=(
                            "estimated homography exceeds truth corner tolerance "
                            f"({outcome.result.truth_corner_error:.3f}px)"
                        ),
                    )

                if self._cancel_event is not None and self._cancel_event.is_set():
                    stop_status = "cancelled"
                    break
                if self._pause_event is not None and self._pause_event.is_set():
                    stop_status = "paused"
                    break
                if (
                    pause_after_pairs is not None
                    and newly_committed >= pause_after_pairs
                ):
                    stop_status = "paused"
                    break
            for future in futures.values():
                future.cancel()
        return stop_status, newly_committed

    def _write_report(self, report: dict[str, Any]) -> None:
        _atomic_json(self.output_dir / "report.json", report)

    def _mark_run_state(self, status: str) -> None:
        _atomic_json(
            self.run_state_path,
            {
                "status": status,
                "parameter_version": self.parameter_version,
                "algorithm_version": ALGORITHM_VERSION,
                "dataset_hash": self.dataset.content_hash,
            },
        )

    def _prepare_run_outputs(self) -> None:
        for name in (
            "report.json",
            "panorama.png",
            "owner.npz",
            "seams.json",
            "registration_error_heatmap.png",
        ):
            try:
                (self.output_dir / name).unlink(missing_ok=True)
            except OSError as exc:
                raise WorkbenchError(
                    ErrorCode.IO,
                    f"failed to clear stale output {name}: {exc}",
                    filename=str(self.output_dir / name),
                ) from exc

    def run(
        self,
        *,
        pause_after_pairs: int | None = None,
        cancel_event: threading.Event | None = None,
        pause_event: threading.Event | None = None,
        progress: ProgressCallback | None = None,
    ) -> RunResult:
        self._mark_run_state("running")
        try:
            result = self._run_impl(
                pause_after_pairs=pause_after_pairs,
                cancel_event=cancel_event,
                pause_event=pause_event,
                progress=progress,
            )
        except BaseException:
            self._mark_run_state("error")
            raise
        self._mark_run_state(result.status)
        return result

    def _run_impl(
        self,
        *,
        pause_after_pairs: int | None = None,
        cancel_event: threading.Event | None = None,
        pause_event: threading.Event | None = None,
        progress: ProgressCallback | None = None,
    ) -> RunResult:
        started = time.perf_counter()
        if pause_after_pairs is not None and pause_after_pairs < 0:
            raise WorkbenchError(
                ErrorCode.PARA,
                "pause_after_pairs cannot be negative",
                field="runtime.pause_after_pairs",
            )
        self._cancel_event = cancel_event
        self._pause_event = pause_event
        self._progress = progress
        previous_threads = cv2.getNumThreads()
        cv2.setNumThreads(self.config.runtime.deterministic_cv_threads)
        try:
            self._restore_checkpoint()
            self._prepare_run_outputs()
            initial_registration = build_global_registration(
                self.registry,
                self.dataset.base_index,
                sorted(self.dataset.images),
            )
            self._write_report(
                make_report(
                    dataset=self.dataset,
                    config=self.config,
                    decision=self.decision,
                    parameter_version=self.parameter_version,
                    registry=self.registry,
                    global_registration=initial_registration,
                    geometry=None,
                    panorama_state=PanoramaState.PENDING,
                    status="running",
                    feature_counts=self.feature_counts,
                    panorama_info=None,
                    warnings=[],
                )
            )
            self._emit(
                stage="start",
                message=(
                    f"mode={self.decision.mode} budget={self.decision.actual_feature_budget} "
                    f"candidates={self.decision.candidate_count}"
                ),
            )
            stop_status, newly_committed = self._pair_phase(pause_after_pairs)
            if stop_status in {"paused", "cancelled"}:
                status = "paused" if stop_status == "paused" else "cancelled"
                report = make_report(
                    dataset=self.dataset,
                    config=self.config,
                    decision=self.decision,
                    parameter_version=self.parameter_version,
                    registry=self.registry,
                    global_registration=build_global_registration(
                        self.registry,
                        self.dataset.base_index,
                        sorted(self.dataset.images),
                    ),
                    geometry=None,
                    panorama_state=self.panorama_record.state,
                    status=status,
                    feature_counts=self.feature_counts,
                    panorama_info=None,
                    warnings=[],
                )
                self._write_report(report)
                self._emit(
                    stage="pause" if status == "paused" else "cancel",
                    message=(
                        f"{self.registry.committed_count} committed pairs persisted; "
                        "panorama not composed"
                    ),
                )
                return RunResult(
                    status=status,
                    report=report,
                    output_dir=self.output_dir,
                    checkpoint_path=self.checkpoint_path,
                    panorama=None,
                    owner=None,
                    seams=(),
                    resumed_pairs=self.resumed_pairs,
                    checkpoint_invalid_reason=self.checkpoint_invalid_reason,
                    duration_seconds=time.perf_counter() - started,
                )

            global_registration = build_global_registration(
                self.registry,
                self.dataset.base_index,
                sorted(self.dataset.images),
            )
            self.panorama_record.begin()
            self._emit(stage="panorama", state="compositing")
            if global_registration.missing_images:
                error = WorkbenchError(
                    ErrorCode.HOMO,
                    "not all images could be connected to the base image",
                    field="registration.missing_images",
                    details={"images": list(global_registration.missing_images)},
                )
                self.panorama_record.fail(error)
                report = make_report(
                    dataset=self.dataset,
                    config=self.config,
                    decision=self.decision,
                    parameter_version=self.parameter_version,
                    registry=self.registry,
                    global_registration=global_registration,
                    geometry=None,
                    panorama_state=self.panorama_record.state,
                    status="failed",
                    feature_counts=self.feature_counts,
                    panorama_info={
                        "state": PanoramaState.FAILED.value,
                        "error": error.to_dict(),
                    },
                    warnings=[],
                )
                self._write_report(report)
                return RunResult(
                    status="failed",
                    report=report,
                    output_dir=self.output_dir,
                    checkpoint_path=self.checkpoint_path,
                    panorama=None,
                    owner=None,
                    seams=(),
                    resumed_pairs=self.resumed_pairs,
                    checkpoint_invalid_reason=self.checkpoint_invalid_reason,
                    duration_seconds=time.perf_counter() - started,
                )

            image_sizes = {
                index: (meta.width, meta.height)
                for index, meta in self.dataset.images.items()
            }
            estimated_geometry = compute_canvas_geometry(
                global_registration.homographies_to_base, image_sizes
            )
            width_difference = abs(
                estimated_geometry.width - self.dataset.theoretical_width
            )
            height_difference = abs(
                estimated_geometry.height - self.dataset.theoretical_height
            )
            warnings: list[str] = []
            if (
                width_difference > self.config.blend.canvas_reconcile_px
                or height_difference > self.config.blend.canvas_reconcile_px
            ):
                error = WorkbenchError(
                    ErrorCode.HOMO,
                    "estimated canvas differs from theoretical canvas beyond tolerance",
                    field="canvas",
                    details={
                        "estimated_width": estimated_geometry.width,
                        "estimated_height": estimated_geometry.height,
                        "theoretical_width": self.dataset.theoretical_width,
                        "theoretical_height": self.dataset.theoretical_height,
                        "tolerance": self.config.blend.canvas_reconcile_px,
                    },
                )
                self.panorama_record.fail(error)
                report = make_report(
                    dataset=self.dataset,
                    config=self.config,
                    decision=self.decision,
                    parameter_version=self.parameter_version,
                    registry=self.registry,
                    global_registration=global_registration,
                    geometry=estimated_geometry,
                    panorama_state=self.panorama_record.state,
                    status="failed",
                    feature_counts=self.feature_counts,
                    panorama_info={
                        "state": PanoramaState.FAILED.value,
                        "error": error.to_dict(),
                    },
                    warnings=warnings,
                )
                self._write_report(report)
                return RunResult(
                    status="failed",
                    report=report,
                    output_dir=self.output_dir,
                    checkpoint_path=self.checkpoint_path,
                    panorama=None,
                    owner=None,
                    seams=(),
                    resumed_pairs=self.resumed_pairs,
                    checkpoint_invalid_reason=self.checkpoint_invalid_reason,
                    duration_seconds=time.perf_counter() - started,
                )
            if width_difference or height_difference:
                warnings.append(
                    "estimated canvas reconciled to theoretical dimensions "
                    f"({estimated_geometry.width}x{estimated_geometry.height} -> "
                    f"{self.dataset.theoretical_width}x{self.dataset.theoretical_height})"
                )
            geometry = CanvasGeometry(
                origin_x=estimated_geometry.origin_x,
                origin_y=estimated_geometry.origin_y,
                width=self.dataset.theoretical_width,
                height=self.dataset.theoretical_height,
                raw_min_x=estimated_geometry.raw_min_x,
                raw_min_y=estimated_geometry.raw_min_y,
                raw_max_x=estimated_geometry.raw_max_x,
                raw_max_y=estimated_geometry.raw_max_y,
            )
            homographies_to_canvas = {
                index: geometry.translation @ homography
                for index, homography in global_registration.homographies_to_base.items()
            }
            try:
                exposure_model = estimate_exposure(
                    self.dataset,
                    homographies_to_canvas,
                    (geometry.width, geometry.height),
                    self.config.exposure,
                )
                blend_result: BlendResult = compose_panorama(
                    self.dataset,
                    homographies_to_canvas,
                    geometry,
                    exposure_model,
                    self.config.blend,
                    self.config.exposure,
                    max_cached_images=self.config.runtime.max_cached_images,
                    pair_id_by_image=global_registration.pair_id_by_image,
                )
            except WorkbenchError as exc:
                self.panorama_record.fail(exc)
                report = make_report(
                    dataset=self.dataset,
                    config=self.config,
                    decision=self.decision,
                    parameter_version=self.parameter_version,
                    registry=self.registry,
                    global_registration=global_registration,
                    geometry=geometry,
                    panorama_state=self.panorama_record.state,
                    status="failed",
                    feature_counts=self.feature_counts,
                    panorama_info={
                        "state": PanoramaState.FAILED.value,
                        "error": exc.to_dict(),
                    },
                    warnings=warnings,
                )
                self._write_report(report)
                return RunResult(
                    status="failed",
                    report=report,
                    output_dir=self.output_dir,
                    checkpoint_path=self.checkpoint_path,
                    panorama=None,
                    owner=None,
                    seams=(),
                    resumed_pairs=self.resumed_pairs,
                    checkpoint_invalid_reason=self.checkpoint_invalid_reason,
                    duration_seconds=time.perf_counter() - started,
                )
            except ValueError as exc:
                error = WorkbenchError(
                    ErrorCode.CLIP,
                    str(exc),
                    field="blend.seam_gradient_limit",
                )
                self.panorama_record.fail(error)
                report = make_report(
                    dataset=self.dataset,
                    config=self.config,
                    decision=self.decision,
                    parameter_version=self.parameter_version,
                    registry=self.registry,
                    global_registration=global_registration,
                    geometry=geometry,
                    panorama_state=self.panorama_record.state,
                    status="failed",
                    feature_counts=self.feature_counts,
                    panorama_info={
                        "state": PanoramaState.FAILED.value,
                        "error": error.to_dict(),
                    },
                    warnings=warnings,
                )
                self._write_report(report)
                return RunResult(
                    status="failed",
                    report=report,
                    output_dir=self.output_dir,
                    checkpoint_path=self.checkpoint_path,
                    panorama=None,
                    owner=None,
                    seams=(),
                    resumed_pairs=self.resumed_pairs,
                    checkpoint_invalid_reason=self.checkpoint_invalid_reason,
                    duration_seconds=time.perf_counter() - started,
                )

            panorama_path = self.output_dir / "panorama.png"
            panorama_hash, hash_kind = _atomic_png(panorama_path, blend_result.panorama)
            _atomic_npz(
                self.output_dir / "owner.npz",
                blend_result.owner,
                blend_result.coverage,
            )
            seams_path = self.output_dir / "seams.json"
            _atomic_json(
                seams_path,
                {
                    "seams": [
                        {"pair_id": pair_id, **seam.to_dict()}
                        for pair_id, seam in blend_result.seams
                    ]
                },
            )
            heatmap = registration_error_heatmap(
                self.dataset,
                self.registry.ordered(),
                global_registration.homographies_to_base,
                (geometry.width, geometry.height),
                (geometry.origin_x, geometry.origin_y),
            )
            heatmap_path = self.output_dir / "registration_error_heatmap.png"
            ok, encoded_heatmap = cv2.imencode(
                ".png", heatmap, [cv2.IMWRITE_PNG_COMPRESSION, 3]
            )
            if ok:
                heatmap_path.write_bytes(encoded_heatmap.tobytes())
            panorama_info: dict[str, Any] = {
                "state": PanoramaState.COMMITTED.value,
                "width": int(blend_result.panorama.shape[1]),
                "height": int(blend_result.panorama.shape[0]),
                "theoretical_width": self.dataset.theoretical_width,
                "theoretical_height": self.dataset.theoretical_height,
                "matches_theoretical": (
                    blend_result.panorama.shape[1] == self.dataset.theoretical_width
                    and blend_result.panorama.shape[0] == self.dataset.theoretical_height
                ),
                "estimated_width": estimated_geometry.width,
                "estimated_height": estimated_geometry.height,
                "canvas_reconciled": bool(width_difference or height_difference),
                "origin_x": geometry.origin_x,
                "origin_y": geometry.origin_y,
                "output_path": "panorama.png",
                "owner_path": "owner.npz",
                "seams_path": "seams.json",
                "error_heatmap_path": "registration_error_heatmap.png",
                "output_hash": panorama_hash,
                "hash_kind": hash_kind,
                "seam_count": blend_result.summary()["seam_count"],
                "seam_gradient_p95": blend_result.seam_gradient_p95,
                "seam_gradient_max": blend_result.seam_gradient_max,
                "seam_gradient_limit": self.config.blend.seam_gradient_limit,
                "seam_gradient_ok": (
                    blend_result.seam_gradient_p95
                    <= self.config.blend.seam_gradient_limit
                ),
                "exposure": exposure_model.to_dict(),
                "blend": blend_result.summary(),
                "max_workers": self.config.runtime.max_workers,
                "max_cached_images": self.config.runtime.max_cached_images,
            }
            report = make_report(
                dataset=self.dataset,
                config=self.config,
                decision=self.decision,
                parameter_version=self.parameter_version,
                registry=self.registry,
                global_registration=global_registration,
                geometry=geometry,
                panorama_state=PanoramaState.COMMITTED,
                status="completed",
                feature_counts=self.feature_counts,
                panorama_info=panorama_info,
                warnings=warnings,
            )
            self.panorama_record.commit(panorama_info)
            self.checkpoint.commit_panorama(panorama_info)
            self._write_report(report)
            self._emit(
                stage="complete",
                state="committed",
                message=(
                    f"panorama {blend_result.panorama.shape[1]}x"
                    f"{blend_result.panorama.shape[0]} committed"
                ),
            )
            return RunResult(
                status="completed",
                report=report,
                output_dir=self.output_dir,
                checkpoint_path=self.checkpoint_path,
                panorama=blend_result.panorama,
                owner=blend_result.owner,
                seams=blend_result.seams,
                resumed_pairs=self.resumed_pairs,
                checkpoint_invalid_reason=self.checkpoint_invalid_reason,
                duration_seconds=time.perf_counter() - started,
            )
        finally:
            cv2.setNumThreads(previous_threads)
