from __future__ import annotations

import os
import signal as signal_module
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .engine import (
    ExecutionResult,
    PreviousState,
    RecomputePlan,
    execute_plan,
    plan_from_previous,
)
from .errors import ErrorCode, HapticError
from .models import DeviceConfig, WaveSequence
from .persistence import (
    AtomicTraceLog,
    PendingWork,
    RecoveryReport,
    SnapshotState,
    SnapshotStore,
)
from .sampler import (
    CancelToken,
    SamplingResult,
    compute_full_signal,
    validate_for_sampling,
)
from .versions import (
    AtomicPublisher,
    ConfigVersion,
    ConfigVersionStore,
    PreviewHandle,
    PublishedSignal,
    QueryResult,
)


@dataclass(slots=True)
class _ActiveJob:
    token: CancelToken


class HapticSession:
    """Coordinates planning, publication, persistence and recovery."""

    def __init__(self, data_dir: str | os.PathLike[str]) -> None:
        self.data_dir = Path(data_dir)
        self.data_dir.mkdir(parents=True, exist_ok=True)
        self._config_store = ConfigVersionStore()
        self._publisher = AtomicPublisher()
        self._snapshot_store = SnapshotStore(self.data_dir)
        self._trace = AtomicTraceLog(self.data_dir / "trace.jsonl")
        self._job_lock = threading.Lock()
        self._publish_lock = threading.RLock()
        self._active_job: _ActiveJob | None = None
        self._previous: PreviousState | None = None
        self._current_config_version: int | None = None
        self._recovery_report = RecoveryReport(status="new", latest_publication=None)

    @classmethod
    def create(
        cls,
        sequence: WaveSequence,
        config: DeviceConfig,
        data_dir: str | os.PathLike[str],
    ) -> HapticSession:
        session = cls(data_dir)
        session._snapshot_store.discard_temporary_files()
        for name in ("session.snapshot.json", "trace.jsonl"):
            try:
                (session.data_dir / name).unlink()
            except FileNotFoundError:
                pass
        validate_for_sampling(sequence, config)
        initial = session._config_store.commit(config)
        session._current_config_version = initial.version
        session._snapshot_store.save(
            SnapshotState(config_versions=session._config_store.versions)
        )
        session.recompute(sequence, operation="initial")
        return session

    @classmethod
    def open(
        cls,
        data_dir: str | os.PathLike[str],
        *,
        sequence: WaveSequence | None = None,
        config: DeviceConfig | None = None,
        recover_pending: bool = True,
    ) -> HapticSession:
        session = cls(data_dir)
        session._snapshot_store.discard_temporary_files()
        if not session._snapshot_store.path.exists():
            if sequence is None or config is None:
                raise HapticError(
                    ErrorCode.NOT_FOUND,
                    "no snapshot exists and no initial sequence/config was supplied",
                )
            return cls.create(sequence, config, data_dir)
        state = session._snapshot_store.load()
        session._config_store.restore(state.config_versions)
        for publication in state.publications:
            session._publisher.restore(publication)
        if state.publications:
            latest = state.publications[-1]
            session._previous = PreviousState(
                sequence=latest.sequence,
                config=latest.config,
                result=latest.sampling,
            )
            session._current_config_version = latest.config_version
            report = RecoveryReport(
                status="clean",
                latest_publication=latest.publication_version,
                recovered_publication=latest.publication_version,
                note="restored latest complete publication from snapshot",
            )
        else:
            report = RecoveryReport(
                status="empty",
                latest_publication=None,
                note="snapshot has no publication",
            )
        session._recovery_report = report
        session._trace.append(report.to_dict())
        if state.pending is not None and recover_pending:
            session._recover_pending(state.pending, state)
        elif state.pending is not None:
            report.pending_target = state.pending.target_publication_version
            session._trace.append(report.to_dict())
        elif not state.publications:
            if sequence is None or config is None:
                raise HapticError(
                    ErrorCode.RECOVERY_INVALID,
                    "snapshot has no publication and no recovery sequence was supplied",
                    version=1,
                    field="publications",
                )
            if state.config_versions:
                latest_config = state.config_versions[-1]
                session._current_config_version = latest_config.version
            else:
                latest_config = session._config_store.commit(config)
                session._current_config_version = latest_config.version
                session._persist(
                    SnapshotState(config_versions=session._config_store.versions)
                )
            validate_for_sampling(sequence, latest_config.config)
            published = session.recompute(sequence, operation="initial_recovery")
            session._recovery_report = RecoveryReport(
                status="resumed_empty",
                latest_publication=None,
                recovered_publication=published.publication_version,
                note="restarted first publication from supplied sequence/config",
            )
            session._trace.append(session._recovery_report.to_dict())
        return session

    @property
    def sequence(self) -> WaveSequence:
        if self._previous is None:
            raise HapticError(ErrorCode.NOT_FOUND, "no active waveform sequence")
        return self._previous.sequence

    @property
    def config(self) -> DeviceConfig:
        if self._previous is None:
            raise HapticError(ErrorCode.NOT_FOUND, "no active device config")
        return self._previous.config

    @property
    def sampling(self) -> SamplingResult:
        if self._previous is None:
            raise HapticError(ErrorCode.NOT_FOUND, "no active sampling result")
        return self._previous.result

    @property
    def current_config_version(self) -> int:
        if self._current_config_version is None:
            raise HapticError(ErrorCode.NOT_FOUND, "no active config version")
        return self._current_config_version

    @property
    def latest_publication(self) -> PublishedSignal:
        return self._publisher.current

    @property
    def recovery_report(self) -> RecoveryReport:
        return self._recovery_report

    @property
    def trace_events(self) -> tuple[dict[str, Any], ...]:
        return self._trace.events

    def issue_handle(self) -> PreviewHandle:
        with self._publish_lock:
            return self._publisher.issue_handle()

    def preview(self, handle: PreviewHandle) -> QueryResult:
        with self._publish_lock:
            return self._publisher.query(handle)

    def preview_version(self, version: int) -> QueryResult:
        with self._publish_lock:
            return self._publisher.query_version(version)

    def publication_history(self) -> tuple[PublishedSignal, ...]:
        with self._publish_lock:
            return self._publisher.history()

    def config_history(self) -> tuple[ConfigVersion, ...]:
        return self._config_store.versions

    def diff_config_versions(self, older: int, newer: int) -> dict[str, Any]:
        return self._config_store.diff(older, newer)

    def diff_publications(self, older: int, newer: int) -> dict[str, Any]:
        left = self.preview_version(older).published
        right = self.preview_version(newer).published
        return {
            "older_publication": older,
            "newer_publication": newer,
            "older_version_hash": left.version_hash,
            "newer_version_hash": right.version_hash,
            "config_diff": self._config_store.diff(
                left.config_version, right.config_version
            ),
            "signal_hash_changed": left.signal_hash != right.signal_hash,
            "affected_segments": list(right.affected_segments),
            "affected_ranges": [list(item) for item in right.affected_ranges],
        }

    def verify_historical(self, version: int) -> dict[str, Any]:
        published = self.preview_version(version).published
        recomputed = compute_full_signal(published.sequence, published.config)
        return {
            "version": version,
            "config_version": published.config_version,
            "expected_signal_hash": published.signal_hash,
            "recomputed_signal_hash": recomputed.signal_hash,
            "match": recomputed.signal_hash == published.signal_hash,
        }

    def cancel(self) -> bool:
        with self._publish_lock:
            if self._active_job is None:
                return False
            self._active_job.token.cancel()
            return True

    def recompute(
        self,
        sequence: WaveSequence,
        *,
        new_config: DeviceConfig | None = None,
        operation: str = "recompute",
        cancellation: CancelToken | None = None,
        crash_after_segments: int | None = None,
    ) -> PublishedSignal:
        if new_config is None:
            if self._previous is not None:
                config = self._previous.config
            else:
                config = self._config_store.get(self.current_config_version).config
        else:
            config = new_config
        validate_for_sampling(sequence, config)
        if not self._job_lock.acquire(blocking=False):
            raise HapticError(
                ErrorCode.BUSY,
                "another recompute task is already active",
            )
        token = cancellation or CancelToken()
        self._active_job = _ActiveJob(token=token)
        try:
            if new_config is not None:
                plan = plan_from_previous(sequence, config, self._previous)
                config_version = self._config_store.commit(config)
                self._current_config_version = config_version.version
                self._persist(
                    SnapshotState(
                        config_versions=self._config_store.versions,
                        publications=self._publisher.history(),
                        pending=None,
                    )
                )
                self._trace.append(
                    {
                        "event": "config_commit",
                        "config_version": config_version.version,
                        "config_hash": config_version.config_hash,
                        "parent_hash": config_version.parent_hash,
                        "source_version": config_version.source_version,
                    }
                )
            else:
                plan = plan_from_previous(sequence, config, self._previous)
                config_version = self._config_store.get(self.current_config_version)
            return self._run_plan(
                plan,
                config_version=config_version,
                operation=operation,
                token=token,
                restored_signals={},
                crash_after_segments=crash_after_segments,
            )
        except HapticError as exc:
            if self._publisher.history():
                self._current_config_version = self._publisher.current.config_version
            if exc.code == ErrorCode.CANCELLED:
                self._persist(
                    SnapshotState(
                        config_versions=self._config_store.versions,
                        publications=self._publisher.history(),
                        pending=None,
                    )
                )
                self._trace.append(
                    {
                        "event": "recompute_cancelled",
                        "latest_publication": (
                            None
                            if not self._publisher.history()
                            else self._publisher.current.publication_version
                        ),
                    }
                )
            raise
        finally:
            self._active_job = None
            self._job_lock.release()

    def rollback_config(self, version: int) -> PublishedSignal:
        target = self._config_store.get(version)
        sequence = self.sequence
        validate_for_sampling(sequence, target.config)
        if not self._job_lock.acquire(blocking=False):
            raise HapticError(
                ErrorCode.BUSY,
                "another recompute task is already active",
            )
        token = CancelToken()
        self._active_job = _ActiveJob(token=token)
        try:
            plan = plan_from_previous(sequence, target.config, self._previous)
            config_version = self._config_store.rollback(version)
            self._current_config_version = config_version.version
            self._persist(
                SnapshotState(
                    config_versions=self._config_store.versions,
                    publications=self._publisher.history(),
                    pending=None,
                )
            )
            self._trace.append(
                {
                    "event": "config_rollback",
                    "new_version": config_version.version,
                    "source_version": version,
                    "config_hash": config_version.config_hash,
                }
            )
            return self._run_plan(
                plan,
                config_version=config_version,
                operation="rollback",
                token=token,
                restored_signals={},
            )
        finally:
            self._active_job = None
            self._job_lock.release()

    def _run_plan(
        self,
        plan: RecomputePlan,
        *,
        config_version: ConfigVersion,
        operation: str,
        token: CancelToken,
        restored_signals: dict[int, Any],
        crash_after_segments: int | None = None,
    ) -> PublishedSignal:
        history = self._publisher.history()
        target_version = (history[-1].publication_version + 1) if history else 1
        parent_hash = history[-1].version_hash if history else None
        base_version = history[-1].publication_version if history else None
        pending = PendingWork(
            target_publication_version=target_version,
            base_publication_version=base_version,
            parent_version_hash=parent_hash,
            sequence=plan.sequence,
            config_version=config_version.version,
            config=plan.config,
            completed_signals=tuple(restored_signals.values()),
            operation=operation,
        )
        self._persist_pending(pending)
        self._trace.append(
            {
                "event": "recompute_start",
                "target_version": target_version,
                "base_version": base_version,
                "config_version": config_version.version,
                "sequence_hash": plan.sequence.content_hash,
                "config_hash": plan.config.content_hash,
                "operation": operation,
                "full_recompute": plan.full_recompute,
                "full_reason": plan.full_reason,
                "recompute_segments": list(plan.recompute_segments),
                "reuse_segments": list(plan.reuse_segments),
            }
        )
        checkpoints = {"count": 0}

        def checkpoint(signal: Any) -> None:
            nonlocal pending
            pending = pending.with_signal(signal)
            self._persist_pending(pending)
            checkpoints["count"] += 1
            if (
                crash_after_segments is not None
                and checkpoints["count"] >= crash_after_segments
            ):
                sigkill = getattr(signal_module, "SIGKILL", None)
                if sigkill is not None:
                    os.kill(os.getpid(), sigkill)
                os._exit(137)

        execution = execute_plan(
            plan,
            cancellation=token,
            checkpoint=checkpoint,
            restored_signals=restored_signals,
        )
        token.check()
        published = self._make_published(
            plan.sequence,
            plan.config,
            config_version.version,
            target_version,
            parent_hash,
            execution,
            plan,
        )
        with self._publish_lock:
            # Durable commit happens before the in-memory reference swap. The
            # session preview lock makes the save+swap pair invisible until both
            # have completed.
            state = SnapshotState(
                config_versions=self._config_store.versions,
                publications=(*history, published),
                pending=None,
            )
            self._persist(state)
            self._publisher.publish(published)
            self._previous = PreviousState(
                sequence=published.sequence,
                config=published.config,
                result=published.sampling,
            )
            self._current_config_version = config_version.version
        self._trace.append(published.trace_record())
        return published

    def _make_published(
        self,
        sequence: WaveSequence,
        config: DeviceConfig,
        config_version: int,
        publication_version: int,
        parent_hash: str | None,
        execution: ExecutionResult,
        plan: RecomputePlan,
    ) -> PublishedSignal:
        sampling = execution.sampling
        affected_ranges = plan.affected_ranges
        reused_ranges = plan.reused_ranges
        recomputed_samples = sum(end - start for start, end in affected_ranges)
        return PublishedSignal(
            publication_version=publication_version,
            config_version=config_version,
            sequence=sequence,
            config=config,
            sampling=sampling,
            affected_segments=plan.affected_segments,
            recomputed_segments=execution.computed_segments,
            restored_from_checkpoint=execution.restored_from_checkpoint,
            reused_segments=execution.reused_segments,
            affected_ranges=affected_ranges,
            reused_ranges=reused_ranges,
            recomputed_sample_count=recomputed_samples,
            reused_sample_count=sampling.total_frames - recomputed_samples,
            full_recompute=plan.full_recompute,
            full_reason=plan.full_reason,
            reasons=plan.reasons,
            parent_version_hash=parent_hash,
        )

    def _recover_pending(self, pending: PendingWork, state: SnapshotState) -> None:
        base = self._previous
        plan = plan_from_previous(pending.sequence, pending.config, base)
        target = pending.target_publication_version
        history = self._publisher.history()
        if history and target != history[-1].publication_version + 1:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "pending target is not next publication version",
                version=target,
                field="target_publication_version",
            )
        if not history and target != 1:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "first pending target must be version 1",
                version=target,
                field="target_publication_version",
            )
        restored = {signal.index: signal for signal in pending.completed_signals}
        token = CancelToken()
        pending_holder = {"value": pending}

        def checkpoint(signal: Any) -> None:
            updated = pending_holder["value"].with_signal(signal)
            pending_holder["value"] = updated
            self._persist(
                SnapshotState(
                    config_versions=self._config_store.versions,
                    publications=self._publisher.history(),
                    pending=updated,
                )
            )

        execution = execute_plan(
            plan,
            cancellation=token,
            checkpoint=checkpoint,
            restored_signals=restored,
        )
        config_version = self._config_store.get(pending.config_version)
        published = self._make_published(
            plan.sequence,
            plan.config,
            config_version.version,
            target,
            pending.parent_version_hash,
            execution,
            plan,
        )
        state_to_save = SnapshotState(
            config_versions=self._config_store.versions,
            publications=(*history, published),
            pending=None,
        )
        self._persist(state_to_save)
        self._publisher.publish(published)
        self._previous = PreviousState(
            sequence=published.sequence,
            config=published.config,
            result=published.sampling,
        )
        self._current_config_version = config_version.version
        self._recovery_report = RecoveryReport(
            status="resumed_pending",
            latest_publication=(history[-1].publication_version if history else None),
            pending_target=target,
            restored_checkpoint_segments=execution.restored_from_checkpoint,
            recomputed_segments=execution.computed_segments,
            recovered_publication=target,
            note="completed pending publication from atomic checkpoints",
        )
        self._trace.append(self._recovery_report.to_dict())
        self._trace.append(published.trace_record())

    def _persist(self, state: SnapshotState) -> None:
        try:
            self._snapshot_store.save(state)
        except HapticError:
            raise
        except OSError as exc:
            raise HapticError(
                ErrorCode.IO,
                "atomic snapshot write failed",
                details={"error": str(exc)},
            ) from exc

    def _persist_pending(self, pending: PendingWork) -> None:
        self._persist(
            SnapshotState(
                config_versions=self._config_store.versions,
                publications=self._publisher.history(),
                pending=pending,
            )
        )
