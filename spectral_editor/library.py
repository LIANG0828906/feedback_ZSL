from __future__ import annotations

import threading
import time
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any, Callable, Iterable, Mapping

from .canonical import canonical_bytes, sha256_bytes
from .color import ColorPipeline, max_channel_delta
from .errors import (
    BusinessError,
    busy,
    material_invalid,
    snapshot_stale,
    version_conflict,
)
from .materials import MaterialDefinition
from .models import (
    MaterialHistory,
    PendingJob,
    PipelineConfig,
    PublicationChainEntry,
    PublishedSnapshot,
    TransitionRequest,
    VersionRecord,
    freeze_value,
    thaw_value,
)
from .persistence import (
    reconcile_pending,
    save_recovery_anchor,
    save_pending,
    save_snapshot,
    snapshot_bytes,
)
from .versioning import (
    diff_versions,
    make_publication_entry,
    make_version_record,
)


@dataclass(frozen=True)
class CandidatePublication:
    snapshot: PublishedSnapshot
    base_generation: int
    base_publication_hash: str
    result: "TransitionResult"


@dataclass(frozen=True)
class TransitionResult:
    operation: str
    generation: int
    previous_generation: int
    affected_material_ids: tuple[str, ...]
    affected_versions: Mapping[str, int]
    reused_versions: Mapping[str, int]
    recomputed_count: int
    reused_count: int
    max_channel_deltas: Mapping[str, Any]
    publication_hash: str
    previous_publication_hash: str
    snapshot_hash: str
    ui_state: Mapping[str, Any]
    materials: Mapping[str, Any]
    pipeline: Mapping[str, Any]

    def trace_payload(self) -> dict[str, Any]:
        return {
            "event": "publish",
            "operation": self.operation,
            "generation": self.generation,
            "previous_generation": self.previous_generation,
            "affected_material_ids": list(self.affected_material_ids),
            "affected_versions": dict(self.affected_versions),
            "reused_versions": dict(self.reused_versions),
            "recomputed_count": self.recomputed_count,
            "reused_count": self.reused_count,
            "max_channel_deltas": thaw_value(self.max_channel_deltas),
            "publication_hash": self.publication_hash,
            "previous_publication_hash": self.previous_publication_hash,
            "snapshot_hash": self.snapshot_hash,
            "ui_state": thaw_value(self.ui_state),
            "materials": thaw_value(self.materials),
            "pipeline": thaw_value(self.pipeline),
        }


class SnapshotHandle:
    """A complete published snapshot held independently of later publications."""

    __slots__ = ("_library", "snapshot", "_released", "handle_id")

    def __init__(
        self,
        library: "MaterialLibrary",
        snapshot: PublishedSnapshot,
        handle_id: int,
    ) -> None:
        self._library = library
        self.snapshot = snapshot
        self._released = False
        self.handle_id = handle_id

    @property
    def released(self) -> bool:
        return self._released

    def ensure_valid(self) -> PublishedSnapshot:
        if self._released or self._library._closed:
            raise snapshot_stale()
        if not self._library._is_registered_handle(self):
            raise snapshot_stale("snapshot handle was not issued by this library")
        return self.snapshot

    def current_materials(self) -> dict[str, dict[str, Any]]:
        return self.ensure_valid().current_materials()

    def material(self, material_id: str) -> dict[str, Any]:
        snapshot = self.ensure_valid()
        try:
            record = snapshot.current_record(material_id)
        except KeyError as exc:
            raise material_invalid(
                "material_id", f"unknown material {material_id!r}"
            ) from exc
        return {
            "version": record.version,
            "colors": thaw_value(record.colors),
            "content_hash": record.content_hash,
            "chain_hash": record.chain_hash,
            "dependencies": thaw_value(record.dependencies),
            "intermediates": thaw_value(record.intermediates),
        }

    def history(self, material_id: str) -> tuple[VersionRecord, ...]:
        snapshot = self.ensure_valid()
        try:
            return snapshot.histories[material_id].versions
        except KeyError as exc:
            raise material_invalid(
                "material_id", f"unknown material {material_id!r}"
            ) from exc

    def release(self) -> None:
        if not self._released:
            self._released = True
            self._library._release_handle(self)

    def __enter__(self) -> "SnapshotHandle":
        self.ensure_valid()
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        self.release()


class MaterialLibrary:
    def __init__(
        self,
        snapshot: PublishedSnapshot,
        *,
        snapshot_path: str | Path | None = None,
        trace_path: str | Path | None = None,
        pending_path: str | Path | None = None,
        max_publish_block_ms: float = 25.0,
    ) -> None:
        self._snapshot = snapshot
        self._snapshot_path = Path(snapshot_path) if snapshot_path else None
        self._trace_path = Path(trace_path) if trace_path else None
        self._pending_path = (
            Path(pending_path)
            if pending_path
            else (
                self._snapshot_path.parent / "pending.json"
                if self._snapshot_path is not None
                else None
            )
        )
        self._pointer_lock = threading.Lock()
        self._persistence_lock = threading.Lock()
        self._job_guard = threading.Lock()
        self._handle_lock = threading.Lock()
        self._handles: dict[int, SnapshotHandle] = {}
        self._handle_counts: dict[int, int] = {}
        self._retained: dict[int, PublishedSnapshot] = {}
        self._next_handle_id = 1
        self._closed = False
        self.max_publish_block_ms = float(max_publish_block_ms)
        self.max_observed_pointer_wait_ms = 0.0

    @classmethod
    def create(
        cls,
        definitions: Iterable[MaterialDefinition],
        pipeline_config: PipelineConfig,
        *,
        library_id: str,
        seed: int,
        snapshot_path: str | Path | None = None,
        trace_path: str | Path | None = None,
        pending_path: str | Path | None = None,
        ui_state: Mapping[str, Any] | None = None,
    ) -> "MaterialLibrary":
        definition_map = {item.material_id: item for item in definitions}
        if not definition_map:
            raise material_invalid("materials", "at least one material is required")
        pipeline = ColorPipeline.from_config(pipeline_config)
        histories: dict[str, MaterialHistory] = {}
        for material_id in sorted(definition_map):
            definition = definition_map[material_id]
            dependencies = pipeline.dependencies(definition.params, pipeline_config)
            result = pipeline.compute(definition.params, pipeline_config)
            record = make_version_record(
                material_id=material_id,
                version=1,
                parent_version=None,
                params=definition.params,
                dependencies=result.dependencies,
                colors=result.colors,
                intermediates=result.intermediates,
                previous_chain_hash="",
            )
            histories[material_id] = MaterialHistory(material_id, (record,))
        records = {
            material_id: history.current()
            for material_id, history in histories.items()
        }
        publication = make_publication_entry(
            generation=1,
            previous_publication_hash="",
            pipeline_hash=pipeline_config.config_hash(),
            records=records,
        )
        first_id = sorted(histories)[0]
        selected_ui_state = dict(
            ui_state
            or {
                "selected_material": first_id,
                "selected_version": 1,
                "timeline_material": first_id,
            }
        )
        snapshot = PublishedSnapshot(
            library_id=library_id,
            seed=seed,
            generation=1,
            previous_generation=0,
            publication_hash=publication.publication_hash,
            previous_publication_hash="",
            pipeline=pipeline_config,
            histories=freeze_value(histories),
            publication_chain=(publication,),
            ui_state=freeze_value(selected_ui_state),
        )
        library = cls(
            snapshot,
            snapshot_path=snapshot_path,
            trace_path=trace_path,
            pending_path=pending_path,
        )
        if snapshot_path:
            digest = save_snapshot(snapshot_path, snapshot)
            save_recovery_anchor(snapshot_path, snapshot)
            library._append_trace(
                TransitionResult(
                    operation="initial_publish",
                    generation=1,
                    previous_generation=0,
                    affected_material_ids=tuple(sorted(histories)),
                    affected_versions={
                        material_id: 1 for material_id in sorted(histories)
                    },
                    reused_versions={},
                    recomputed_count=len(histories),
                    reused_count=0,
                    max_channel_deltas={},
                    publication_hash=snapshot.publication_hash,
                    previous_publication_hash="",
                    snapshot_hash=digest,
                    ui_state=snapshot.ui_state,
                    materials=snapshot.current_materials(),
                    pipeline=snapshot.pipeline.to_dict(),
                ).trace_payload()
            )
        return library

    @classmethod
    def from_snapshot(
        cls,
        snapshot: PublishedSnapshot,
        *,
        snapshot_path: str | Path | None = None,
        trace_path: str | Path | None = None,
        pending_path: str | Path | None = None,
    ) -> "MaterialLibrary":
        return cls(
            snapshot,
            snapshot_path=snapshot_path,
            trace_path=trace_path,
            pending_path=pending_path,
        )

    @property
    def current_snapshot(self) -> PublishedSnapshot:
        with self._pointer_lock:
            return self._snapshot

    def acquire_snapshot(self) -> SnapshotHandle:
        started = time.perf_counter()
        with self._pointer_lock:
            if self._closed:
                raise snapshot_stale()
            snapshot = self._snapshot
            snapshot_key = id(snapshot)
            handle_id = self._next_handle_id
            self._next_handle_id += 1
            self._handle_counts[snapshot_key] = (
                self._handle_counts.get(snapshot_key, 0) + 1
            )
            self._retained[snapshot_key] = snapshot
            handle = SnapshotHandle(self, snapshot, handle_id)
            self._handles[handle_id] = handle
        elapsed_ms = (time.perf_counter() - started) * 1000.0
        self.max_observed_pointer_wait_ms = max(
            self.max_observed_pointer_wait_ms, elapsed_ms
        )
        return handle

    def read_snapshot(self) -> dict[str, dict[str, Any]]:
        with self.acquire_snapshot() as handle:
            return handle.current_materials()

    def _is_registered_handle(self, handle: SnapshotHandle) -> bool:
        with self._handle_lock:
            return self._handles.get(handle.handle_id) is handle

    def _release_handle(self, handle: SnapshotHandle) -> None:
        with self._handle_lock:
            self._handles.pop(handle.handle_id, None)
        with self._pointer_lock:
            key = id(handle.snapshot)
            count = self._handle_counts.get(key, 1) - 1
            if count > 0:
                self._handle_counts[key] = count
            else:
                self._handle_counts.pop(key, None)
                if id(self._snapshot) != key:
                    self._retained.pop(key, None)

    def _require_job_slot(self) -> None:
        if not self._job_guard.acquire(blocking=False):
            raise busy()

    def _release_job_slot(self) -> None:
        if self._job_guard.locked():
            self._job_guard.release()

    def write_pending(self, request: TransitionRequest) -> PendingJob:
        if self._pending_path is None:
            raise version_conflict("library has no pending journal path")
        base = self.current_snapshot
        pipeline = request.pipeline or base.pipeline
        pending = PendingJob(
            library_id=base.library_id,
            base_generation=base.generation,
            base_publication_hash=base.publication_hash,
            operation=request.operation,
            requested_pipeline=pipeline,
            material_update=request.material_update,
            rollback=request.rollback,
        )
        save_pending(self._pending_path, pending)
        return pending

    def prepare_transition(
        self,
        request: TransitionRequest,
        *,
        cancel_event: threading.Event | None = None,
    ) -> CandidatePublication:
        base = self.current_snapshot
        requested_pipeline = request.pipeline or base.pipeline
        pipeline = ColorPipeline.from_config(requested_pipeline)
        material_updates: dict[str, Mapping[str, Any]] = {}
        if request.material_update is not None:
            if not isinstance(request.material_update, Mapping):
                raise material_invalid("material_update", "must be an object")
            for material_id, params in request.material_update.items():
                if material_id not in base.histories:
                    raise material_invalid(
                        "material_update.material_id",
                        f"unknown material {material_id!r}",
                    )
                material_updates[str(material_id)] = params
        forced: set[str] = set(material_updates)
        rollback_params: dict[str, Mapping[str, Any]] = {}
        if request.rollback is not None:
            if not isinstance(request.rollback, Mapping):
                raise version_conflict("rollback request must be an object")
            material_id = str(request.rollback.get("material_id", ""))
            if material_id not in base.histories:
                raise version_conflict(
                    f"unknown material {material_id!r}", "rollback.material_id"
                )
            target_version = request.rollback.get("target_version")
            if isinstance(target_version, bool) or not isinstance(target_version, int):
                raise version_conflict(
                    "target_version must be an integer", "rollback.target_version"
                )
            expected_head_hash = request.rollback.get("expected_head_hash")
            current = base.current_record(material_id)
            if (
                expected_head_hash is not None
                and str(expected_head_hash) != current.content_hash
            ):
                raise version_conflict(
                    "material head hash changed; rollback was not applied",
                    "rollback.expected_head_hash",
                )
            try:
                target = base.histories[material_id].get(target_version)
            except KeyError as exc:
                raise version_conflict(
                    f"target material version {target_version} does not exist",
                    "rollback.target_version",
                ) from exc
            rollback_params[material_id] = target.params
            forced.add(material_id)
        new_histories: dict[str, MaterialHistory] = {}
        affected_ids: list[str] = []
        affected_versions: dict[str, int] = {}
        reused_versions: dict[str, int] = {}
        deltas: dict[str, Any] = {}
        for material_id in sorted(base.histories):
            history = base.histories[material_id]
            current = history.current()
            if material_id in rollback_params:
                desired_params = rollback_params[material_id]
            elif material_id in material_updates:
                desired_params = material_updates[material_id]
            else:
                desired_params = current.params
            dependencies = pipeline.dependencies(desired_params, requested_pipeline)
            is_affected = (
                material_id in forced or dependencies != current.dependencies
            )
            if is_affected:
                if cancel_event is not None and cancel_event.is_set():
                    from .errors import cancelled

                    raise cancelled()
                chain_result = pipeline.compute(
                    desired_params,
                    requested_pipeline,
                    cancel_check=(
                        cancel_event.is_set if cancel_event is not None else None
                    ),
                )
                record = make_version_record(
                    material_id=material_id,
                    version=current.version + 1,
                    parent_version=current.version,
                    params=desired_params,
                    dependencies=chain_result.dependencies,
                    colors=chain_result.colors,
                    intermediates=chain_result.intermediates,
                    previous_chain_hash=current.chain_hash,
                )
                new_histories[material_id] = MaterialHistory(
                    material_id, history.versions + (record,)
                )
                affected_ids.append(material_id)
                affected_versions[material_id] = record.version
                deltas[material_id] = max_channel_delta(
                    current.colors, record.colors
                )
            else:
                new_histories[material_id] = history
                reused_versions[material_id] = current.version
        if cancel_event is not None and cancel_event.is_set():
            from .errors import cancelled

            raise cancelled()
        records = {
            material_id: history.current()
            for material_id, history in new_histories.items()
        }
        generation = base.generation + 1
        chain_entry = make_publication_entry(
            generation=generation,
            previous_publication_hash=base.publication_hash,
            pipeline_hash=requested_pipeline.config_hash(),
            records=records,
        )
        snapshot = PublishedSnapshot(
            library_id=base.library_id,
            seed=base.seed,
            generation=generation,
            previous_generation=base.generation,
            publication_hash=chain_entry.publication_hash,
            previous_publication_hash=base.publication_hash,
            pipeline=requested_pipeline,
            histories=freeze_value(new_histories),
            publication_chain=base.publication_chain + (chain_entry,),
            ui_state=base.ui_state,
        )
        result = TransitionResult(
            operation=request.operation,
            generation=generation,
            previous_generation=base.generation,
            affected_material_ids=tuple(affected_ids),
            affected_versions=freeze_value(affected_versions),
            reused_versions=freeze_value(reused_versions),
            recomputed_count=len(affected_ids),
            reused_count=len(reused_versions),
            max_channel_deltas=freeze_value(deltas),
            publication_hash=snapshot.publication_hash,
            previous_publication_hash=snapshot.previous_publication_hash,
            snapshot_hash="",
            ui_state=snapshot.ui_state,
            materials=snapshot.current_materials(),
            pipeline=snapshot.pipeline.to_dict(),
        )
        return CandidatePublication(
            snapshot=snapshot,
            base_generation=base.generation,
            base_publication_hash=base.publication_hash,
            result=result,
        )

    def publish_candidate(self, candidate: CandidatePublication) -> TransitionResult:
        with self._persistence_lock:
            with self._pointer_lock:
                if (
                    self._snapshot.generation != candidate.base_generation
                    or self._snapshot.publication_hash
                    != candidate.base_publication_hash
                ):
                    raise version_conflict(
                        "library head changed before publication; candidate discarded"
                    )
                current = self._snapshot
            if current.ui_state != candidate.snapshot.ui_state:
                latest_snapshot = replace(
                    candidate.snapshot, ui_state=current.ui_state
                )
                candidate = replace(
                    candidate,
                    snapshot=latest_snapshot,
                    result=replace(candidate.result, ui_state=current.ui_state),
                )
            digest = ""
            if self._snapshot_path is not None:
                digest = save_snapshot(self._snapshot_path, candidate.snapshot)
                save_recovery_anchor(self._snapshot_path, candidate.snapshot)
            else:
                digest = sha256_bytes(snapshot_bytes(candidate.snapshot))
            started = time.perf_counter()
            with self._pointer_lock:
                if (
                    self._snapshot.generation != candidate.base_generation
                    or self._snapshot.publication_hash
                    != candidate.base_publication_hash
                ):
                    raise version_conflict(
                        "library head changed during publication; candidate discarded"
                    )
                self._snapshot = candidate.snapshot
                current_key = id(current)
                if self._handle_counts.get(current_key, 0) == 0:
                    self._retained.pop(current_key, None)
            elapsed_ms = (time.perf_counter() - started) * 1000.0
            self.max_observed_pointer_wait_ms = max(
                self.max_observed_pointer_wait_ms, elapsed_ms
            )
            if self._pending_path is not None:
                self._pending_path.unlink(missing_ok=True)
            result = replace(candidate.result, snapshot_hash=digest)
            self._append_trace(result.trace_payload())
            return result

    def run_transition_sync(
        self,
        request: TransitionRequest,
        *,
        cancel_event: threading.Event | None = None,
        write_pending: bool = True,
    ) -> TransitionResult:
        self._require_job_slot()
        pending_written = False
        try:
            if write_pending and self._pending_path is not None:
                self.write_pending(request)
                pending_written = True
            candidate = self.prepare_transition(
                request, cancel_event=cancel_event
            )
            return self.publish_candidate(candidate)
        except BusinessError:
            if pending_written and self._pending_path is not None:
                self._pending_path.unlink(missing_ok=True)
            raise
        finally:
            self._release_job_slot()

    def resume_pending(self, pending: PendingJob) -> TransitionResult:
        request = TransitionRequest(
            operation=pending.operation,
            pipeline=pending.requested_pipeline,
            material_update=pending.material_update,
            rollback=pending.rollback,
        )
        return self.run_transition_sync(request, write_pending=True)

    def material_update_params(
        self, material_id: str, descriptor: Mapping[str, Any]
    ) -> Mapping[str, Any]:
        from .materials import params_from_single_descriptor

        if material_id not in self.current_snapshot.histories:
            raise material_invalid(
                "material_id", f"unknown material {material_id!r}"
            )
        parsed = params_from_single_descriptor(
            descriptor, self.current_snapshot.pipeline.grid, path="material"
        )
        if parsed.material_id != material_id:
            raise material_invalid(
                "material.id", "descriptor id does not match edited material"
            )
        return parsed.params

    def diff_versions(
        self, material_id: str, from_version: int, to_version: int
    ) -> dict[str, Any]:
        snapshot = self.current_snapshot
        try:
            history = snapshot.histories[material_id]
        except KeyError as exc:
            raise material_invalid(
                "material_id", f"unknown material {material_id!r}"
            ) from exc
        try:
            old = history.get(from_version)
            new = history.get(to_version)
        except KeyError as exc:
            raise version_conflict(
                "requested version does not exist", "version"
            ) from exc
        return diff_versions(old, new)

    def update_ui_state(self, **changes: Any) -> PublishedSnapshot:
        with self._persistence_lock:
            current = self.current_snapshot
            next_state = dict(current.ui_state)
            next_state.update(changes)
            updated = replace(current, ui_state=freeze_value(next_state))
            if self._snapshot_path is not None:
                save_snapshot(self._snapshot_path, updated)
            with self._pointer_lock:
                if self._snapshot is not current:
                    raise version_conflict(
                        "library changed while updating UI state; state not written"
                    )
                self._snapshot = updated
            return updated

    def _append_trace(self, payload: Mapping[str, Any]) -> None:
        if self._trace_path is None:
            return
        self._trace_path.parent.mkdir(parents=True, exist_ok=True)
        line = canonical_bytes(payload) + b"\n"
        with self._trace_path.open("ab") as handle:
            handle.write(line)
            handle.flush()

    def close(self) -> None:
        with self._pointer_lock:
            self._closed = True
            self._retained.clear()
            self._handle_counts.clear()
        with self._handle_lock:
            handles = list(self._handles.values())
            self._handles.clear()
        for handle in handles:
            handle._released = True


def load_library(
    snapshot_path: str | Path,
    *,
    trace_path: str | Path | None = None,
    pending_path: str | Path | None = None,
) -> tuple[MaterialLibrary, PendingJob | None]:
    from .persistence import load_snapshot, reconcile_pending

    snapshot = load_snapshot(snapshot_path)
    resolved_pending_path = (
        Path(pending_path)
        if pending_path is not None
        else Path(snapshot_path).parent / "pending.json"
    )
    pending = reconcile_pending(snapshot, resolved_pending_path)
    library = MaterialLibrary.from_snapshot(
        snapshot,
        snapshot_path=snapshot_path,
        trace_path=trace_path,
        pending_path=resolved_pending_path,
    )
    return library, pending
