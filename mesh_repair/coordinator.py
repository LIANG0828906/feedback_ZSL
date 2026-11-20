from __future__ import annotations

import hashlib
import json
import math
import os
import threading
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .binary import atomic_write_bytes, file_sha256, load_mesh, write_mesh
from .boundary import BoundaryLoop, compare_boundary_tolerances, extract_boundary_loops
from .budget import (
    BlockTask,
    BudgetScheduler,
    MemoryUsage,
    TaskResult,
    TraceWriter,
    estimate_memory,
    measure_mesh,
)
from .commit import (
    BLOCK_TABLE_VERSION,
    CommitStore,
    SessionLock,
    sha256_json,
)
from .errors import (
    BusinessError,
    E_BOUNDARY_PENDING,
    E_BUDGET_EXCEEDED,
    E_BUSY,
    E_CANCELLED,
    E_CONFIG_INVALID,
    E_RECOVERY_INVALID,
    SUB_CORRUPT_COMMIT,
    SUB_INVALID_PARAMETER,
)
from .evaluate import evaluate_repair
from .mesh import Mesh
from .repair import (
    FillResult,
    FragmentResult,
    NormalResult,
    SplitResult,
    compute_normals,
    fill_boundary_loops,
    remove_isolated_fragments,
    split_nonmanifold_edges,
)
from .snapshot import SnapshotStore
from .topology import Topology, build_topology
from .weld import WeldResult, make_cross_block_weld_map, weld_vertices


PAYLOAD_VERSION = 1


@dataclass(frozen=True)
class RepairConfig:
    tolerance: float = 1e-6
    block_shape: tuple[int, int, int] = (2, 2, 1)
    parallelism: int = 2
    global_budget_bytes: int = 8 * 1024 * 1024
    task_budget_bytes: int = 4 * 1024 * 1024
    min_fragment_faces: int = 3
    area_epsilon: float = 1e-12
    normal_tolerance_deg: float = 30.0

    def validate(self) -> "RepairConfig":
        try:
            tolerance = float(self.tolerance)
            normal_tolerance = float(self.normal_tolerance_deg)
            area_epsilon = float(self.area_epsilon)
        except (TypeError, ValueError) as exc:
            raise _config_error("numeric repair parameters must be finite") from exc
        if not math.isfinite(tolerance) or tolerance < 0:
            raise _config_error("tolerance must be finite and non-negative", "tolerance")
        if not math.isfinite(normal_tolerance) or normal_tolerance <= 0:
            raise _config_error(
                "normal_tolerance_deg must be finite and positive",
                "normal_tolerance_deg",
            )
        if not math.isfinite(area_epsilon) or area_epsilon <= 0:
            raise _config_error(
                "area_epsilon must be finite and positive", "area_epsilon"
            )
        shape = tuple(int(value) for value in self.block_shape)
        if len(shape) != 3 or any(value < 1 or value > 32 for value in shape):
            raise _config_error(
                "block_shape must contain three integers in [1, 32]", "block_shape"
            )
        if int(self.parallelism) < 1 or int(self.parallelism) > 64:
            raise _config_error(
                "parallelism must be in [1, 64]", "parallelism"
            )
        if int(self.global_budget_bytes) <= 0:
            raise _config_error(
                "global_budget_bytes must be positive", "global_budget_bytes"
            )
        if int(self.task_budget_bytes) <= 0:
            raise _config_error(
                "task_budget_bytes must be positive", "task_budget_bytes"
            )
        if int(self.min_fragment_faces) < 1:
            raise _config_error(
                "min_fragment_faces must be at least one", "min_fragment_faces"
            )
        return RepairConfig(
            tolerance=tolerance,
            block_shape=shape,
            parallelism=int(self.parallelism),
            global_budget_bytes=int(self.global_budget_bytes),
            task_budget_bytes=int(self.task_budget_bytes),
            min_fragment_faces=int(self.min_fragment_faces),
            area_epsilon=area_epsilon,
            normal_tolerance_deg=normal_tolerance,
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "tolerance": self.tolerance,
            "block_shape": list(self.block_shape),
            "parallelism": self.parallelism,
            "global_budget_bytes": self.global_budget_bytes,
            "task_budget_bytes": self.task_budget_bytes,
            "min_fragment_faces": self.min_fragment_faces,
            "area_epsilon": self.area_epsilon,
            "normal_tolerance_deg": self.normal_tolerance_deg,
        }

    @property
    def config_hash(self) -> str:
        return sha256_json(self.to_dict())

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "RepairConfig":
        shape = tuple(data.get("block_shape", (2, 2, 1)))
        return cls(
            tolerance=float(data.get("tolerance", 1e-6)),
            block_shape=shape,
            parallelism=int(data.get("parallelism", 2)),
            global_budget_bytes=int(data.get("global_budget_bytes", 8 * 1024 * 1024)),
            task_budget_bytes=int(data.get("task_budget_bytes", 4 * 1024 * 1024)),
            min_fragment_faces=int(data.get("min_fragment_faces", 3)),
            area_epsilon=float(data.get("area_epsilon", 1e-12)),
            normal_tolerance_deg=float(data.get("normal_tolerance_deg", 30.0)),
        ).validate()


def _config_error(message: str, field_name: str | None = None) -> BusinessError:
    return BusinessError(
        E_CONFIG_INVALID,
        message,
        subcode=SUB_INVALID_PARAMETER,
        location={"field": field_name} if field_name else None,
    )


@dataclass
class BlockPlan:
    index: int
    block_id: str
    cell: tuple[int, int, int]
    initial_face_ids: np.ndarray
    planned_usage: MemoryUsage


class BoundaryRegistry:
    """Committed weld-map readiness used by cross-block hole filling."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._maps: dict[int, dict[str, Any]] = {}

    def register(self, block_index: int, mapping: dict[str, Any]) -> None:
        with self._lock:
            self._maps[block_index] = mapping

    def check(self, required: Any) -> tuple[bool, list[int]]:
        with self._lock:
            missing = sorted(int(block) for block in required if int(block) not in self._maps)
        return not missing, missing

    def snapshot(self) -> dict[int, dict[str, Any]]:
        with self._lock:
            return dict(self._maps)


@dataclass
class BackgroundRepair:
    service: "RepairService"
    input_path: str | os.PathLike[str]
    output_path: str | os.PathLike[str]
    report_path: str | os.PathLike[str] | None
    config: RepairConfig
    snapshot_store: SnapshotStore = field(default_factory=SnapshotStore)
    cancel_event: threading.Event = field(default_factory=threading.Event)
    _thread: threading.Thread | None = None
    result: dict[str, Any] | None = None
    error: BusinessError | None = None

    def start(self) -> "BackgroundRepair":
        self._thread = threading.Thread(
            target=self._run,
            name="mesh-repair-background",
            daemon=False,
        )
        self._thread.start()
        return self

    def _run(self) -> None:
        try:
            self.result = self.service.repair_file(
                self.input_path,
                self.output_path,
                config=self.config,
                report_path=self.report_path,
                cancel_event=self.cancel_event,
                snapshot_store=self.snapshot_store,
            )
        except BusinessError as exc:
            self.error = exc
        except Exception as exc:  # surfaced to join caller
            self.error = BusinessError(
                "E_INTERNAL",
                f"background repair failed: {type(exc).__name__}: {exc}",
            )

    def cancel(self) -> None:
        self.cancel_event.set()

    def join(self, timeout: float | None = None) -> dict[str, Any]:
        if self._thread is None:
            raise RuntimeError("background repair was not started")
        self._thread.join(timeout)
        if self._thread.is_alive():
            raise TimeoutError("background repair is still running")
        if self.error is not None:
            raise self.error
        if self.result is None:
            raise RuntimeError("background repair ended without a result")
        return self.result


class RepairService:
    def __init__(self, work_dir: str | os.PathLike[str] = "repair-work") -> None:
        self.work_dir = Path(work_dir)
        self.work_dir.mkdir(parents=True, exist_ok=True)
        self._active_lock = threading.Lock()
        self._active_sources: set[str] = set()

    def start_background(
        self,
        input_path: str | os.PathLike[str],
        output_path: str | os.PathLike[str],
        *,
        config: RepairConfig | None = None,
        report_path: str | os.PathLike[str] | None = None,
        snapshot_store: SnapshotStore | None = None,
    ) -> BackgroundRepair:
        return BackgroundRepair(
            service=self,
            input_path=input_path,
            output_path=output_path,
            report_path=report_path,
            config=(config or RepairConfig()).validate(),
            snapshot_store=snapshot_store or SnapshotStore(),
        ).start()

    def repair_file(
        self,
        input_path: str | os.PathLike[str],
        output_path: str | os.PathLike[str],
        *,
        config: RepairConfig | None = None,
        report_path: str | os.PathLike[str] | None = None,
        cancel_event: threading.Event | None = None,
        snapshot_store: SnapshotStore | None = None,
    ) -> dict[str, Any]:
        config = (config or RepairConfig()).validate()
        cancel_event = cancel_event or threading.Event()
        snapshot_store = snapshot_store or SnapshotStore()
        input_file = Path(input_path)
        output_file = Path(output_path)
        source_sha = file_sha256(input_file)
        with self._active_lock:
            if source_sha in self._active_sources:
                raise BusinessError(
                    E_BUSY,
                    "the same source mesh is already being repaired",
                    context={"source_sha256": source_sha},
                )
            self._active_sources.add(source_sha)
        lock = SessionLock(self.work_dir / "locks" / f"{source_sha}.lock")
        try:
            with lock:
                return self._repair_locked(
                    input_file=input_file,
                    output_file=output_file,
                    source_sha=source_sha,
                    config=config,
                    report_path=report_path,
                    cancel_event=cancel_event,
                    snapshot_store=snapshot_store,
                )
        finally:
            with self._active_lock:
                self._active_sources.discard(source_sha)

    def _repair_locked(
        self,
        *,
        input_file: Path,
        output_file: Path,
        source_sha: str,
        config: RepairConfig,
        report_path: str | os.PathLike[str] | None,
        cancel_event: threading.Event,
        snapshot_store: SnapshotStore,
    ) -> dict[str, Any]:
        run_id = f"{time.time_ns()}-{os.getpid()}-{uuid.uuid4().hex[:8]}"
        trace_path = self.work_dir / "traces" / f"trace-{run_id}.jsonl"
        trace = TraceWriter(trace_path, run_id=run_id)
        trace.emit(
            "run_start",
            source=str(input_file),
            source_sha256=source_sha,
            config=config.to_dict(),
            config_hash=config.config_hash,
        )
        original, header = load_mesh(input_file, area_epsilon=config.area_epsilon)
        trace.emit(
            "mesh_loaded",
            header=header.to_dict(),
            vertices=original.vertex_count,
            faces=original.face_count,
        )
        original_topology = build_topology(original)
        trace.emit("original_topology", **original_topology.summary())
        snapshot_store.publish(
            original.vertices,
            original.faces,
            face_block_ids=np.zeros(original.face_count, dtype=np.int64),
            metadata={
                "source_sha256": source_sha,
                "stage": "input",
            },
        )

        if cancel_event.is_set():
            raise BusinessError(E_CANCELLED, "repair canceled before welding")

        welded: WeldResult = weld_vertices(original, config.tolerance)
        trace.emit("weld_complete", **welded.summary(), groups=len(welded.groups))
        welded_topology = build_topology(welded.mesh)
        boundary_before_fill = extract_boundary_loops(welded.mesh, welded_topology)
        trace.emit("boundary_extract", **boundary_before_fill.to_dict())

        partition_bounds, blocks = self._partition_blocks(
            welded.mesh, config.block_shape
        )
        welded_face_block = np.full(welded.mesh.face_count, -1, dtype=np.int64)
        for plan in blocks:
            welded_face_block[plan.initial_face_ids] = plan.index
        trace.emit(
            "partition_complete",
            block_count=len(blocks),
            block_shape=list(config.block_shape),
            block_cells=[list(plan.cell) for plan in blocks],
        )

        block_table = self._build_block_table(blocks, welded.mesh)
        store = CommitStore(
            self._session_dir(
                source_sha=source_sha,
                config_hash=config.config_hash,
                block_table=block_table,
            ),
            source_sha256=source_sha,
            config_hash=config.config_hash,
            block_table=block_table,
        )
        manifest = store.initialize()
        precommitted = set(store.committed)
        trace.emit(
            "session_ready",
            session_id=store.session_id,
            session_dir=str(store.session_dir),
            manifest_hash=manifest.manifest_hash,
            block_table_hash=store.block_table_hash,
            block_table_version=BLOCK_TABLE_VERSION,
            committed_blocks=sorted(precommitted),
        )

        stage_a_tasks = [
            BlockTask(plan.block_id, plan.index, plan.planned_usage, plan)
            for plan in blocks
        ]
        scheduler_a = BudgetScheduler(
            global_budget_bytes=config.global_budget_bytes,
            task_budget_bytes=config.task_budget_bytes,
            parallelism=config.parallelism,
            trace=trace,
        )
        preflight_a = scheduler_a.preflight(stage_a_tasks)
        if preflight_a:
            raise next(iter(preflight_a.values()))

        registry = BoundaryRegistry()
        raw_face_to_welded_face = np.delete(
            np.arange(original.face_count, dtype=np.int64),
            np.asarray(welded.dropped_faces, dtype=np.int64),
        )
        blocks_of_raw_vertex: dict[int, set[int]] = {}
        for plan in blocks:
            for welded_face_id in plan.initial_face_ids:
                raw_face_id = int(raw_face_to_welded_face[int(welded_face_id)])
                for raw_vertex in original.faces[raw_face_id]:
                    blocks_of_raw_vertex.setdefault(int(raw_vertex), set()).add(plan.index)

        stage_a_maps: dict[int, dict[str, Any]] = {}

        def prepare_worker(task: BlockTask, task_cancel: threading.Event) -> dict[str, Any]:
            if task_cancel.is_set():
                raise BusinessError(
                    E_CANCELLED,
                    "block preparation canceled",
                    location={"block_id": task.block_id},
                )
            _acceptance_delay(task_cancel)
            plan: BlockPlan = task.payload
            local_mesh, global_vertex_ids = _local_mesh(
                welded.mesh, plan.initial_face_ids
            )
            local_topology = build_topology(local_mesh)
            local_weld = weld_vertices(local_mesh, config.tolerance)
            raw_ids = sorted(
                int(vertex)
                for vertex in np.unique(
                    original.faces[raw_face_to_welded_face[plan.initial_face_ids]].reshape(-1)
                )
            )
            weld_map = make_cross_block_weld_map(
                block_id=plan.index,
                raw_vertex_ids=raw_ids,
                old_to_new=welded.old_to_new,
                blocks_of_vertex=blocks_of_raw_vertex,
            )
            mapping = {
                "block_id": plan.block_id,
                "block_index": plan.index,
                "raw_vertex_count": len(raw_ids),
                "local_vertex_count": local_mesh.vertex_count,
                "local_to_global": [
                    [int(local_id), int(global_id)]
                    for local_id, global_id in enumerate(global_vertex_ids.tolist())
                ],
                "cross_boundary": weld_map["cross_boundary"],
                "map_hash": weld_map["map_hash"],
                "raw_weld_rows": weld_map["local_to_global"],
                "local_topology": local_topology.summary(),
                "local_weld": local_weld.summary(),
            }
            registry.register(plan.index, mapping)
            stage_a_maps[plan.index] = mapping
            return mapping

        results_a = scheduler_a.execute(stage_a_tasks, prepare_worker, cancel_event=cancel_event)
        _raise_first_task_error(results_a)
        missing_maps = registry.check(range(len(blocks)))[1]
        if missing_maps:
            raise BusinessError(
                E_BOUNDARY_PENDING,
                "block weld maps are not all committed",
                context={"missing_blocks": missing_maps},
            )
        trace.emit(
            "stage_a_complete",
            max_used_bytes=scheduler_a._max_used_bytes,
            max_active_blocks=scheduler_a._max_active,
        )
        aggregate_groups: dict[int, list[dict[str, Any]]] = {}
        for mapping in stage_a_maps.values():
            for row in mapping["raw_weld_rows"]:
                aggregate_groups.setdefault(int(row["welded_vertex"]), []).append(
                    {
                        "block_id": mapping["block_id"],
                        "block_index": int(mapping["block_index"]),
                        "local_vertex": int(row["local"]),
                        "source_vertex": int(row["source_vertex"]),
                    }
                )
        cross_block_weld_merges = [
            {
                "welded_vertex": welded_vertex,
                "endpoints": sorted(
                    endpoints,
                    key=lambda item: (
                        item["block_index"],
                        item["local_vertex"],
                        item["source_vertex"],
                    ),
                ),
            }
            for welded_vertex, endpoints in sorted(aggregate_groups.items())
            if len({endpoint["block_index"] for endpoint in endpoints}) > 1
        ]
        trace.emit(
            "cross_block_weld_maps_committed",
            merge_group_count=len(cross_block_weld_merges),
            merges=cross_block_weld_merges,
        )

        if cancel_event.is_set():
            raise BusinessError(E_CANCELLED, "repair canceled after block preparation")

        block_vertex_sets: dict[int, set[int]] = {}
        for plan in blocks:
            for face_id in plan.initial_face_ids:
                for vertex in welded.mesh.faces[int(face_id)]:
                    block_vertex_sets.setdefault(int(vertex), set()).add(plan.index)
        pre_fill_components = welded_topology.face_components()
        doomed_faces = {
            face_id
            for component in pre_fill_components
            if len(component) < config.min_fragment_faces
            for face_id in component
        }
        fill_loops: list[BoundaryLoop] = []
        skipped_fragment_loops: list[dict[str, Any]] = []
        for loop in boundary_before_fill.loops:
            loop_faces = {
                face_id
                for index, current in enumerate(loop.vertices)
                for face_id in welded_topology.edge_table.get(
                    (
                        min(current, loop.vertices[(index + 1) % loop.vertex_count]),
                        max(current, loop.vertices[(index + 1) % loop.vertex_count]),
                    ),
                    (),
                )
            }
            if loop_faces and loop_faces.issubset(doomed_faces):
                skipped_fragment_loops.append(
                    {"loop": loop.to_dict(), "face_ids": sorted(loop_faces)}
                )
                continue
            fill_loops.append(loop)
        loop_block_ids: dict[int, set[int]] = {}
        loop_owner: dict[int, int] = {}
        for loop in fill_loops:
            dependencies = sorted(
                {
                    block
                    for vertex in loop.vertices
                    for block in block_vertex_sets.get(int(vertex), set())
                }
            )
            loop_block_ids[loop.loop_id] = set(dependencies)
            loop_owner[loop.loop_id] = dependencies[0] if dependencies else 0
        trace.emit(
            "fill_dependencies",
            filled_loop_count=len(fill_loops),
            skipped_fragment_loop_count=len(skipped_fragment_loops),
            skipped_fragment_loops=skipped_fragment_loops,
            loops={
                str(loop_id): sorted(blocks_required)
                for loop_id, blocks_required in loop_block_ids.items()
            },
        )

        surviving_provenance = raw_face_to_welded_face.copy()
        fill_result = fill_boundary_loops(
            welded.mesh,
            fill_loops,
            face_provenance=surviving_provenance,
            loop_block_ids=loop_block_ids,
            readiness=registry.check,
        )
        trace.emit("hole_fill_complete", **fill_result.summary())

        face_block_ids = welded_face_block.copy()
        for loop_info in fill_result.loop_faces:
            owner = loop_owner[loop_info["loop_id"]]
            face_block_ids = np.concatenate(
                (
                    face_block_ids,
                    np.full(len(loop_info["new_face_ids"]), owner, dtype=np.int64),
                )
            )
        if face_block_ids.shape[0] != fill_result.mesh.face_count:
            raise RuntimeError("fill face-to-block assignment is inconsistent")

        split_result = split_nonmanifold_edges(
            fill_result.mesh, face_provenance=fill_result.face_provenance
        )
        trace.emit("nonmanifold_split_complete", **split_result.summary())
        fragment_result = remove_isolated_fragments(
            split_result.mesh,
            min_faces=config.min_fragment_faces,
            face_provenance=split_result.face_provenance,
        )
        trace.emit("fragment_removal_complete", **fragment_result.summary())
        face_block_ids = face_block_ids[fragment_result.kept_face_ids]
        final_mesh = fragment_result.mesh
        final_provenance = fragment_result.face_provenance
        normals = compute_normals(final_mesh)
        trace.emit("normals_recomputed", **normals.summary())

        if face_block_ids.shape[0] != final_mesh.face_count:
            raise RuntimeError("final face-to-block assignment is inconsistent")

        exact_block_tasks: list[BlockTask] = []
        exact_usages: dict[int, MemoryUsage] = {}
        for plan in blocks:
            face_ids = np.flatnonzero(face_block_ids == plan.index).astype(np.int64)
            local_mesh, _ = _local_mesh(final_mesh, face_ids)
            usage = measure_mesh(local_mesh)
            exact_usages[plan.index] = usage
            exact_block_tasks.append(
                BlockTask(plan.block_id, plan.index, usage, (plan, face_ids))
            )
        scheduler_b = BudgetScheduler(
            global_budget_bytes=config.global_budget_bytes,
            task_budget_bytes=config.task_budget_bytes,
            parallelism=config.parallelism,
            trace=trace,
        )
        preflight_b = scheduler_b.preflight(exact_block_tasks)
        if preflight_b:
            raise next(iter(preflight_b.values()))

        crash_after = _crash_after_commits()
        new_commit_count = 0
        commit_lock = threading.Lock()

        def finalize_worker(task: BlockTask, task_cancel: threading.Event) -> dict[str, Any]:
            nonlocal new_commit_count
            if task_cancel.is_set():
                raise BusinessError(
                    E_CANCELLED,
                    "block finalization canceled",
                    location={"block_id": task.block_id},
                )
            _acceptance_delay(task_cancel)
            if task_cancel.is_set():
                raise BusinessError(
                    E_CANCELLED,
                    "block finalization canceled at commit boundary",
                    location={"block_id": task.block_id},
                )
            plan, face_ids = task.payload
            payload = _make_block_payload(
                final_mesh=final_mesh,
                face_ids=face_ids,
                face_block_ids=face_block_ids,
                provenance=final_provenance,
                block_plan=plan,
                weld_map=stage_a_maps[plan.index],
                cross_block_weld_merges=cross_block_weld_merges,
                usage=exact_usages[plan.index],
            )
            was_committed = task.block_id in precommitted
            with commit_lock:
                record = store.commit_block(task.block_id, payload)
                if not was_committed:
                    new_commit_count += 1
                    current = new_commit_count
                    trace.emit(
                        "block_atomic_commit",
                        block_id=task.block_id,
                        sequence=record["sequence"],
                        content_hash=record["content_hash"],
                        used_bytes=task.usage.total_bytes,
                    )
                    if crash_after is not None and current >= crash_after:
                        trace.emit(
                            "test_forced_termination",
                            after_new_commits=current,
                            threshold=crash_after,
                        )
                        os._exit(137)
                else:
                    trace.emit(
                        "block_reused",
                        block_id=task.block_id,
                        sequence=record["sequence"],
                        content_hash=record["content_hash"],
                    )
            return {
                "block_id": task.block_id,
                "sequence": record["sequence"],
                "content_hash": record["content_hash"],
                "reused": was_committed,
            }

        if cancel_event.is_set():
            raise BusinessError(E_CANCELLED, "repair canceled before final block commits")
        results_b = scheduler_b.execute(
            exact_block_tasks, finalize_worker, cancel_event=cancel_event
        )
        _raise_first_task_error(results_b)
        committed_now = store.load_committed()
        if set(committed_now) != {plan.block_id for plan in blocks}:
            missing = sorted(
                {plan.block_id for plan in blocks} - set(committed_now)
            )
            raise BusinessError(
                E_RECOVERY_INVALID,
                "not all blocks have an immutable committed result",
                subcode=SUB_CORRUPT_COMMIT,
                context={"missing_blocks": missing},
            )

        assembled, assembly_report = _assemble_blocks(
            committed=committed_now,
            block_order=[plan.block_id for plan in blocks],
            expected_face_count=final_mesh.face_count,
        )
        trace.emit("blocks_assembled", **assembly_report)

        evaluation = evaluate_repair(
            original,
            assembled.mesh,
            face_provenance=assembled.face_provenance,
            weld_summary=welded.summary(),
            fill_summary=fill_result.summary(),
            split_summary=split_result.summary(),
            fragment_summary=fragment_result.summary(),
            normal_tolerance_deg=config.normal_tolerance_deg,
        )
        trace.emit(
            "evaluation_complete",
            status=evaluation["status"],
            normal_deviation=evaluation["normal_deviation"],
            topology_ok=evaluation["topology_ok"],
        )

        if report_path is None:
            report_path = output_file.with_name(output_file.name + ".report.json")
        report_file = Path(report_path)
        commit_records = [
            {
                "block_id": record["block_id"],
                "sequence": int(record["sequence"]),
                "content_hash": record["content_hash"],
                "payload_sha256": record["payload_sha256"],
                "reused": record["block_id"] in precommitted,
            }
            for record in sorted(
                committed_now.values(), key=lambda value: int(value["sequence"])
            )
        ]
        report: dict[str, Any] = {
            "ok": True,
            "status": evaluation["status"],
            "input": str(input_file),
            "output": str(output_file),
            "source_sha256": source_sha,
            "header": header.to_dict(),
            "config": config.to_dict(),
            "config_hash": config.config_hash,
            "session_id": store.session_id,
            "session_dir": str(store.session_dir),
            "manifest_hash": manifest.manifest_hash,
            "block_table_version": BLOCK_TABLE_VERSION,
            "block_table_hash": store.block_table_hash,
            "blocks": [
                {
                    "block_id": plan.block_id,
                    "submit_seq": plan.index,
                    "cell": list(plan.cell),
                    "initial_face_count": int(plan.initial_face_ids.size),
                    "planned_usage": plan.planned_usage.to_dict(),
                    "final_usage": exact_usages[plan.index].to_dict(),
                }
                for plan in blocks
            ],
            "weld": welded.summary(),
            "boundaries_before_fill": boundary_before_fill.to_dict(),
            "hole_fill": fill_result.summary(),
            "nonmanifold_split": split_result.summary(),
            "fragment_removal": fragment_result.summary(),
            "normals": normals.summary(),
            "cross_block_weld_merges": cross_block_weld_merges,
            "memory": {
                "global_budget_bytes": config.global_budget_bytes,
                "task_budget_bytes": config.task_budget_bytes,
                "max_used_bytes_stage_a": scheduler_a._max_used_bytes,
                "max_active_blocks_stage_a": scheduler_a._max_active,
                "max_used_bytes_stage_b": scheduler_b._max_used_bytes,
                "max_active_blocks_stage_b": scheduler_b._max_active,
            },
            "commits": commit_records,
            "boundary_consistency": assembly_report,
            "artifact_topology": build_topology(assembled.mesh).summary(),
            "evaluation": evaluation,
            "trace_path": str(trace_path),
            "run_id": run_id,
        }
        # Verify report serialization before touching the target mesh.
        json.dumps(report, allow_nan=False, sort_keys=True)
        atomic_write_bytes(
            store.session_dir / "report.json",
            json.dumps(report, indent=2, sort_keys=True, allow_nan=False).encode("utf-8")
            + b"\n",
        )
        output_sha = write_mesh(
            output_file,
            assembled.mesh,
            coord_type="f64",
            index_width=4,
            area_epsilon=config.area_epsilon,
        )
        report["output_sha256"] = output_sha
        report["artifact_vertices"] = assembled.mesh.vertex_count
        report["artifact_faces"] = assembled.mesh.face_count
        report_bytes = (
            json.dumps(report, indent=2, sort_keys=True, allow_nan=False).encode("utf-8")
            + b"\n"
        )
        atomic_write_bytes(report_file, report_bytes)
        atomic_write_bytes(store.session_dir / "report.json", report_bytes)
        snapshot_store.publish(
            assembled.mesh.vertices,
            assembled.mesh.faces,
            face_block_ids=assembled.face_block_ids,
            metadata={
                "source_sha256": source_sha,
                "output_sha256": output_sha,
                "session_id": store.session_id,
                "status": report["status"],
            },
        )
        trace.emit(
            "run_complete",
            output=str(output_file),
            output_sha256=output_sha,
            vertices=assembled.mesh.vertex_count,
            faces=assembled.mesh.face_count,
            status=report["status"],
        )
        return report

    def compare_tolerance(
        self,
        input_path: str | os.PathLike[str],
        *,
        old_tolerance: float,
        new_tolerance: float,
        area_epsilon: float = 1e-12,
    ) -> dict[str, Any]:
        mesh, _ = load_mesh(input_path, area_epsilon=area_epsilon)
        return compare_boundary_tolerances(mesh, old_tolerance, new_tolerance)

    def _session_dir(
        self,
        *,
        source_sha: str,
        config_hash: str,
        block_table: list[dict[str, Any]],
    ) -> Path:
        from .commit import derive_session_id, make_block_table_hash

        table_hash = make_block_table_hash(block_table)
        return (
            self.work_dir
            / "sessions"
            / derive_session_id(source_sha, config_hash, table_hash)
        )

    def _partition_blocks(
        self, mesh: Mesh, shape: tuple[int, int, int]
    ) -> tuple[tuple[np.ndarray, np.ndarray], list[BlockPlan]]:
        centroids = mesh.face_centroids()
        minima = centroids.min(axis=0)
        maxima = centroids.max(axis=0)
        cells = _quantile_cell_ids(centroids, shape)
        grouped: dict[tuple[int, int, int], list[int]] = {}
        for face_id, cell in enumerate(cells):
            grouped.setdefault(tuple(int(value) for value in cell), []).append(face_id)
        keys = sorted(grouped)
        blocks: list[BlockPlan] = []
        boundary_count = len(build_topology(mesh).boundary_edges)
        for index, key in enumerate(keys):
            face_ids = np.asarray(grouped[key], dtype=np.int64)
            local_mesh, _ = _local_mesh(mesh, face_ids)
            local_topology = build_topology(local_mesh)
            local_nonmanifold = len(local_topology.nonmanifold_edges)
            planned = estimate_memory(
                local_mesh.vertex_count + boundary_count + local_nonmanifold,
                local_mesh.face_count + boundary_count,
                local_topology.edge_count + boundary_count * 2,
            )
            blocks.append(
                BlockPlan(
                    index=index,
                    block_id=f"block-{index:04d}",
                    cell=key,
                    initial_face_ids=face_ids,
                    planned_usage=planned,
                )
            )
        return (minima, maxima), blocks

    def _build_block_table(
        self, blocks: list[BlockPlan], mesh: Mesh
    ) -> list[dict[str, Any]]:
        table: list[dict[str, Any]] = []
        for plan in blocks:
            face_bytes = np.asarray(plan.initial_face_ids, dtype="<i8").tobytes()
            table.append(
                {
                    "block_id": plan.block_id,
                    "submit_seq": plan.index,
                    "cell": list(plan.cell),
                    "initial_face_count": int(plan.initial_face_ids.size),
                    "initial_face_sha256": hashlib.sha256(face_bytes).hexdigest(),
                }
            )
        return table


@dataclass
class AssembledMesh:
    mesh: Mesh
    face_provenance: np.ndarray
    face_block_ids: np.ndarray


def _local_mesh(mesh: Mesh, face_ids: np.ndarray) -> tuple[Mesh, np.ndarray]:
    face_ids = np.asarray(face_ids, dtype=np.int64)
    if face_ids.size == 0:
        return Mesh(np.empty((0, 3), np.float64), np.empty((0, 3), np.int64)), np.empty(
            0, dtype=np.int64
        )
    faces_global = mesh.faces[face_ids]
    global_vertex_ids = np.unique(faces_global.reshape(-1))
    global_to_local = np.full(mesh.vertex_count, -1, dtype=np.int64)
    global_to_local[global_vertex_ids] = np.arange(global_vertex_ids.size, dtype=np.int64)
    local_faces = global_to_local[faces_global]
    return Mesh(mesh.vertices[global_vertex_ids], local_faces), global_vertex_ids


def _cell_ids(
    points: np.ndarray,
    minima: np.ndarray,
    maxima: np.ndarray,
    shape: tuple[int, int, int],
) -> np.ndarray:
    extent = maxima - minima
    safe_extent = np.where(extent > 1e-15, extent, 1.0)
    normalized = (points - minima) / safe_extent
    normalized[:, extent <= 1e-15] = 0.0
    dims = np.asarray(shape, dtype=np.float64)
    cells = np.floor(normalized * dims).astype(np.int64)
    for axis, size in enumerate(shape):
        cells[:, axis] = np.clip(cells[:, axis], 0, size - 1)
    return cells


def _quantile_cell_ids(
    points: np.ndarray, shape: tuple[int, int, int]
) -> np.ndarray:
    """Spatial partition by per-axis coordinate quantiles.

    Quantile cuts remain spatial (axis-aligned cells) but are not distorted by
    a few distant scan fragments, unlike a raw bounding-box grid.
    """

    cells = np.zeros((points.shape[0], 3), dtype=np.int64)
    for axis, size in enumerate(shape):
        if size == 1 or points.shape[0] == 0:
            continue
        quantiles = np.arange(1, size, dtype=np.float64) / float(size)
        thresholds = np.quantile(points[:, axis], quantiles, method="linear")
        cells[:, axis] = np.searchsorted(thresholds, points[:, axis], side="right")
        cells[:, axis] = np.clip(cells[:, axis], 0, size - 1)
    return cells


def _raise_first_task_error(results: list[TaskResult]) -> None:
    for result in results:
        if result.error is not None:
            raise result.error


def _crash_after_commits() -> int | None:
    raw = os.environ.get("MESH_REPAIR_CRASH_AFTER_COMMITS")
    if raw is None:
        return None
    try:
        value = int(raw)
    except ValueError as exc:
        raise BusinessError(
            E_CONFIG_INVALID,
            "MESH_REPAIR_CRASH_AFTER_COMMITS must be an integer",
            subcode=SUB_INVALID_PARAMETER,
        ) from exc
    return value if value > 0 else None


def _acceptance_delay(cancel_event: threading.Event) -> None:
    """Test-only delay used to exercise cancellation at block boundaries."""

    raw = os.environ.get("MESH_REPAIR_TEST_BLOCK_DELAY_MS")
    if raw is None:
        return
    try:
        milliseconds = max(0, int(raw))
    except ValueError as exc:
        raise BusinessError(
            E_CONFIG_INVALID,
            "MESH_REPAIR_TEST_BLOCK_DELAY_MS must be an integer",
            subcode=SUB_INVALID_PARAMETER,
        ) from exc
    if milliseconds and cancel_event.wait(milliseconds / 1000.0):
        raise BusinessError(
            E_CANCELLED,
            "block canceled at a cooperative boundary",
        )


def _make_block_payload(
    *,
    final_mesh: Mesh,
    face_ids: np.ndarray,
    face_block_ids: np.ndarray,
    provenance: np.ndarray,
    block_plan: BlockPlan,
    weld_map: dict[str, Any],
    cross_block_weld_merges: list[dict[str, Any]],
    usage: MemoryUsage,
) -> dict[str, Any]:
    face_ids = np.asarray(face_ids, dtype=np.int64)
    if face_ids.size:
        faces = final_mesh.faces[face_ids]
        vertex_ids = np.unique(faces.reshape(-1))
        local_mesh, _ = _local_mesh(final_mesh, face_ids)
        vertex_rows = [
            [int(vertex_id), *[float(value) for value in final_mesh.vertices[vertex_id]]]
            for vertex_id in vertex_ids
        ]
        face_rows = [[int(value) for value in face] for face in faces]
        local_topology = build_topology(local_mesh).summary()
    else:
        vertex_ids = np.empty(0, dtype=np.int64)
        vertex_rows = []
        face_rows = []
        local_topology = build_topology(
            Mesh(np.empty((0, 3), np.float64), np.empty((0, 3), np.int64))
        ).summary()
    return {
        "payload_version": PAYLOAD_VERSION,
        "block_id": block_plan.block_id,
        "block_index": block_plan.index,
        "cell": list(block_plan.cell),
        "global_vertex_ids": [int(value) for value in vertex_ids],
        "vertices": vertex_rows,
        "face_ids": [int(value) for value in face_ids],
        "faces": face_rows,
        "face_provenance": [int(provenance[face_id]) for face_id in face_ids],
        "local_topology": local_topology,
        "memory_usage": usage.to_dict(),
        "weld_map": weld_map,
        "cross_block_weld_merges": [
            item
            for item in cross_block_weld_merges
            if any(
                endpoint["block_index"] == block_plan.index
                for endpoint in item["endpoints"]
            )
        ],
        "all_face_block_ids": [int(face_block_ids[face_id]) for face_id in face_ids],
    }


def _assemble_blocks(
    *,
    committed: dict[str, dict[str, Any]],
    block_order: list[str],
    expected_face_count: int,
) -> tuple[AssembledMesh, dict[str, Any]]:
    vertex_coords: dict[int, np.ndarray] = {}
    shared_vertex_conflicts = 0
    face_rows: dict[int, tuple[int, int, int]] = {}
    face_provenance: dict[int, int] = {}
    face_block: dict[int, int] = {}
    edge_blocks: dict[tuple[int, int], set[int]] = {}
    block_payloads: dict[str, dict[str, Any]] = {}

    for block_id in block_order:
        if block_id not in committed:
            raise BusinessError(
                E_RECOVERY_INVALID,
                "committed block set is incomplete during assembly",
                subcode=SUB_CORRUPT_COMMIT,
                context={"missing_block": block_id},
            )
        payload = committed[block_id]["payload"]
        block_payloads[block_id] = payload
        for row in payload["vertices"]:
            vertex_id = int(row[0])
            point = np.asarray(row[1:4], dtype=np.float64)
            previous = vertex_coords.get(vertex_id)
            if previous is None:
                vertex_coords[vertex_id] = point
            elif not np.array_equal(previous, point):
                shared_vertex_conflicts += 1
        local_face_ids = payload["face_ids"]
        local_faces = payload["faces"]
        local_provenance = payload["face_provenance"]
        local_blocks = payload["all_face_block_ids"]
        for face_id, face, provenance, block_index in zip(
            local_face_ids, local_faces, local_provenance, local_blocks
        ):
            face_id = int(face_id)
            if face_id in face_rows:
                raise BusinessError(
                    E_RECOVERY_INVALID,
                    "multiple committed blocks claim the same face",
                    subcode=SUB_CORRUPT_COMMIT,
                    location={"face": face_id, "block_id": block_id},
                )
            triangle = tuple(int(value) for value in face)
            face_rows[face_id] = triangle
            face_provenance[face_id] = int(provenance)
            face_block[face_id] = int(block_index)
            for u, v in (
                (triangle[0], triangle[1]),
                (triangle[1], triangle[2]),
                (triangle[2], triangle[0]),
            ):
                edge_blocks.setdefault((min(u, v), max(u, v)), set()).add(
                    int(block_index)
                )

    expected_ids = set(range(expected_face_count))
    actual_ids = set(face_rows)
    if actual_ids != expected_ids:
        raise BusinessError(
            E_RECOVERY_INVALID,
            "committed block assembly does not cover the expected face set",
            subcode=SUB_CORRUPT_COMMIT,
            context={
                "missing_faces": sorted(expected_ids - actual_ids)[:20],
                "unexpected_faces": sorted(actual_ids - expected_ids)[:20],
            },
        )
    if shared_vertex_conflicts:
        raise BusinessError(
            E_RECOVERY_INVALID,
            "shared block-boundary vertices have inconsistent coordinates",
            subcode=SUB_CORRUPT_COMMIT,
            context={"conflict_count": shared_vertex_conflicts},
        )

    sorted_face_ids = sorted(face_rows)
    ordered_faces_global = np.asarray(
        [face_rows[face_id] for face_id in sorted_face_ids], dtype=np.int64
    )
    used_global = np.unique(ordered_faces_global.reshape(-1))
    global_to_local = np.full(int(used_global.max()) + 1, -1, dtype=np.int64)
    global_to_local[used_global] = np.arange(used_global.size, dtype=np.int64)
    vertices = np.asarray([vertex_coords[int(vertex_id)] for vertex_id in used_global])
    faces = global_to_local[ordered_faces_global]
    provenance = np.asarray(
        [face_provenance[face_id] for face_id in sorted_face_ids], dtype=np.int64
    )
    block_ids = np.asarray(
        [face_block[face_id] for face_id in sorted_face_ids], dtype=np.int64
    )
    assembled = AssembledMesh(Mesh(vertices, faces), provenance, block_ids)
    topology = build_topology(assembled.mesh)
    summary = topology.summary()
    shared_edges = sum(1 for blocks in edge_blocks.values() if len(blocks) > 1)
    consistency = {
        "shared_vertex_conflicts": shared_vertex_conflicts,
        "shared_edge_count": shared_edges,
        "assembled_from_blocks": len(block_order),
        "output_vertices": assembled.mesh.vertex_count,
        "output_faces": assembled.mesh.face_count,
        "output_boundary_edge_count": int(summary["boundary_edge_count"]),
        "output_nonmanifold_edge_count": int(summary["nonmanifold_edge_count"]),
        "watertight": int(summary["boundary_edge_count"]) == 0,
        "seam_consistent": shared_vertex_conflicts == 0,
        "block_payload_hashes": {
            block_id: committed[block_id]["payload_sha256"] for block_id in block_order
        },
    }
    return assembled, consistency


def repair_file(
    input_path: str | os.PathLike[str],
    output_path: str | os.PathLike[str],
    *,
    config: RepairConfig | None = None,
    work_dir: str | os.PathLike[str] = "repair-work",
    report_path: str | os.PathLike[str] | None = None,
) -> dict[str, Any]:
    return RepairService(work_dir).repair_file(
        input_path,
        output_path,
        config=config,
        report_path=report_path,
    )
