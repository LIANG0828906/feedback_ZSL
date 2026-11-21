"""Incremental calibration orchestration, publication and crash recovery."""

from __future__ import annotations

import threading
from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import Any, Callable, Sequence

import numpy as np

from .alignment import alignment_difference, apply_alignment, detect_arrivals
from .errors import (
    CalibrationError,
    E_BUSY,
    E_FILTER_INVALID,
    E_PARAMS_INVALID,
    E_RECOVERY_INVALID,
    E_VERSION_STALE,
    E_WAV_INVALID,
    OperationCancelled,
)
from .filters import (
    affected_smoothing_bands,
    affected_target_bands,
    design_compensation_filter,
    smooth_band_powers,
    validate_filter_request,
)
from .hashing import sha256_file, sha256_obj
from .models import (
    AlignmentResult,
    BandSpec,
    CalibrationParams,
    CalibrationState,
    ChannelAlignment,
    ChannelComputation,
    ConsistencyAlert,
    all_node_keys,
)
from .propagation import compute_channel_errors
from .recovery import RecoverySnapshot, SnapshotManager, SnapshotRun
from .spectral import estimate_channel_spectrum, make_bands
from .versioning import StateStore, version_diff
from .wavio import WavData, read_wav_16

ProgressCallback = Callable[[dict[str, Any]], None]


@dataclass(frozen=True)
class CancellationToken:
    event: threading.Event = field(default_factory=threading.Event)

    @property
    def cancelled(self) -> bool:
        return self.event.is_set()

    def cancel(self) -> None:
        self.event.set()

    def checkpoint(self, stage: str, channel: int | None = None, band: int | None = None) -> None:
        if self.event.is_set():
            raise OperationCancelled(f"cancelled at {stage} checkpoint")


@dataclass
class AsyncTask:
    token: CancellationToken
    thread: threading.Thread
    done: threading.Event = field(default_factory=threading.Event)
    result: CalibrationState | None = None
    error: BaseException | None = None

    def cancel(self) -> None:
        self.token.cancel()

    def wait(self, timeout: float | None = None) -> CalibrationState:
        if not self.done.wait(timeout):
            raise TimeoutError("calibration task did not finish before timeout")
        if self.error is not None:
            raise self.error
        if self.result is None:
            raise RuntimeError("calibration task finished without a state")
        return self.result


@dataclass(frozen=True)
class RecoveryReport:
    state: CalibrationState
    run_id: str | None
    reused_channels: tuple[int, ...]
    recomputed_channels: tuple[int, ...]
    already_published: bool


@dataclass(frozen=True)
class _IncrementalPlan:
    alignment_changed: bool
    spectral_changed: bool
    smoothing_bands: tuple[int, ...]
    filter_bands: tuple[int, ...]
    error_bands: tuple[int, ...]
    consistency_bands: tuple[int, ...]
    affected_channels: tuple[int, ...]
    affected_bands: tuple[int, ...]


class CalibrationEngine:
    """One-measurement calibration coordinator.

    A measurement is represented by one WAV file and one workspace. Only one
    recomputation may run at a time; publications are immutable whole-state
    replacements protected by a parent hash.
    """

    def __init__(self, workspace_root: str | Path, wav_path: str | Path | None = None) -> None:
        self.workspace_root = Path(workspace_root)
        self.workspace_root.mkdir(parents=True, exist_ok=True)
        self.state_store = StateStore(self.workspace_root / "state")
        self.snapshot_manager = SnapshotManager(self.workspace_root)
        self._wav_path: Path | None = None
        self._wav_cache: WavData | None = None
        self._wav_hash: str | None = None
        self._busy_lock = threading.Lock()
        self._cancel_token: CancellationToken | None = None
        self._active_task: AsyncTask | None = None
        if wav_path is not None:
            self.set_source(wav_path)

    @property
    def wav_path(self) -> Path | None:
        return self._wav_path

    @property
    def active_task(self) -> AsyncTask | None:
        return self._active_task

    def set_source(self, wav_path: str | Path) -> WavData:
        path = Path(wav_path).resolve()
        wav = read_wav_16(path)
        self._wav_path = path
        self._wav_cache = wav
        self._wav_hash = sha256_file(path)
        return wav

    def _load_source(self) -> tuple[WavData, str]:
        if self._wav_path is None:
            raise CalibrationError(E_WAV_INVALID, "no WAV source has been selected")
        current_hash = sha256_file(self._wav_path)
        if self._wav_cache is None or self._wav_hash != current_hash:
            self._wav_cache = read_wav_16(self._wav_path)
            self._wav_hash = current_hash
        return self._wav_cache, current_hash

    @staticmethod
    def parse_params(payload: dict[str, Any]) -> CalibrationParams:
        try:
            params = CalibrationParams.from_dict(payload)
            validate_filter_request(params)
            return params
        except CalibrationError:
            raise
        except (TypeError, ValueError, KeyError) as exc:
            text = str(exc)
            code = E_FILTER_INVALID if ("filter" in text or "target" in text) else E_PARAMS_INVALID
            raise CalibrationError(code, "invalid calibration parameters", {"reason": text}) from exc

    def current_state(self) -> CalibrationState | None:
        return self.state_store.latest()

    def _validate_measurement(self, wav: WavData, params: CalibrationParams) -> None:
        if params.alignment.reference_channel >= wav.channel_count:
            raise CalibrationError(
                E_PARAMS_INVALID,
                "reference channel is outside the WAV layout",
                {"reference_channel": params.alignment.reference_channel, "channels": wav.channel_count},
            )
        validate_filter_request(params)

    def _plan(
        self,
        old: CalibrationState | None,
        params: CalibrationParams,
        bands: Sequence[BandSpec],
    ) -> _IncrementalPlan:
        all_bands = tuple(range(len(bands)))
        all_channels = tuple(range(len(self._wav_cache.channel_names))) if self._wav_cache else ()
        if old is None:
            return _IncrementalPlan(
                alignment_changed=True,
                spectral_changed=True,
                smoothing_bands=all_bands,
                filter_bands=all_bands,
                error_bands=all_bands,
                consistency_bands=all_bands,
                affected_channels=all_channels,
                affected_bands=all_bands,
            )
        old_bands = tuple(old.bands)
        if len(old_bands) != len(bands):
            return _IncrementalPlan(
                alignment_changed=True,
                spectral_changed=True,
                smoothing_bands=all_bands,
                filter_bands=all_bands,
                error_bands=all_bands,
                consistency_bands=all_bands,
                affected_channels=all_channels,
                affected_bands=all_bands,
            )

        alignment_changed = old.params.alignment != params.alignment
        spectral_changed = alignment_changed or old.params.spectral != params.spectral or old_bands != tuple(bands)
        smoothing_changed = old.params.smoothing_fraction != params.smoothing_fraction
        target_changed = old.params.target_curve != params.target_curve
        filter_parameters_changed = (
            old.params.filter_order != params.filter_order
            or old.params.max_boost_db != params.max_boost_db
            or old.params.max_cut_db != params.max_cut_db
        )
        threshold_changed = old.params.error_threshold_db != params.error_threshold_db
        tolerance_changed = old.params.consistency_tolerance_db != params.consistency_tolerance_db

        if spectral_changed:
            smoothing_bands = all_bands
        elif smoothing_changed:
            smoothing_bands = affected_smoothing_bands(
                bands,
                old.params.smoothing_fraction,
                params.smoothing_fraction,
            )
        else:
            smoothing_bands = ()

        if spectral_changed or filter_parameters_changed:
            filter_bands = all_bands
        else:
            selected: set[int] = set(smoothing_bands)
            if target_changed:
                selected.update(affected_target_bands(bands, old.params.target_curve, params.target_curve))
            filter_bands = tuple(sorted(selected))

        if threshold_changed or spectral_changed:
            error_bands = all_bands
        else:
            error_bands = tuple(sorted(set(smoothing_bands) | set(filter_bands)))

        if spectral_changed or tolerance_changed:
            consistency_bands = all_bands
        else:
            consistency_bands = smoothing_bands

        affected_bands = tuple(
            sorted(set(smoothing_bands) | set(filter_bands) | set(error_bands) | set(consistency_bands))
        )
        affected_channels = all_channels if affected_bands else ()
        return _IncrementalPlan(
            alignment_changed=alignment_changed,
            spectral_changed=spectral_changed,
            smoothing_bands=smoothing_bands,
            filter_bands=filter_bands,
            error_bands=error_bands,
            consistency_bands=consistency_bands,
            affected_channels=affected_channels,
            affected_bands=affected_bands,
        )

    @staticmethod
    def _alignment_from_state(state: CalibrationState) -> AlignmentResult:
        return AlignmentResult(
            reference_channel=state.params.alignment.reference_channel,
            reference_arrival=state.channels[state.params.alignment.reference_channel].alignment.arrival_sample,
            channels=tuple(channel.alignment for channel in state.channels),
        )

    @staticmethod
    def _resume_alignment(
        detected: AlignmentResult,
        params: CalibrationParams,
        completed: dict[int, ChannelComputation],
    ) -> AlignmentResult:
        reference_index = params.alignment.reference_channel
        reference = completed.get(reference_index)
        if reference is not None:
            reference_alignment = reference.alignment
        else:
            reference_alignment = detected.channels[reference_index]
        channels: list[ChannelAlignment] = []
        for channel in detected.channels:
            completed_channel = completed.get(channel.channel_index)
            if completed_channel is not None:
                channels.append(completed_channel.alignment)
                continue
            gain = (
                reference_alignment.direct_peak / channel.direct_peak
                if channel.direct_peak > 0.0
                else 1.0
            )
            channels.append(
                replace(
                    channel,
                    offset=channel.arrival_sample - reference_alignment.arrival_sample,
                    gain=float(gain),
                )
            )
        return AlignmentResult(
            reference_channel=reference_index,
            reference_arrival=reference_alignment.arrival_sample,
            channels=tuple(channels),
        )

    @staticmethod
    def _detect_consistency(
        channel_names: Sequence[str],
        channels: Sequence[ChannelComputation],
        bands: Sequence[BandSpec],
        tolerance_db: float,
    ) -> dict[int, tuple[ConsistencyAlert, ...]]:
        matrix = np.asarray([channel.smoothing.power_db for channel in channels], dtype=np.float64)
        output: dict[int, tuple[ConsistencyAlert, ...]] = {}
        for band_index, band in enumerate(bands):
            values = matrix[:, band_index]
            median = float(np.median(values))
            alerts: list[ConsistencyAlert] = []
            for channel_index, value in enumerate(values):
                deviation = float(value - median)
                if abs(deviation) > tolerance_db:
                    alerts.append(
                        ConsistencyAlert(
                            channel_index=channel_index,
                            channel_name=channel_names[channel_index],
                            band_index=band_index,
                            center_hz=band.center_hz,
                            value_db=float(value),
                            peer_median_db=median,
                            deviation_db=deviation,
                            tolerance_db=float(tolerance_db),
                        )
                    )
            output[band_index] = tuple(alerts)
        return output

    @staticmethod
    def _node_payload(key: str, channels: Sequence[ChannelComputation], bands: Sequence[BandSpec]) -> Any:
        prefix, channel_name, *parts = key.split(":")
        if prefix == "consistency":
            band_index = int(channel_name)
            # The caller replaces this marker with the final alert set.
            return {"band_index": band_index}
        channel_index = next(
            (index for index, channel in enumerate(channels) if channel.alignment.channel_name == channel_name),
            None,
        )
        if channel_index is None:
            return None
        channel = channels[channel_index]
        if prefix == "align":
            return channel.alignment.to_dict()
        if prefix == "filter":
            design = channel.filter
            return {
                "order": design.order,
                "coefficients": design.coefficients,
                "group_delay_samples": design.group_delay_samples,
                "desired_gain_db": design.desired_gain_db,
                "truncation_rel": design.truncation_rel,
            }
        band_index = int(parts[0])
        if prefix == "spectral":
            return channel.spectral.bands[band_index].to_dict()
        if prefix == "smooth":
            return {
                "power_db": channel.smoothing.power_db[band_index],
                "weights": channel.smoothing.weights[band_index],
            }
        if prefix == "error":
            return channel.errors[band_index].to_dict()
        return None

    def _compute_and_publish(
        self,
        params: CalibrationParams,
        expected_parent_hash: str | None,
        run: SnapshotRun | None,
        token: CancellationToken,
        progress: ProgressCallback | None,
        resume: RecoverySnapshot | None = None,
    ) -> CalibrationState:
        wav, source_hash = self._load_source()
        self._validate_measurement(wav, params)
        base_state = self.state_store.latest()
        base_hash = None if base_state is None else base_state.content_hash
        if expected_parent_hash != base_hash:
            raise CalibrationError(
                E_VERSION_STALE,
                "workspace parent version changed before submission",
                {"expected_parent_hash": expected_parent_hash, "actual_parent_hash": base_hash},
            )
        if base_state is not None and base_state.source_hash != source_hash:
            raise CalibrationError(
                E_VERSION_STALE,
                "WAV source hash differs from the published measurement",
                {"expected_source_hash": base_state.source_hash, "actual_source_hash": source_hash},
            )

        bands = make_bands(params.spectral, wav.sample_rate)
        plan = self._plan(base_state, params, bands)
        completed = {} if resume is None else resume.completed_channels

        token.checkpoint("prepare")
        if resume is None:
            if plan.alignment_changed or base_state is None:
                alignment_result = detect_arrivals(wav, params.alignment)
            else:
                alignment_result = self._alignment_from_state(base_state)
        else:
            raw_alignment = detect_arrivals(wav, params.alignment)
            alignment_result = self._resume_alignment(raw_alignment, params, completed)

        aligned = apply_alignment(wav, alignment_result)
        old_alignment = None if base_state is None else self._alignment_from_state(base_state)
        align_diff = alignment_difference(old_alignment, alignment_result)

        channel_results: list[ChannelComputation | None] = [None] * wav.channel_count
        recomputed: set[str] = set()
        reused_count = 0

        for channel_index in range(wav.channel_count):
            token.checkpoint("channel", channel=channel_index)
            channel_name = wav.channel_names[channel_index]
            old_channel = (
                base_state.channels[channel_index]
                if base_state is not None and channel_index < len(base_state.channels)
                else None
            )
            completed_channel = completed.get(channel_index)
            if completed_channel is not None:
                channel_results[channel_index] = completed_channel
                reused_count += 1
                if progress:
                    progress(
                        {
                            "stage": "recovery_reuse_channel",
                            "channel": channel_index,
                            "completed": channel_index + 1,
                            "total": wav.channel_count,
                        }
                    )
                continue

            if base_state is None or plan.alignment_changed:
                recomputed.add(f"align:{channel_name}")

            if base_state is None or plan.spectral_changed:
                token.checkpoint("spectral", channel=channel_index, band=0)
                spectral = estimate_channel_spectrum(
                    aligned[channel_index],
                    wav.sample_rate,
                    bands,
                    params.spectral,
                    channel_index,
                    checkpoint=lambda band: token.checkpoint("spectral", channel=channel_index, band=band),
                )
                recomputed.update(f"spectral:{channel_name}:{band.index}" for band in bands)
            else:
                assert old_channel is not None
                spectral = old_channel.spectral

            if base_state is None or plan.spectral_changed:
                smoothing_bands = tuple(range(len(bands)))
            else:
                smoothing_bands = plan.smoothing_bands
            if smoothing_bands:
                smoothing = smooth_band_powers(
                    bands,
                    [band.power_db for band in spectral.bands],
                    params.smoothing_fraction,
                    channel_index=channel_index,
                    band_indices=smoothing_bands,
                    previous=None if old_channel is None else old_channel.smoothing,
                    checkpoint=lambda band: token.checkpoint("smoothing", channel=channel_index, band=band),
                )
                recomputed.update(f"smooth:{channel_name}:{band}" for band in smoothing.recomputed_bands)
            else:
                assert old_channel is not None
                smoothing = old_channel.smoothing

            filter_bands = tuple(range(len(bands))) if old_channel is None else plan.filter_bands
            if filter_bands:
                token.checkpoint("filter", channel=channel_index)
                filter_design = design_compensation_filter(
                    bands,
                    smoothing.power_db,
                    wav.sample_rate,
                    params,
                    channel_index=channel_index,
                    previous=None if old_channel is None else old_channel.filter,
                    affected_band_indices=filter_bands,
                )
                recomputed.add(f"filter:{channel_name}")
            else:
                assert old_channel is not None
                filter_design = old_channel.filter

            if base_state is None:
                error_bands = tuple(range(len(bands)))
            elif plan.error_bands:
                error_bands = plan.error_bands
            else:
                error_bands = ()
            if error_bands:
                errors = compute_channel_errors(
                    alignment_result.channels[channel_index],
                    spectral,
                    smoothing,
                    filter_design,
                    params.error_threshold_db,
                    affected_band_indices=error_bands,
                    previous=None if old_channel is None else old_channel.errors,
                    checkpoint=lambda band: token.checkpoint("error", channel=channel_index, band=band),
                )
                recomputed.update(f"error:{channel_name}:{band}" for band in error_bands)
            else:
                assert old_channel is not None
                errors = old_channel.errors

            channel_result = ChannelComputation(
                alignment=alignment_result.channels[channel_index],
                spectral=spectral,
                smoothing=smoothing,
                filter=filter_design,
                errors=errors,
            )
            channel_results[channel_index] = channel_result
            if run is not None:
                # The manager itself rejects duplicate channel intervals.
                self.snapshot_manager.write_channel(run, channel_result)
            if progress:
                progress(
                    {
                        "stage": "channel_complete",
                        "channel": channel_index,
                        "completed": channel_index + 1,
                        "total": wav.channel_count,
                    }
                )

        token.checkpoint("consistency")
        channels = tuple(channel for channel in channel_results if channel is not None)
        if len(channels) != wav.channel_count:
            raise RuntimeError("internal error: incomplete channel results")

        if base_state is None or resume is not None:
            consistency_bands = tuple(range(len(bands)))
        else:
            consistency_bands = plan.consistency_bands
        consistency_by_band = self._detect_consistency(
            wav.channel_names,
            channels,
            bands,
            params.consistency_tolerance_db,
        )
        consistency_alerts: list[ConsistencyAlert] = []
        for band in bands:
            if base_state is None or resume is not None or band.index in consistency_bands:
                consistency_alerts.extend(consistency_by_band[band.index])
                recomputed.add(f"consistency:{band.index}")
            else:
                consistency_alerts.extend(
                    alert for alert in base_state.consistency_alerts if alert.band_index == band.index
                )

        if resume is not None:
            affected_channels = tuple(range(wav.channel_count))
            affected_bands = tuple(range(len(bands)))
        else:
            affected_channels = plan.affected_channels
            affected_bands = plan.affected_bands

        node_hashes: dict[str, str] = {}
        old_hashes = {} if base_state is None else base_state.node_hashes
        for key in all_node_keys(wav.channel_names, bands):
            if key not in recomputed and key in old_hashes and resume is None:
                node_hashes[key] = old_hashes[key]
                reused_count += 1
                continue
            if key.startswith("consistency:"):
                band_index = int(key.split(":", 1)[1])
                node_hashes[key] = sha256_obj(
                    [alert.to_dict() for alert in consistency_alerts if alert.band_index == band_index]
                )
            else:
                node_hashes[key] = sha256_obj(self._node_payload(key, channels, bands))

        version = 1 if base_state is None else base_state.version + 1
        state = CalibrationState(
            version=version,
            parent_hash=base_hash,
            params=params,
            source_hash=source_hash,
            sample_rate=wav.sample_rate,
            channel_names=wav.channel_names,
            bands=tuple(bands),
            channels=channels,
            consistency_alerts=tuple(consistency_alerts),
            affected_channels=affected_channels,
            affected_bands=affected_bands,
            recomputed_nodes=tuple(sorted(recomputed)),
            node_hashes=node_hashes,
            content_hash="",
            rollback_of=None,
            run_id=None if run is None else run.run_id,
            alignment_diff=align_diff,
        ).with_computed_hash()

        token.checkpoint("publish")
        latest = self.state_store.latest()
        latest_hash = None if latest is None else latest.content_hash
        if latest_hash != base_hash:
            raise CalibrationError(
                E_VERSION_STALE,
                "version changed while recomputation was running",
                {"expected_parent_hash": base_hash, "actual_parent_hash": latest_hash},
            )
        published = self.state_store.publish(state, expected_latest_hash=base_hash)
        if run is not None:
            self.snapshot_manager.mark(run, "published", published.version, published.content_hash)
        if progress:
            progress(
                {
                    "stage": "published",
                    "version": published.version,
                    "content_hash": published.content_hash,
                    "reused_nodes": reused_count,
                    "recomputed_nodes": len(recomputed),
                }
            )
        return published

    def _run_with_guard(
        self,
        params: CalibrationParams,
        expected_parent_hash: str | None,
        token: CancellationToken,
        progress: ProgressCallback | None,
        resume: RecoverySnapshot | None = None,
    ) -> CalibrationState:
        wav, source_hash = self._load_source()
        self._validate_measurement(wav, params)
        base_state = self.state_store.latest()
        base_version = 0 if base_state is None else base_state.version
        base_hash = None if base_state is None else base_state.content_hash
        if resume is not None:
            run = SnapshotRun(
                run_id=resume.run_id,
                directory=resume.manifest_path.parent,
                manifest_path=resume.manifest_path,
                base_version=resume.base_version,
                base_hash=resume.base_hash,
                params=resume.params,
                source_hash=resume.source_hash,
                sample_rate=resume.sample_rate,
                channel_names=resume.channel_names,
                bands=resume.bands,
                segments=[segment.to_manifest() for segment in resume.segments],
                chain_head=resume.chain_head,
                status="running",
            )
        else:
            bands = make_bands(params.spectral, wav.sample_rate)
            run = self.snapshot_manager.begin_run(
                params=params,
                source_hash=source_hash,
                sample_rate=wav.sample_rate,
                channel_names=wav.channel_names,
                bands=bands,
                base_version=base_version,
                base_hash=base_hash,
            )
        try:
            return self._compute_and_publish(
                params,
                expected_parent_hash,
                run,
                token,
                progress,
                resume=resume,
            )
        except OperationCancelled:
            self.snapshot_manager.mark(run, "cancelled")
            raise
        except CalibrationError as exc:
            self.snapshot_manager.mark(run, "failed")
            raise exc
        except BaseException:
            self.snapshot_manager.mark(run, "failed")
            raise

    def submit(
        self,
        params: CalibrationParams,
        expected_latest_hash: str | None = None,
        token: CancellationToken | None = None,
        progress: ProgressCallback | None = None,
    ) -> CalibrationState:
        if not self._busy_lock.acquire(blocking=False):
            raise CalibrationError(E_BUSY, "a recalculation task is already running")
        cancellation = token or CancellationToken()
        self._cancel_token = cancellation
        try:
            latest = self.state_store.latest()
            expected = latest.content_hash if expected_latest_hash is None and latest is not None else expected_latest_hash
            return self._run_with_guard(params, expected, cancellation, progress)
        finally:
            self._cancel_token = None
            self._busy_lock.release()

    def start_async(
        self,
        params: CalibrationParams,
        expected_latest_hash: str | None = None,
        progress: ProgressCallback | None = None,
    ) -> AsyncTask:
        if not self._busy_lock.acquire(blocking=False):
            raise CalibrationError(E_BUSY, "a recalculation task is already running")
        token = CancellationToken()
        holder: dict[str, AsyncTask] = {}

        def worker() -> None:
            task = holder["task"]
            try:
                latest = self.state_store.latest()
                expected = (
                    latest.content_hash
                    if expected_latest_hash is None and latest is not None
                    else expected_latest_hash
                )
                task.result = self._run_with_guard(params, expected, token, progress)
            except BaseException as exc:
                task.error = exc
            finally:
                self._cancel_token = None
                self._busy_lock.release()
                task.done.set()

        thread = threading.Thread(target=worker, name="acoustic-calibration-worker", daemon=True)
        task = AsyncTask(token=token, thread=thread)
        holder["task"] = task
        self._active_task = task
        self._cancel_token = token
        thread.start()
        return task

    def cancel(self) -> bool:
        token = self._cancel_token
        if token is None:
            return False
        token.cancel()
        return True

    def diff(self, left_version: int, right_version: int) -> dict[str, Any]:
        return version_diff(self.state_store.load(left_version), self.state_store.load(right_version))

    def rollback(
        self,
        target_version: int,
        expected_latest_hash: str | None = None,
    ) -> CalibrationState:
        if not self._busy_lock.acquire(blocking=False):
            raise CalibrationError(E_BUSY, "a recalculation task is already running")
        try:
            latest = self.state_store.latest()
            expected = latest.content_hash if expected_latest_hash is None and latest is not None else expected_latest_hash
            return self.state_store.rollback(target_version, expected_latest_hash=expected)
        finally:
            self._busy_lock.release()

    def recover(
        self,
        token: CancellationToken | None = None,
        progress: ProgressCallback | None = None,
    ) -> RecoveryReport:
        if not self._busy_lock.acquire(blocking=False):
            raise CalibrationError(E_BUSY, "a recalculation task is already running")
        cancellation = token or CancellationToken()
        self._cancel_token = cancellation
        try:
            wav, source_hash = self._load_source()
            snapshot = self.snapshot_manager.load_latest_valid()
            if snapshot is None:
                current = self.state_store.latest()
                if current is None:
                    raise CalibrationError(
                        E_RECOVERY_INVALID,
                        "no snapshot or published version is available",
                        {"first_broken_interval": None},
                    )
                return RecoveryReport(current, None, (), (), True)

            published_match = None
            for version in self.state_store.list_versions():
                state = self.state_store.load(version)
                if state.run_id == snapshot.run_id:
                    published_match = state
                    break
            if published_match is not None:
                self.snapshot_manager.mark(
                    SnapshotRun(
                        run_id=snapshot.run_id,
                        directory=snapshot.manifest_path.parent,
                        manifest_path=snapshot.manifest_path,
                        base_version=snapshot.base_version,
                        base_hash=snapshot.base_hash,
                        params=snapshot.params,
                        source_hash=snapshot.source_hash,
                        sample_rate=snapshot.sample_rate,
                        channel_names=snapshot.channel_names,
                        bands=snapshot.bands,
                        segments=[segment.to_manifest() for segment in snapshot.segments],
                        chain_head=snapshot.chain_head,
                    ),
                    "published",
                    published_match.version,
                    published_match.content_hash,
                )
                return RecoveryReport(
                    published_match,
                    snapshot.run_id,
                    tuple(range(wav.channel_count)),
                    (),
                    True,
                )
            if snapshot.status == "failed":
                current = self.state_store.latest()
                if current is None:
                    raise CalibrationError(
                        E_RECOVERY_INVALID,
                        "latest snapshot belongs to a failed run and no published version exists",
                        {"run_id": snapshot.run_id, "first_broken_interval": None},
                    )
                return RecoveryReport(current, snapshot.run_id, (), (), True)

            if snapshot.source_hash != source_hash:
                raise CalibrationError(
                    E_RECOVERY_INVALID,
                    "snapshot source hash does not match the selected WAV",
                    {
                        "run_id": snapshot.run_id,
                        "expected_source_hash": snapshot.source_hash,
                        "actual_source_hash": source_hash,
                        "first_broken_interval": None,
                    },
                )
            if snapshot.sample_rate != wav.sample_rate or snapshot.channel_names != wav.channel_names:
                raise CalibrationError(
                    E_RECOVERY_INVALID,
                    "snapshot channel layout does not match the selected WAV",
                    {"run_id": snapshot.run_id, "first_broken_interval": None},
                )

            base_state: CalibrationState | None = None
            if snapshot.base_version > 0:
                try:
                    base_state = self.state_store.load(snapshot.base_version)
                except BaseException as exc:
                    raise CalibrationError(
                        E_RECOVERY_INVALID,
                        "snapshot base version cannot be restored",
                        {
                            "run_id": snapshot.run_id,
                            "base_version": snapshot.base_version,
                            "first_broken_interval": None,
                            "error": str(exc),
                        },
                    ) from exc
                if base_state.content_hash != snapshot.base_hash:
                    raise CalibrationError(
                        E_RECOVERY_INVALID,
                        "snapshot base hash chain is broken",
                        {
                            "run_id": snapshot.run_id,
                            "expected_base_hash": snapshot.base_hash,
                            "actual_base_hash": base_state.content_hash,
                            "first_broken_interval": None,
                        },
                    )
            elif snapshot.base_hash is not None:
                raise CalibrationError(
                    E_RECOVERY_INVALID,
                    "snapshot has a base hash without a base version",
                    {"run_id": snapshot.run_id, "first_broken_interval": None},
                )

            report = self._run_with_guard(
                snapshot.params,
                snapshot.base_hash,
                cancellation,
                progress,
                resume=snapshot,
            )
            completed = set(snapshot.completed_channel_indices)
            recomputed = tuple(index for index in range(wav.channel_count) if index not in completed)
            return RecoveryReport(
                report,
                snapshot.run_id,
                tuple(sorted(completed)),
                recomputed,
                False,
            )
        finally:
            self._cancel_token = None
            self._busy_lock.release()
