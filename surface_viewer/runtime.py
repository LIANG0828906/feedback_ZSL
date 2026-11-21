from __future__ import annotations

import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Mapping

from .concurrency import (
    RecomputeCoordinator,
    RecomputeTask,
    SnapshotHandle,
    SnapshotStore,
)
from .engine import (
    IncrementalSurfaceEngine,
    ResourceLimits,
    UpdateResult,
)
from .errors import ErrorCode, SurfaceViewerError
from .model import SurfaceAssembly
from .store import ArtifactStore, CommitInfo, RecoveryState


@dataclass(frozen=True)
class RuntimeUpdate:
    result: UpdateResult
    commit: CommitInfo
    handle: SnapshotHandle


@dataclass(frozen=True)
class BootstrapResult:
    recovery: RecoveryState
    update: RuntimeUpdate
    reused_from_disk: int
    recomputed_after_recovery: int


class SurfaceRuntime:
    def __init__(
        self,
        engine: IncrementalSurfaceEngine,
        store: ArtifactStore,
        *,
        commit_seq: int,
        chain_head: str,
        snapshot: SnapshotHandle,
        trace_callback: Callable[[RuntimeUpdate], None] | None = None,
    ):
        self.engine = engine
        self.store = store
        self.snapshots = SnapshotStore()
        self.coordinator = RecomputeCoordinator()
        self._trace_callback = trace_callback
        self._update_lock = threading.Lock()
        self._commit_seq = commit_seq
        self._chain_head = chain_head
        self._snapshot = snapshot

    @classmethod
    def bootstrap(
        cls,
        assembly: SurfaceAssembly,
        root: str | Path,
        *,
        preview_backend: str = "software",
        resource_limits: ResourceLimits | None = None,
        verify_full: bool = True,
        trace_callback: Callable[[RuntimeUpdate], None] | None = None,
    ) -> "SurfaceRuntime":
        store = ArtifactStore(root)
        recovery = store.recover(expected_assembly_id=assembly.assembly_id)

        if recovery.commit_seq == 0:
            engine = IncrementalSurfaceEngine(
                assembly,
                preview_backend=preview_backend,
                resource_limits=resource_limits,
            )
            update_result = engine.initialize(verify_full=verify_full)
            reused_from_disk = 0
        else:
            engine, update_result = IncrementalSurfaceEngine.recover(
                assembly,
                recovery.artifacts,
                preview_backend=preview_backend,
                resource_limits=resource_limits,
                verify_full=verify_full,
            )
            reused_from_disk = len(update_result.reused_ids)

        commit = store.commit(
            assembly,
            engine.graph,
            engine.cache,
            expected_previous_commit_seq=recovery.commit_seq,
        )
        snapshot_store = SnapshotStore()
        handle = snapshot_store.publish(
            commit_seq=commit.commit_seq,
            assembly=engine.assembly,
            graph=engine.graph,
            artifacts=engine.cache,
            screen_preview=engine.screen_preview,
            chain_head=commit.chain_head,
        )
        runtime = cls(
            engine,
            store,
            commit_seq=commit.commit_seq,
            chain_head=commit.chain_head,
            snapshot=handle,
            trace_callback=trace_callback,
        )
        runtime.snapshots = snapshot_store
        runtime_update = RuntimeUpdate(
            result=update_result, commit=commit, handle=handle
        )
        runtime._bootstrap = BootstrapResult(
            recovery=recovery,
            update=runtime_update,
            reused_from_disk=reused_from_disk,
            recomputed_after_recovery=len(update_result.recomputed_ids),
        )
        if trace_callback is not None:
            trace_callback(runtime_update)
        return runtime

    @property
    def bootstrap_result(self) -> BootstrapResult:
        return self._bootstrap

    @property
    def commit_seq(self) -> int:
        return self._commit_seq

    @property
    def chain_head(self) -> str:
        return self._chain_head

    def current(self) -> SnapshotHandle:
        return self.snapshots.current()

    def update(
        self,
        patch_id: str,
        changes: Mapping[str, object],
        *,
        verify_full: bool = True,
    ) -> RuntimeUpdate:
        if not self._update_lock.acquire(blocking=False):
            raise SurfaceViewerError(
                ErrorCode.BUSY,
                "a recompute task is already running for this assembly",
                field=self.engine.assembly.assembly_id,
            )
        try:
            return self._perform_update(patch_id, changes, verify_full)
        finally:
            self._update_lock.release()

    def submit_update(
        self,
        patch_id: str,
        changes: Mapping[str, object],
        *,
        verify_full: bool = True,
    ) -> RecomputeTask:
        assembly_id = self.engine.assembly.assembly_id

        def operation() -> RuntimeUpdate:
            if not self._update_lock.acquire(blocking=False):
                raise SurfaceViewerError(
                    ErrorCode.BUSY,
                    "a recompute task is already running for this assembly",
                    field=assembly_id,
                )
            try:
                return self._perform_update(patch_id, changes, verify_full)
            finally:
                self._update_lock.release()

        return self.coordinator.submit(assembly_id, operation)

    def _perform_update(
        self,
        patch_id: str,
        changes: Mapping[str, object],
        verify_full: bool,
    ) -> RuntimeUpdate:
        old_state = (
            self.engine.assembly,
            self.engine.graph,
            self.engine.topology,
            self.engine.geometry,
            self.engine.cache,
            self.engine.screen_preview,
        )
        result = self.engine.update(
            patch_id, changes, verify_full=verify_full
        )
        try:
            commit = self.store.commit(
                self.engine.assembly,
                self.engine.graph,
                self.engine.cache,
                expected_previous_commit_seq=self._commit_seq,
            )
        except BaseException:
            (
                self.engine.assembly,
                self.engine.graph,
                self.engine.topology,
                self.engine.geometry,
                self.engine.cache,
                self.engine.screen_preview,
            ) = old_state
            raise

        handle = self.snapshots.publish(
            commit_seq=commit.commit_seq,
            assembly=self.engine.assembly,
            graph=self.engine.graph,
            artifacts=self.engine.cache,
            screen_preview=self.engine.screen_preview,
            chain_head=commit.chain_head,
        )
        self._commit_seq = commit.commit_seq
        self._chain_head = commit.chain_head
        self._snapshot = handle
        update = RuntimeUpdate(result=result, commit=commit, handle=handle)
        if self._trace_callback is not None:
            self._trace_callback(update)
        return update
