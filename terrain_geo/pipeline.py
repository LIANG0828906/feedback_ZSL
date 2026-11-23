"""Dependency-aware incremental scheduler with atomic cache replacement."""

from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass

import numpy as np

from .errors import GeoError, ErrorCode
from .hashing import image_hash
from .persistence import SnapshotStore
from .stages import execute_stage, stage_cache_key


@dataclass
class StageStatus:
    stage_id: str
    state: str                 # reused | recomputed | failed
    cache_key: str
    reason: str
    output: object = None


@dataclass
class PipelineResult:
    output: np.ndarray
    statuses: list
    commit_sequence: list
    cache: dict
    source_hash: str

    def invalidation_listing(self) -> list:
        return [{"stage_id": s.stage_id, "state": s.state, "reason": s.reason}
                for s in self.statuses]


class Pipeline:
    """Linear dependency chain of stages with content-addressed cache.

    max_workers bounds tile-level parallelism; shared cache writes and
    snapshot commits are serialized and ordered by the stage topology.
    """

    def __init__(self, stages: list, workdir: str, schema_version: str,
                 max_workers: int = 4, store: SnapshotStore | None = None):
        if max_workers < 1:
            raise GeoError(ErrorCode.E_PARA, "max_workers must be >= 1",
                           field="max_workers")
        self.stages = stages
        self.max_workers = max_workers
        self.store = store or SnapshotStore(workdir, schema_version)
        self._cache = {}          # cache_key -> output (committed, immutable)
        self._cache_lock = threading.Lock()
        self.fault_inject = {}    # stage_id -> exception to raise once
        self.reference_mutator = None  # test hook for E_DIFF localization

    # ------------------------------------------------------------------
    def set_fault(self, stage_id: str, exc: Exception) -> None:
        """Test hook: raise exc inside the named stage on its next run."""
        self.fault_inject[stage_id] = exc

    def clear_cache(self) -> None:
        with self._cache_lock:
            self._cache.clear()

    @property
    def cache(self):
        # Return copies so callers can never silently mutate committed output.
        return {key: value.copy() for key, value in self._cache.items()}

    def committed_output(self, cache_key: str):
        """Read-only access to a committed stage output (copied)."""
        return self._cache[cache_key].copy()

    def update_stage_params(self, stage_id: str, params: dict,
                            param_version: str) -> None:
        """Replace a stage's parameters; downstream stages get invalidated."""
        for stage in self.stages:
            if stage.stage_id == stage_id:
                stage.params = dict(params)
                stage.param_version = param_version
                return
        raise GeoError(ErrorCode.E_PARA,
                       f"unknown stage_id {stage_id}", field="stage_id")

    # ------------------------------------------------------------------
    def run(self, source: np.ndarray, persist: bool = True
            ) -> PipelineResult:
        source_hash = image_hash(source)
        restored = self._try_restore(source, source_hash, source.shape)
        statuses = []
        run_commits = []
        current = source
        upstream_key = None
        input_hash = source_hash
        executor = ThreadPoolExecutor(max_workers=self.max_workers)
        try:
            for index, stage in enumerate(self.stages):
                key = stage_cache_key(stage, input_hash, upstream_key)
                if restored is not None and restored[index]["cache_key"] == key:
                    output = restored[index]["output"]
                    with self._cache_lock:
                        self._cache[key] = output
                    statuses.append(StageStatus(
                        stage.stage_id, "reused", key,
                        "snapshot hit (content hash + param version verified)"))
                elif key in self._cache:
                    output = self._cache[key]
                    statuses.append(StageStatus(
                        stage.stage_id, "reused", key,
                        "in-memory cache hit"))
                else:
                    reason = self._invalidation_reason(
                        stage, input_hash, upstream_key, index)
                    output = self._recompute(stage, current, executor)
                    with self._cache_lock:
                        self._cache[key] = output
                    run_commits.append(stage.stage_id)
                    statuses.append(StageStatus(
                        stage.stage_id, "recomputed", key, reason, output))
                current = output
                upstream_key = key
                input_hash = image_hash(output)
        finally:
            executor.shutdown(wait=True)
        if persist and run_commits:
            # Serialized, stage-ordered commit of newly computed outputs.
            self.store.write_snapshot(source_hash, self._all_manifest_entries(
                source_hash, statuses))
        return PipelineResult(
            output=current,
            statuses=statuses,
            commit_sequence=run_commits,
            cache=self.cache,
            source_hash=source_hash)

    # ------------------------------------------------------------------
    def _recompute(self, stage, image, executor):
        injected = self.fault_inject.pop(stage.stage_id, None)
        if injected is not None:
            if isinstance(injected, GeoError):
                raise injected
            raise GeoError(ErrorCode.E_INTERNAL, str(injected),
                          stage=stage.stage_id)
        try:
            return execute_stage(stage, image, executor)
        except GeoError:
            raise

    def _invalidation_reason(self, stage, input_hash, upstream_key, index):
        if index == 0:
            return "no cache entry for current input/params"
        return (f"upstream change (input_hash={input_hash[:10]}..., "
                f"upstream_key={str(upstream_key)[:10]}...) or new "
                f"param_version={stage.param_version}")

    def _try_restore(self, source, source_hash, source_shape):
        try:
            return self.store.load_snapshot(
                source_hash, self.stages, source_shape)
        except GeoError:
            return None

    def _all_manifest_entries(self, source_hash, statuses):
        """Rebuild ordered entries from the committed cache for one snapshot."""
        entries = []
        upstream_key = None
        input_hash = source_hash
        for stage, status in zip(self.stages, statuses):
            entries.append({
                "stage_id": stage.stage_id,
                "stage": stage,
                "output": status.output if status.output is not None
                else self._cache[status.cache_key],
                "input_hash": input_hash,
                "upstream_key": upstream_key,
                "cache_key": status.cache_key,
                "param_version": stage.param_version,
            })
            upstream_key = status.cache_key
            input_hash = image_hash(entries[-1]["output"])
        return entries

    # ------------------------------------------------------------------
    def run_full_recompute(self, source: np.ndarray) -> PipelineResult:
        """Cold-cache reference path used by the consistency comparison."""
        cold = Pipeline(list(_clone_stages(self.stages)),
                        workdir="",
                        max_workers=self.max_workers,
                        store=_NullStore(),
                        schema_version=self.store.schema_version)
        return cold.run(source, persist=False)

    def assert_matches_full(self, source: np.ndarray,
                            result: PipelineResult) -> tuple:
        """Compare an incremental result against a cold full recomputation.

        Both final images and every stage output are compared; the first
        divergent pixel and owning stage are reported via E_DIFF.
        """
        full = self.run_full_recompute(source)
        inc, ref = result.output, full.output
        if inc.shape != ref.shape or inc.dtype != ref.dtype:
            raise GeoError(
                ErrorCode.E_DIFF,
                f"shape/dtype mismatch inc={inc.shape}/{inc.dtype} "
                f"full={ref.shape}/{ref.dtype}")
        if not np.array_equal(inc, ref):
            first = np.argwhere(inc != ref)[0]
            raise GeoError(
                ErrorCode.E_DIFF,
                f"first inconsistent pixel at "
                f"{tuple(int(v) for v in first)} owning stage="
                f"{self.stages[-1].stage_id}",
                stage=self.stages[-1].stage_id,
                detail={"first_pixel": [int(v) for v in first]})
        # Per-stage replay: parallel vs single-thread full recomputation.
        serial = Pipeline(list(_clone_stages(self.stages)),
                          workdir="",
                          max_workers=1, store=_NullStore(),
                          schema_version=self.store.schema_version)
        if self.reference_mutator is not None:
            serial.reference_mutator = self.reference_mutator
        serial_result = serial.run(source, persist=False)
        for status, serial_status in zip(result.statuses,
                                         serial_result.statuses):
            a = self._cache.get(status.cache_key)
            b = serial_status.output
            if self.reference_mutator is not None:
                b = self.reference_mutator(status.stage_id, b)
            if a is None or not np.array_equal(a, b):
                first = (np.argwhere(a != b)[0] if a is not None
                         else np.array([0, 0]))
                raise GeoError(
                    ErrorCode.E_DIFF,
                    f"stage {status.stage_id} first inconsistent pixel at "
                    f"{tuple(int(v) for v in first)}",
                    stage=status.stage_id,
                    detail={"first_pixel": [int(v) for v in first]})
        return True, None


class _NullStore(SnapshotStore):
    def __init__(self):
        self.dir = ""
        self.schema_version = "null"
        self._lock = threading.Lock()

    def write_snapshot(self, *args, **kwargs):
        return ""

    def load_snapshot(self, *args, **kwargs):
        return None


def _clone_stages(stages):
    import copy
    return [copy.deepcopy(stage) for stage in stages]
