from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .checkpoint import CheckpointStore, SegmentCheckpoint, SnapshotStatus, params_payload
from .config import ALGORITHM_VERSION, AlgorithmInfo, AlignmentConfig
from .drift import (
    DriftModel,
    estimate_drift_model,
    interpolation_error_bound,
    residual_bounds,
    segment_boundaries,
)
from .errors import E_BOUND, E_DIFF, E_PARA, E_STATE, SyncError
from .manifest import LoadedSession
from .markers import MarkerDetection, detect_marker_offset
from .resample import align_channel_range, channel_content_hash
from .state import SegmentState, SegmentStateMachine


ProgressCallback = Callable[[int, int, str], None]


@dataclass
class AlignmentResult:
    session_id: str
    path: str
    sample_rate: int
    reference_channel: str
    channels: dict[str, np.ndarray]
    models: dict[str, DriftModel]
    reports: dict[str, dict[str, Any]]
    segment_boundaries: np.ndarray
    config: AlignmentConfig
    layout: dict[str, Any]
    complete: bool
    algorithm: AlgorithmInfo = field(default_factory=AlgorithmInfo)
    checkpoint_status: SnapshotStatus | None = None
    diagnostics: dict[str, Any] = field(default_factory=dict)

    @property
    def low_confidence_channels(self) -> list[str]:
        return sorted(channel for channel, report in self.reports.items() if report.get("low_confidence", False))

    def channel_order(self) -> list[str]:
        return list(self.channels.keys())

    def report_dict(self) -> dict[str, Any]:
        return {
            "session_id": self.session_id,
            "path": self.path,
            "sample_rate": self.sample_rate,
            "reference_channel": self.reference_channel,
            "algorithm": {
                "name": self.algorithm.name,
                "version": self.algorithm.version,
                "checkpoint_schema": self.algorithm.checkpoint_schema,
            },
            "config": self.config.to_dict(),
            "layout": self.layout,
            "complete": self.complete,
            "checkpoint": None if self.checkpoint_status is None else self.checkpoint_status.to_dict(),
            "diagnostics": self.diagnostics,
            "low_confidence_channels": self.low_confidence_channels,
            "channels": self.reports,
        }


@dataclass
class ThreePathVerification:
    full: AlignmentResult
    realtime: AlignmentResult
    resumed: AlignmentResult
    comparisons: list[dict[str, Any]]

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": True,
            "session_id": self.full.session_id,
            "comparisons": self.comparisons,
            "full_report": self.full.report_dict(),
            "realtime_report": self.realtime.report_dict(),
            "resume_report": self.resumed.report_dict(),
        }


class AlignmentEngine:
    def __init__(
        self,
        session: LoadedSession,
        config: AlignmentConfig | None = None,
        *,
        checkpoint_path: str | Path | None = None,
        algorithm: AlgorithmInfo | None = None,
        cancel_event: threading.Event | None = None,
        progress: ProgressCallback | None = None,
    ) -> None:
        self.session = session
        self.config = config or AlignmentConfig()
        self.checkpoint_path = None if checkpoint_path is None else Path(checkpoint_path)
        self.algorithm = algorithm or AlgorithmInfo()
        self.cancel_event = cancel_event or threading.Event()
        self.progress = progress

    def _notify(self, done: int, total: int, message: str) -> None:
        if self.progress is not None:
            self.progress(done, total, message)

    def _detect_all(self) -> dict[str, list[MarkerDetection]]:
        reference = self.session.manifest.reference_channel
        markers = list(self.session.manifest.markers)
        detections: dict[str, list[MarkerDetection]] = {}
        non_reference = [channel for channel in self.session.manifest.channel_ids() if channel != reference]
        workers = max(1, min(int(self.config.max_workers), len(non_reference)))
        with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="marker-detect") as executor:
            futures = {
                executor.submit(
                    lambda channel_id=channel_id: [
                        detect_marker_offset(
                            self.session.channels[channel_id],
                            marker,
                            max_offset_samples=int(self.config.max_offset_samples),
                            source_offset=self._channel_spec(channel_id).guard_before,
                        )
                        for marker in markers
                    ]
                ): channel_id
                for channel_id in non_reference
            }
            for future in as_completed(futures):
                channel_id = futures[future]
                detections[channel_id] = future.result()
        ordered = {channel_id: detections[channel_id] for channel_id in non_reference}
        return ordered

    def _estimate_models(self, detections: dict[str, list[MarkerDetection]]) -> tuple[dict[str, DriftModel], np.ndarray]:
        boundaries = segment_boundaries(
            self.session.manifest.session_samples,
            int(self.config.segment_length),
        )
        markers = list(self.session.manifest.markers)
        reference = self.session.manifest.reference_channel
        models: dict[str, DriftModel] = {}
        for channel_id in self.session.manifest.channel_ids():
            if channel_id == reference:
                model = estimate_drift_model(
                    channel_id,
                    reference,
                    markers,
                    [MarkerDetection(marker.id, marker.ref_sample, 0.0, 1.0) for marker in markers],
                    boundaries,
                    self.config,
                )
            else:
                model = estimate_drift_model(
                    channel_id,
                    reference,
                    markers,
                    detections[channel_id],
                    boundaries,
                    self.config,
                )
            models[channel_id] = model
        return models, boundaries

    def _load_reusable(
        self,
        grouped: dict[str, list[SegmentCheckpoint]],
        boundaries: np.ndarray,
        params_version: str,
    ) -> tuple[
        dict[tuple[str, int], SegmentCheckpoint],
        dict[tuple[str, int], SegmentStateMachine],
        int,
    ]:
        reusable: dict[tuple[str, int], SegmentCheckpoint] = {}
        states: dict[tuple[str, int], SegmentStateMachine] = {}
        reused_count = 0
        for channel_id in self.session.manifest.channel_ids():
            by_index = {item.segment_index: item for item in grouped.get(channel_id, [])}
            for segment_index in range(len(boundaries) - 1):
                key = (channel_id, segment_index)
                start, stop = int(boundaries[segment_index]), int(boundaries[segment_index + 1])
                item = by_index.get(segment_index)
                valid = False
                if item is not None:
                    expected_hash = channel_content_hash(
                        self.session.channels[channel_id],
                        channel_id,
                        start,
                        stop,
                        self._channel_spec(channel_id).guard_before,
                    )
                    params_ok = self.config.preserve_committed or item.params_version == params_version
                    valid = (
                        item.state == SegmentState.COMMITTED.value
                        and item.start == start
                        and item.stop == stop
                        and item.content_hash == expected_hash
                        and item.algorithm_version == self.algorithm.version
                        and params_ok
                    )
                if valid:
                    reusable[key] = item
                    states[key] = SegmentStateMachine(channel_id, segment_index, SegmentState.COMMITTED)
                    reused_count += 1
                else:
                    states[key] = SegmentStateMachine(channel_id, segment_index, SegmentState.PENDING)
        return reusable, states, reused_count

    def _correct_channel(
        self,
        channel_id: str,
        model: DriftModel,
        start: int,
        stop: int,
        segment_index: int,
    ) -> np.ndarray:
        spec = self._channel_spec(channel_id)
        if model.is_reference:
            return np.asarray(
                self.session.channels[channel_id][
                    spec.guard_before + start : spec.guard_before + stop
                ],
                dtype=np.float64,
            )
        output, _ = align_channel_range(
            self.session.channels[channel_id],
            model,
            start,
            stop,
            self.config,
            segment_index=segment_index,
            source_offset=spec.guard_before,
        )
        return output

    def _channel_spec(self, channel_id: str):
        for spec in self.session.manifest.channels:
            if spec.id == channel_id:
                return spec
        raise SyncError(E_STATE, "channel specification is missing", channel=channel_id)

    def _mark_failed(
        self,
        states: dict[tuple[str, int], SegmentStateMachine],
        segment_index: int,
    ) -> None:
        for (channel_id, index), machine in states.items():
            if index != segment_index:
                continue
            try:
                if machine.state in {SegmentState.PENDING, SegmentState.CORRECTED}:
                    machine.transition(SegmentState.FAILED)
            except SyncError:
                pass

    def _run_segments(
        self,
        *,
        models: dict[str, DriftModel],
        boundaries: np.ndarray,
        store: CheckpointStore | None,
        reusable: dict[tuple[str, int], SegmentCheckpoint],
        states: dict[tuple[str, int], SegmentStateMachine],
        params_version: str,
        interrupt_after: int | None,
    ) -> tuple[dict[str, np.ndarray], int, bool]:
        manifest = self.session.manifest
        outputs = {
            channel_id: np.empty(manifest.session_samples, dtype=np.float64)
            for channel_id in manifest.channel_ids()
        }
        for key, item in reusable.items():
            channel_id, _ = key
            outputs[channel_id][item.start:item.stop] = item.output

        total = len(boundaries) - 1
        newly_processed = 0
        complete = True
        for segment_index in range(total):
            if self.cancel_event.is_set():
                complete = False
                self._notify(segment_index, total, "cancelled")
                break
            start, stop = int(boundaries[segment_index]), int(boundaries[segment_index + 1])
            pending = [
                channel_id
                for channel_id in manifest.channel_ids()
                if states[(channel_id, segment_index)].state != SegmentState.COMMITTED
            ]
            if not pending:
                self._notify(segment_index + 1, total, f"segment {segment_index} reused")
                continue

            segment_outputs: dict[str, np.ndarray] = {}
            try:
                workers = max(1, min(int(self.config.max_workers), len(pending)))
                with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="audio-align") as executor:
                    futures = {
                        executor.submit(
                            self._correct_channel,
                            channel_id,
                            models[channel_id],
                            start,
                            stop,
                            segment_index,
                        ): channel_id
                        for channel_id in pending
                    }
                    failure: Exception | None = None
                    for future in as_completed(futures):
                        channel_id = futures[future]
                        try:
                            segment_outputs[channel_id] = future.result()
                        except Exception as exc:
                            failure = exc
                    if failure is not None:
                        raise failure
            except Exception:
                self._mark_failed(states, segment_index)
                raise

            for channel_id in pending:
                states[(channel_id, segment_index)].transition(SegmentState.CORRECTED)

            if store is not None:
                rows = []
                for channel_id in pending:
                    model = models[channel_id]
                    rows.append(
                        SegmentCheckpoint(
                            channel_id=channel_id,
                            segment_index=segment_index,
                            start=start,
                            stop=stop,
                            params_json=params_payload(
                                model=model.to_dict(),
                                start=start,
                                stop=stop,
                                interpolation_order=int(self.config.interpolation_order),
                                params_version=params_version,
                            ),
                            content_hash=channel_content_hash(
                                self.session.channels[channel_id],
                                channel_id,
                                start,
                                stop,
                                self._channel_spec(channel_id).guard_before,
                            ),
                            params_version=params_version,
                            algorithm_version=self.algorithm.version,
                            output=segment_outputs[channel_id],
                            output_hash="",
                        )
                    )
                try:
                    store.commit_segment_batch(manifest.session_id, rows)
                except Exception:
                    self._mark_failed(states, segment_index)
                    raise

            for channel_id in pending:
                outputs[channel_id][start:stop] = segment_outputs[channel_id]
                states[(channel_id, segment_index)].transition(SegmentState.COMMITTED)
            newly_processed += 1
            self._notify(segment_index + 1, total, f"segment {segment_index} committed")

            if interrupt_after is not None and newly_processed >= interrupt_after and segment_index + 1 < total:
                complete = False
                self._notify(segment_index + 1, total, "interrupted after committed segment")
                break
        return outputs, newly_processed, complete

    def _marker_bandwidth(self) -> float:
        maximum = 0.0
        for marker in self.session.manifest.markers:
            if marker.kind == "pulse":
                maximum = max(maximum, float(marker.template.get("carrier_hz", 0.0)))
            elif marker.kind == "chirp":
                maximum = max(
                    maximum,
                    float(marker.template.get("start_hz", 0.0)),
                    float(marker.template.get("end_hz", 0.0)),
                )
        return maximum

    def _truth_rate_at(self, channel_id: str, positions: np.ndarray) -> np.ndarray:
        truth = self.session.truth["channels"][channel_id]
        knots = np.asarray(truth["knots_ref_samples"], dtype=np.float64)
        rates = np.asarray(truth["rates_ppm"], dtype=np.float64)
        indices = np.searchsorted(knots[1:], positions, side="right")
        indices = np.clip(indices, 0, len(rates) - 1)
        return rates[indices]

    def _assess(
        self,
        outputs: dict[str, np.ndarray],
        models: dict[str, DriftModel],
        boundaries: np.ndarray,
    ) -> dict[str, dict[str, Any]]:
        manifest = self.session.manifest
        markers = list(manifest.markers)
        marker_times = np.array([marker.ref_sample for marker in markers], dtype=np.float64)
        reference = manifest.reference_channel
        max_marker_hz = self._marker_bandwidth()
        interpolation_bound = interpolation_error_bound(
            self.config,
            max_marker_hz,
            manifest.nominal_sample_rate,
        )
        reports: dict[str, dict[str, Any]] = {}
        for channel_id in manifest.channel_ids():
            model = models[channel_id]
            truth = self.session.truth["channels"][channel_id]
            if channel_id == reference:
                residuals = np.zeros_like(marker_times)
                bounds = np.full_like(marker_times, float(self.config.residual_bound_margin))
                marker_scores = np.ones_like(marker_times)
                estimated_marker_offsets = np.zeros_like(marker_times)
            else:
                detections = [
                    detect_marker_offset(
                        outputs[channel_id],
                        marker,
                        max_offset_samples=int(self.config.max_offset_samples),
                    )
                    for marker in markers
                ]
                residuals = np.array([item.offset_samples for item in detections], dtype=np.float64)
                marker_scores = np.array([item.score for item in detections], dtype=np.float64)
                bounds = residual_bounds(model, marker_times, self.config, interpolation_bound)
                estimated_marker_offsets = model.marker_offsets

            cumulative_bounds = np.maximum.accumulate(bounds)
            truth_marker_offsets = np.array(
                [item["offset_samples"] for item in truth["markers"]],
                dtype=np.float64,
            )
            initial_truth = float(truth["initial_offset_samples"])
            initial_error = model.initial_offset_samples - initial_truth
            model_centres = 0.5 * (model.knots_ref_samples[:-1] + model.knots_ref_samples[1:])
            truth_rates_at_model = self._truth_rate_at(channel_id, model_centres)
            rate_errors = model.rates_ppm - truth_rates_at_model
            initial_ci = model.initial_ci_samples
            prediction_ci = model.prediction_ci_at(marker_times) if not model.is_reference else np.zeros_like(marker_times)
            low_reasons: list[str] = []
            if initial_ci > self.config.confidence_limit_samples:
                low_reasons.append(
                    f"initial offset CI95 {initial_ci:.6f} > {self.config.confidence_limit_samples:.6f}"
                )
            if prediction_ci.size and float(np.max(prediction_ci)) > self.config.confidence_limit_samples:
                low_reasons.append(
                    "marker prediction CI95 "
                    f"{float(np.max(prediction_ci)):.6f} > {self.config.confidence_limit_samples:.6f}"
                )
            if model.rates_ci_ppm.size and float(np.max(model.rates_ci_ppm)) > self.config.confidence_limit_ppm:
                low_reasons.append(
                    "rate CI95 "
                    f"{float(np.max(model.rates_ci_ppm)):.6f} ppm > {self.config.confidence_limit_ppm:.6f} ppm"
                )
            observed_bound_ok = bool(np.all(np.abs(residuals) <= bounds + 1.0e-12))
            truth_tolerance_ok = bool(
                np.all(np.abs(estimated_marker_offsets - truth_marker_offsets) <= self.config.tolerance_samples)
            )
            reports[channel_id] = {
                "channel_id": channel_id,
                "initial_offset_estimate_samples": model.initial_offset_samples,
                "initial_offset_truth_samples": initial_truth,
                "initial_offset_error_samples": initial_error,
                "initial_ci95_samples": initial_ci,
                "rates_ppm_estimate": model.rates_ppm,
                "rates_ppm_truth": truth_rates_at_model,
                "rates_ppm_error": rate_errors,
                "rates_ci95_ppm": model.rates_ci_ppm,
                "marker_ids": [marker.id for marker in markers],
                "marker_ref_samples": marker_times,
                "marker_estimated_arrival_offsets_samples": estimated_marker_offsets,
                "marker_truth_offsets_samples": truth_marker_offsets,
                "marker_offset_estimation_errors_samples": estimated_marker_offsets - truth_marker_offsets,
                "marker_residual_offsets_samples": residuals,
                "marker_residual_bounds_samples": bounds,
                "cumulative_error_bounds_samples": cumulative_bounds,
                "marker_detection_scores": marker_scores,
                "max_abs_residual_samples": float(np.max(np.abs(residuals))) if residuals.size else 0.0,
                "within_observed_bound": observed_bound_ok,
                "within_truth_tolerance": truth_tolerance_ok,
                "low_confidence": bool(low_reasons),
                "low_confidence_reasons": low_reasons,
                "bound_source": "observed_marker_spacing_confidence_and_interpolation_order",
                "fit_rmse_samples": model.residual_std_samples,
                "model_pieces": int(len(model.knots_ref_samples) - 1),
            }
        return reports

    def run(
        self,
        path: str = "full",
        *,
        interrupt_after: int | None = None,
        enforce_truth_tolerance: bool = True,
    ) -> AlignmentResult:
        self.config.validate()
        if path not in {"full", "realtime", "resume"}:
            raise SyncError(E_PARA, "path must be full, realtime or resume", field="path")
        if path != "full" and self.checkpoint_path is None:
            raise SyncError(E_PARA, "checkpoint_path is required for realtime/resume paths", field="checkpoint_path")
        if interrupt_after is not None and interrupt_after < 0:
            raise SyncError(E_PARA, "interrupt_after must be non-negative", field="interrupt_after")

        manifest = self.session.manifest
        params_version = self.config.parameter_version()
        self._notify(0, 1, "detecting observable markers")
        detections = self._detect_all()
        models, boundaries = self._estimate_models(detections)
        total_segments = len(boundaries) - 1
        self._notify(0, total_segments, "drift models estimated")

        status: SnapshotStatus | None = None
        reused_count = 0
        if path == "full":
            states = {
                (channel_id, segment_index): SegmentStateMachine(channel_id, segment_index)
                for channel_id in manifest.channel_ids()
                for segment_index in range(total_segments)
            }
            outputs, newly_processed, complete = self._run_segments(
                models=models,
                boundaries=boundaries,
                store=None,
                reusable={},
                states=states,
                params_version=params_version,
                interrupt_after=interrupt_after,
            )
        else:
            with CheckpointStore(self.checkpoint_path, self.algorithm) as store:
                status, grouped = store.load_snapshot(manifest.session_id)
                reusable, states, reused_count = self._load_reusable(
                    grouped,
                    boundaries,
                    params_version,
                )
                outputs, newly_processed, complete = self._run_segments(
                    models=models,
                    boundaries=boundaries,
                    store=store,
                    reusable=reusable,
                    states=states,
                    params_version=params_version,
                    interrupt_after=interrupt_after,
                )

        reports = self._assess(outputs, models, boundaries) if complete else {}
        result = AlignmentResult(
            session_id=manifest.session_id,
            path=path,
            sample_rate=manifest.nominal_sample_rate,
            reference_channel=manifest.reference_channel,
            channels=outputs,
            models=models,
            reports=reports,
            segment_boundaries=boundaries,
            config=self.config,
            layout={
                "name": manifest.layout.name,
                "channels": list(manifest.layout.channels),
                "speakers": list(manifest.layout.speakers),
            },
            complete=complete,
            algorithm=self.algorithm,
            checkpoint_status=status,
            diagnostics={
                "newly_processed_segments": newly_processed,
                "reused_committed_segments": reused_count,
                "params_version": params_version,
                "max_workers": int(self.config.max_workers),
                "max_buffer_bytes": int(self.config.max_buffer_bytes),
                "cancelled": self.cancel_event.is_set(),
            },
        )
        if complete and enforce_truth_tolerance:
            bad_channels = [
                channel_id
                for channel_id, report in reports.items()
                if not report["within_truth_tolerance"]
            ]
            if bad_channels:
                raise SyncError(
                    E_BOUND,
                    "estimated marker offsets exceed the declared truth tolerance",
                    channel=bad_channels[0],
                    field="tolerance_samples",
                    details={"channels": bad_channels},
                )
            bad_bounds = [
                channel_id
                for channel_id, report in reports.items()
                if not report["within_observed_bound"]
            ]
            if bad_bounds:
                channel_id = bad_bounds[0]
                report = reports[channel_id]
                residuals = np.asarray(report["marker_residual_offsets_samples"])
                bounds = np.asarray(report["marker_residual_bounds_samples"])
                violation = int(np.argmax(np.abs(residuals) > bounds))
                raise SyncError(
                    E_BOUND,
                    "measured marker residual exceeds the observed-data error bound",
                    channel=channel_id,
                    marker=report["marker_ids"][violation],
                    sample=int(report["marker_ref_samples"][violation]),
                    details={
                        "residual_samples": float(residuals[violation]),
                        "bound_samples": float(bounds[violation]),
                    },
                )
        return result

    def run_full(self, *, interrupt_after: int | None = None, enforce_truth_tolerance: bool = True) -> AlignmentResult:
        return self.run(
            "full",
            interrupt_after=interrupt_after,
            enforce_truth_tolerance=enforce_truth_tolerance,
        )

    def run_realtime(
        self,
        *,
        interrupt_after: int | None = None,
        enforce_truth_tolerance: bool = True,
    ) -> AlignmentResult:
        return self.run(
            "realtime",
            interrupt_after=interrupt_after,
            enforce_truth_tolerance=enforce_truth_tolerance,
        )

    def run_resume(
        self,
        *,
        interrupt_after: int | None = None,
        enforce_truth_tolerance: bool = True,
    ) -> AlignmentResult:
        return self.run(
            "resume",
            interrupt_after=interrupt_after,
            enforce_truth_tolerance=enforce_truth_tolerance,
        )


def _first_array_mismatch(left: np.ndarray, right: np.ndarray) -> int | None:
    if left.shape != right.shape:
        return 0
    unequal = np.flatnonzero(left != right)
    return None if unequal.size == 0 else int(unequal[0])


def compare_results(
    reference: AlignmentResult,
    candidate: AlignmentResult,
    *,
    label: str = "candidate",
) -> dict[str, Any]:
    if not reference.complete or not candidate.complete:
        raise SyncError(E_DIFF, f"{label} is incomplete and cannot be compared")
    common_order = [channel for channel in reference.channel_order() if channel in candidate.channels]
    if set(common_order) != set(reference.channel_order()) or set(common_order) != set(candidate.channel_order()):
        raise SyncError(E_DIFF, f"{label} channel set differs")
    for channel_id in common_order:
        mismatch = _first_array_mismatch(reference.channels[channel_id], candidate.channels[channel_id])
        if mismatch is not None:
            segment = int(np.searchsorted(reference.segment_boundaries[1:], mismatch, side="right"))
            raise SyncError(
                E_DIFF,
                f"first sample mismatch against {label}",
                channel=channel_id,
                sample=mismatch,
                segment=segment,
                details={
                    "reference": float(reference.channels[channel_id][mismatch]),
                    "candidate": float(candidate.channels[channel_id][mismatch]),
                },
            )
    for channel_id in common_order:
        left = reference.models[channel_id]
        right = candidate.models[channel_id]
        if (
            not np.array_equal(left.knots_ref_samples, right.knots_ref_samples)
            or not np.array_equal(left.coefficients, right.coefficients)
            or not np.array_equal(left.covariance, right.covariance)
        ):
            raise SyncError(
                E_DIFF,
                f"drift parameters differ against {label}",
                channel=channel_id,
                sample=0,
                segment=0,
                details={"field": "drift_parameters"},
            )
        for field in ("marker_residual_offsets_samples", "marker_residual_bounds_samples"):
            if not np.array_equal(
                np.asarray(reference.reports[channel_id][field]),
                np.asarray(candidate.reports[channel_id][field]),
            ):
                raise SyncError(
                    E_DIFF,
                    f"marker residual statistics differ against {label}",
                    channel=channel_id,
                    sample=0,
                    segment=0,
                    details={"field": field},
                )
    return {
        "vs": label,
        "ok": True,
        "channels": len(common_order),
        "samples_per_channel": int(reference.channels[common_order[0]].shape[0]),
        "identical_samples": True,
        "identical_drift_parameters": True,
        "identical_residual_statistics": True,
    }


def run_three_paths(
    session: LoadedSession,
    config: AlignmentConfig | None = None,
    *,
    checkpoint_root: str | Path,
) -> ThreePathVerification:
    config = config or AlignmentConfig()
    root = Path(checkpoint_root)
    root.mkdir(parents=True, exist_ok=True)

    full = AlignmentEngine(session, config).run_full()
    realtime = AlignmentEngine(
        session,
        config,
        checkpoint_path=root / "realtime.sqlite",
    ).run_realtime()

    resume_path = root / "resume.sqlite"
    if resume_path.exists():
        resume_path.unlink()
    for suffix in ("-wal", "-shm"):
        sidecar = Path(str(resume_path) + suffix)
        if sidecar.exists():
            sidecar.unlink()
    total_segments = len(segment_boundaries(session.manifest.session_samples, config.segment_length)) - 1
    interrupt_after = max(1, total_segments // 2)
    AlignmentEngine(
        session,
        config,
        checkpoint_path=resume_path,
    ).run_realtime(interrupt_after=interrupt_after)
    resumed = AlignmentEngine(
        session,
        config,
        checkpoint_path=resume_path,
    ).run_resume()

    comparisons = [
        compare_results(full, realtime, label="realtime"),
        compare_results(full, resumed, label="resume"),
    ]
    return ThreePathVerification(full=full, realtime=realtime, resumed=resumed, comparisons=comparisons)

