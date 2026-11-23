"""Application service tying samples, incremental runs, snapshots, and verification."""

from __future__ import annotations

import threading
from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import Any

from .compare import DiffReport, compare_results
from .concurrency import ReplayCoordinator, SerializedSnapshotWriter, available_parallelism
from .errors import ParameterError
from .models import BindingTable, DeviceSpec, SampleBundle
from .pipeline import Pipeline, PipelineRun
from .replay import ReplayResult, TruthReport, check_truth
from .session import HotSwitchResult, HotSwitchSession
from .snapshot import SnapshotLoadResult, SnapshotStore, SnapshotWriteReport
from .validation import load_sample, validate_mapping


@dataclass(frozen=True)
class VerificationReport:
    diff: DiffReport
    full_result: ReplayResult
    full_result_hash: str
    checked_at_generation: int

    @property
    def equal(self) -> bool:
        return self.diff.equal

    def to_dict(self) -> dict[str, Any]:
        return {
            "equal": self.equal,
            "diff": self.diff.to_dict(),
            "full_result_hash": self.full_result_hash,
            "checked_at_generation": self.checked_at_generation,
        }


@dataclass(frozen=True)
class SessionUpdate:
    run: PipelineRun
    verification: VerificationReport | None
    previous_result: ReplayResult | None
    changed_parameters: tuple[str, ...] = ()

    def to_dict(self) -> dict[str, Any]:
        return {
            "run": self.run.to_dict(),
            "verification": self.verification.to_dict() if self.verification else None,
            "changed_parameters": list(self.changed_parameters),
        }


@dataclass
class StudioSession:
    bundle: SampleBundle
    snapshot_path: Path = Path("artifacts/snapshots/state.json")
    max_parallel: int = field(default_factory=available_parallelism)
    event_buffer_limit: int = 256
    window_size: int = 32
    restore_snapshot: bool = True

    pipeline: Pipeline = field(init=False)
    coordinator: ReplayCoordinator = field(init=False)
    snapshot_store: SnapshotStore = field(init=False)
    snapshot_writer: SerializedSnapshotWriter = field(init=False)
    last_run: PipelineRun = field(init=False)
    previous_result: ReplayResult | None = field(default=None, init=False)
    last_verification: VerificationReport | None = field(default=None, init=False)
    snapshot_load: SnapshotLoadResult | None = field(default=None, init=False)
    snapshot_write: SnapshotWriteReport | None = field(default=None, init=False)
    truth_report: TruthReport | None = field(default=None, init=False)
    _lock: threading.RLock = field(default_factory=threading.RLock, init=False)

    def __post_init__(self) -> None:
        self.coordinator = ReplayCoordinator(
            max_parallel=self.max_parallel,
            event_buffer_limit=self.event_buffer_limit,
        )
        self.snapshot_store = SnapshotStore(self.snapshot_path)
        self.snapshot_writer = SerializedSnapshotWriter(self.snapshot_store)
        self.pipeline = Pipeline(
            self.bundle,
            max_event_buffer=self.event_buffer_limit,
            window_size=self.window_size,
        )
        started_from_snapshot = False
        if self.restore_snapshot:
            self.snapshot_load = self.snapshot_store.load(self.pipeline)
            if self.snapshot_load.valid:
                self.pipeline.install_cache_entries(self.snapshot_load.entries)
                started_from_snapshot = True
        self.last_run = self.pipeline.recompute()
        if self.last_run.recomputed_count and started_from_snapshot:
            # A valid snapshot must be a pure reuse; otherwise treat it as invalid.
            self.snapshot_load = SnapshotLoadResult(
                path=str(self.snapshot_path),
                valid=False,
                reason="cache_chain_mismatch",
                invalidated_node_count=len(self.pipeline.order),
                metadata=self.snapshot_load.metadata if self.snapshot_load else {},
                entries={},
                version_mismatches=("snapshot entries did not fully reuse",),
            )
            self.pipeline.clear_caches()
            self.last_run = self.pipeline.recompute()
        self.truth_report = self._current_truth_report()

    def _current_truth_report(self) -> TruthReport | None:
        if (
            self.pipeline.device != self.bundle.device
            or self.pipeline.bindings != self.bundle.bindings
        ):
            return None
        return check_truth(self.bundle, self.last_run.result)

    @classmethod
    def from_path(
        cls,
        sample_path: str | Path,
        *,
        snapshot_path: str | Path = "artifacts/snapshots/state.json",
        max_parallel: int | None = None,
        event_buffer_limit: int = 256,
        restore_snapshot: bool = True,
    ) -> "StudioSession":
        bundle = load_sample(sample_path)
        return cls(
            bundle=bundle,
            snapshot_path=Path(snapshot_path),
            max_parallel=max_parallel or available_parallelism(),
            event_buffer_limit=event_buffer_limit,
            restore_snapshot=restore_snapshot,
        )

    def update_axis(
        self,
        axis_id: str,
        *,
        deadzone: float | None = None,
        inverted: bool | None = None,
        curve_exponent: float | None = None,
        verify: bool = True,
    ) -> SessionUpdate:
        with self._lock:
            changes: dict[str, Any] = {}
            if deadzone is not None:
                if not 0.0 <= deadzone <= 0.95:
                    raise ParameterError(
                        "deadzone must be within [0, 0.95]",
                        field="deadzone",
                    )
                changes["deadzone"] = float(deadzone)
            if inverted is not None:
                changes["inverted"] = bool(inverted)
            if curve_exponent is not None:
                axis = self.pipeline.device.axis_map[axis_id]
                curve = replace(axis.curve, kind="power", exponent=float(curve_exponent))
                changes["curve"] = curve
            if not changes:
                raise ParameterError("no axis parameter changes supplied", field="axis")
            device = self.pipeline.device.with_axis(axis_id, **changes)
            return self._apply(
                device=device,
                changed_parameters=tuple(f"axis:{axis_id}.{key}" for key in changes),
                verify=verify,
            )

    def update_rule(self, rule: Any, *, verify: bool = True) -> SessionUpdate:
        with self._lock:
            index = next(
                (
                    position
                    for position, existing in enumerate(self.pipeline.bindings.rules)
                    if existing.id == rule.id
                ),
                None,
            )
            if index is None:
                raise ParameterError(
                    f"binding rule {rule.id!r} does not exist",
                    field="rule_id",
                )
            rules = list(self.pipeline.bindings.rules)
            rules[index] = rule
            bindings = BindingTable(
                schema_version=self.pipeline.bindings.schema_version,
                rules=tuple(rules),
                declared_version=self.pipeline.bindings.declared_version,
            )
            issues = validate_mapping(self.pipeline.device, bindings)
            if issues:
                from .errors import SampleValidationError

                raise SampleValidationError(issues)
            return self._apply(
                bindings=bindings,
                changed_parameters=(f"rule:{rule.id}",),
                verify=verify,
            )

    def update_device(self, device: DeviceSpec, *, verify: bool = True) -> SessionUpdate:
        with self._lock:
            return self._apply(
                device=device,
                changed_parameters=(f"device:{device.id}",),
                verify=verify,
            )

    def retry_node(self, node_id: str, *, verify: bool = True) -> SessionUpdate:
        with self._lock:
            return self._apply(
                force_node_ids=(node_id,),
                changed_parameters=(f"retry:{node_id}",),
                verify=verify,
            )

    def _apply(
        self,
        *,
        device: DeviceSpec | None = None,
        bindings: BindingTable | None = None,
        force_node_ids: tuple[str, ...] = (),
        changed_parameters: tuple[str, ...],
        verify: bool,
    ) -> SessionUpdate:
        self.previous_result = self.last_run.result
        self.last_run = self.pipeline.update(
            device=device,
            bindings=bindings,
            force_node_ids=force_node_ids,
        )
        self.truth_report = self._current_truth_report()
        verification = self.verify() if verify else None
        return SessionUpdate(
            run=self.last_run,
            verification=verification,
            previous_result=self.previous_result,
            changed_parameters=changed_parameters,
        )

    def verify(self, *, raise_on_divergence: bool = True) -> VerificationReport:
        full_pipeline = Pipeline(
            self.pipeline.bundle,
            max_event_buffer=self.event_buffer_limit,
            window_size=self.window_size,
        )
        full_run = full_pipeline.recompute()
        diff = compare_results(
            self.last_run.result,
            full_run.result,
            raise_on_divergence=raise_on_divergence,
        )
        report = VerificationReport(
            diff=diff,
            full_result=full_run.result,
            full_result_hash=full_run.result.result_hash,
            checked_at_generation=self.last_run.generation,
        )
        self.last_verification = report
        return report

    def save_snapshot(self) -> SnapshotWriteReport:
        with self._lock:
            self.snapshot_write = self.snapshot_writer.write(self.pipeline)
            return self.snapshot_write

    def reload_snapshot(self) -> SnapshotLoadResult:
        with self._lock:
            result = self.snapshot_store.load(self.pipeline)
            self.snapshot_load = result
            if result.valid:
                self.pipeline.install_cache_entries(result.entries)
                self.last_run = self.pipeline.recompute()
                self.truth_report = self._current_truth_report()
            else:
                self.pipeline.clear_caches()
                self.last_run = self.pipeline.recompute()
                self.truth_report = self._current_truth_report()
            return result

    def switch_device(
        self,
        target_bundle: SampleBundle,
        *,
        switch_t_ms: int,
        target_bindings: BindingTable | None = None,
    ) -> HotSwitchResult:
        with self._lock:
            session = HotSwitchSession(
                self.bundle,
                target_bundle,
                switch_t_ms=switch_t_ms,
                target_bindings=target_bindings,
                max_event_buffer=self.event_buffer_limit,
                window_size=self.window_size,
            )
            return session.result

    def state(self) -> dict[str, Any]:
        return {
            "sample": self.bundle.metadata(),
            "pipeline": {
                "generation": self.last_run.generation,
                "graph_signature": self.last_run.graph_signature,
                "param_version": self.last_run.param_version,
                "reused_count": self.last_run.reused_count,
                "recomputed_count": self.last_run.recomputed_count,
                "frame_count": len(self.last_run.result.frames),
                "result_hash": self.last_run.result.result_hash,
            },
            "truth": self.truth_report.to_dict() if self.truth_report else None,
            "snapshot_load": self.snapshot_load.to_dict() if self.snapshot_load else None,
            "snapshot_write": self.snapshot_write.to_dict() if self.snapshot_write else None,
            "verification": (
                self.last_verification.to_dict() if self.last_verification else None
            ),
            "concurrency": {
                "max_parallel": self.coordinator.max_parallel,
                "event_buffer_limit": self.coordinator.event_buffer_limit,
            },
        }

    def close(self) -> None:
        self.coordinator.close()

    def __enter__(self) -> "StudioSession":
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        self.close()


__all__ = [
    "SessionUpdate",
    "StudioSession",
    "VerificationReport",
]
