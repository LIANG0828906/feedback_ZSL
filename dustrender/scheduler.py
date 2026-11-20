"""Deterministic chunk scheduling under parallelism and sample budgets."""

from __future__ import annotations

import hashlib
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .chunks import Chunk, assemble_chunks, build_chunk_table, chunking_summary
from .config import RenderConfig
from .errors import (
    E_BUDGET_EXCEEDED,
    E_BUSY,
    E_CANCELLED,
    DustRenderError,
)
from .renderer import (
    ChunkResult,
    SampleEstimate,
    estimate_chunk,
    fixed_point_upper_bound,
    render_chunk,
)
from .scene import Scene
from .seam import compare_seams
from .trace import TraceRecorder
from .util import (
    array_sha256,
    atomic_write_json,
    atomic_write_npy,
    canonical_json_bytes,
    sha256_bytes,
    write_ppm,
)


def content_hash(result: ChunkResult) -> str:
    digest = hashlib.sha256()
    digest.update(np.ascontiguousarray(result.rgb, dtype="<f4").tobytes(order="C"))
    digest.update(np.ascontiguousarray(result.transmittance, dtype="<f4").tobytes(order="C"))
    digest.update(
        f"{result.primary_samples}:{result.shadow_samples}:{result.step:.17g}".encode("ascii")
    )
    return digest.hexdigest()


@dataclass
class RenderRun:
    scene: Scene
    config: RenderConfig
    chunks: list[Chunk]
    results: dict[str, ChunkResult]
    statuses: dict[str, str]
    estimates: dict[str, SampleEstimate]
    trace: TraceRecorder
    image: np.ndarray
    transmittance: np.ndarray
    effective_step: float
    degraded: bool
    error: DustRenderError | None
    seam: dict[str, Any] = field(default_factory=dict)
    precision_compare: dict[str, Any] | None = None
    recovered: set[str] = field(default_factory=set)
    reused: set[str] = field(default_factory=set)
    rejected: set[str] = field(default_factory=set)
    impossible: list[str] = field(default_factory=list)
    incremental: dict[str, Any] | None = None
    initial_waiting: list[str] = field(default_factory=list)

    @property
    def complete(self) -> bool:
        return self.error is None and all(
            status in {"completed", "recovered", "reused"} for status in self.statuses.values()
        )

    @property
    def chunk_hashes(self) -> list[str]:
        return [
            content_hash(self.results[chunk.id])
            for chunk in self.chunks
            if chunk.id in self.results
        ]

    def summary(self) -> dict[str, Any]:
        primary = sum(
            result.primary_samples
            for chunk_id, result in self.results.items()
            if self.statuses.get(chunk_id) in {"completed", "recovered", "reused"}
        )
        shadow = sum(
            result.shadow_samples
            for chunk_id, result in self.results.items()
            if self.statuses.get(chunk_id) in {"completed", "recovered", "reused"}
        )
        usage = []
        for chunk in self.chunks:
            estimate = self.estimates.get(chunk.id, SampleEstimate(chunk.id, 0, 0, 0))
            result = self.results.get(chunk.id)
            usage.append(
                {
                    "chunk": chunk.id,
                    "budget": self.config.task_budget,
                    "estimated": estimate.primary,
                    "samples": result.samples if result else 0,
                    "status": self.statuses.get(chunk.id, "missing"),
                }
            )
        samples = {
            "primary": primary,
            "shadow": shadow,
            "total": primary + shadow,
        }
        estimated_total = sum(item.primary for item in self.estimates.values())
        peak_task = max((result.samples for result in self.results.values()), default=0)
        summary: dict[str, Any] = {
            "summary_version": 1,
            "scene_id": self.scene.scene_id,
            "scene_sha256": self.scene.scene_sha256,
            "payload_sha256": self.scene.payload_sha256,
            "density_version": self.scene.density_version,
            "screen": {"width": self.scene.width, "height": self.scene.height},
            "precision": self.config.precision,
            "config": self.config.to_dict(),
            "config_fingerprint": self.config.fingerprint,
            "chunking": chunking_summary(
                self.scene.width, self.scene.height, self.config, self.chunks
            ),
            "chunk_hashes": self.chunk_hashes,
            "samples": samples,
            "budget": {
                "policy": self.config.budget_policy,
                "per_task": self.config.task_budget,
                "estimated_total": estimated_total,
                "samples_total": samples["total"],
                "peak_task": peak_task,
                "awaiting": max(len(self.rejected), len(self.initial_waiting)),
                "rejected": len(self.rejected),
                "usage": usage,
            },
            "step": {
                "base": self.config.base_step,
                "effective": self.effective_step,
                "scale": self.effective_step / self.config.base_step,
                "degraded": self.degraded,
            },
            "tasks": {
                "order": [chunk.id for chunk in self.chunks],
                "completed": sum(
                    status in {"completed", "recovered", "reused"}
                    for status in self.statuses.values()
                ),
                "failed": 0,
                "rejected": len(self.rejected),
                "awaiting": 0,
                "reused": len(self.reused),
            },
            "seam": self.seam,
            "image_sha256": array_sha256(self.image),
            "transmittance_sha256": array_sha256(self.transmittance),
            "error": self.error.to_dict() if self.error else None,
        }
        if self.precision_compare is not None:
            summary["precision_compare"] = self.precision_compare
            summary["precision_compare_hash"] = sha256_bytes(
                canonical_json_bytes(self.precision_compare)
            )
        if self.incremental is not None:
            summary["incremental"] = self.incremental
        summary["summary_sha256"] = sha256_bytes(canonical_json_bytes(summary))
        return summary

    def result_document(self) -> dict[str, Any]:
        chunks = []
        for chunk in self.chunks:
            result = self.results.get(chunk.id)
            status = self.statuses.get(chunk.id, "missing")
            chunks.append(
                {
                    "id": chunk.id,
                    "index": chunk.index,
                    "bounds": list(chunk.bounds),
                    "status": status,
                    "content_hash": content_hash(result) if result else None,
                    "primary_samples": result.primary_samples if result else 0,
                    "samples": result.samples if result else 0,
                    "step": result.step if result else self.effective_step,
                }
            )
        summary = self.summary()
        return {
            "scene_id": self.scene.scene_id,
            "scene_sha256": self.scene.scene_sha256,
            "config_fingerprint": self.config.fingerprint,
            "complete": self.complete,
            "chunks": chunks,
            "image_sha256": summary["image_sha256"],
            "transmittance_sha256": summary["transmittance_sha256"],
            "summary": summary,
            "error": self.error.to_dict() if self.error else None,
        }

    def write(self, outdir: str | Path) -> None:
        target = Path(outdir)
        target.mkdir(parents=True, exist_ok=True)
        atomic_write_npy(target / "image.npy", self.image)
        atomic_write_npy(target / "transmittance.npy", self.transmittance)
        atomic_write_json(target / "summary.json", self.summary())
        atomic_write_json(target / "result.json", self.result_document())
        self.trace.write(target / "trace.jsonl")
        write_ppm(target / "image.ppm", self.image)


class RenderScheduler:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._active = False
        self._cancel = threading.Event()

    def request_cancel(self) -> None:
        self._cancel.set()

    def render(
        self,
        scene: Scene,
        config: RenderConfig,
        *,
        preloaded: dict[str, ChunkResult] | None = None,
        preloaded_status: str = "recovered",
        reuse_existing: bool = True,
        compare_precision: bool | None = None,
        on_progress: Callable[[dict[str, ChunkResult], dict[str, str], float], None] | None = None,
    ) -> RenderRun:
        with self._lock:
            if self._active:
                raise DustRenderError(E_BUSY, "a whole-frame render is already active", "$render")
            self._active = True
            self._cancel.clear()
        try:
            return self._render(
                scene,
                config,
                preloaded=preloaded or {},
                preloaded_status=preloaded_status,
                reuse_existing=reuse_existing,
                compare_precision=compare_precision,
                on_progress=on_progress,
            )
        finally:
            with self._lock:
                self._active = False

    def _render(
        self,
        scene: Scene,
        config: RenderConfig,
        *,
        preloaded: dict[str, ChunkResult],
        preloaded_status: str,
        reuse_existing: bool,
        compare_precision: bool | None,
        on_progress: Callable[[dict[str, ChunkResult], dict[str, str], float], None] | None,
    ) -> RenderRun:
        chunks = build_chunk_table(
            scene.width, scene.height, config.chunk_width, config.chunk_height
        )
        trace = TraceRecorder()
        trace.emit(
            "render_start",
            width=scene.width,
            height=scene.height,
            chunk_count=len(chunks),
            chunk_size=[config.chunk_width, config.chunk_height],
            parallelism=config.parallelism,
            task_budget=config.task_budget,
            budget_policy=config.budget_policy,
            phase=config.phase,
            precision=config.precision,
            scene_id=scene.scene_id,
            scene_sha256=scene.scene_sha256,
        )

        results: dict[str, ChunkResult] = {}
        statuses: dict[str, str] = {}
        reused: set[str] = set()
        for chunk in chunks:
            if reuse_existing and chunk.id in preloaded:
                result = preloaded[chunk.id]
                if result.chunk.bounds != chunk.bounds:
                    continue
                results[chunk.id] = result
                statuses[chunk.id] = preloaded_status
                reused.add(chunk.id)
        if on_progress is not None and results:
            on_progress(results, statuses, config.base_step)

        pending = [chunk for chunk in chunks if chunk.id not in results]
        estimates: dict[str, SampleEstimate] = {}
        for chunk in chunks:
            if chunk.id in preloaded and chunk.id not in pending:
                result = preloaded[chunk.id]
                estimates[chunk.id] = SampleEstimate(
                    chunk.id,
                    result.primary_samples,
                    result.shadow_samples,
                    result.max_segments,
                )
            else:
                estimates[chunk.id] = estimate_chunk(scene, config, chunk)
        initial_estimates = dict(estimates)
        initial_waiting = [
            chunk.id
            for chunk in pending
            if initial_estimates[chunk.id].total > config.task_budget
        ]

        effective_step = config.base_step
        degraded = False
        over = [chunk for chunk in pending if estimates[chunk.id].total > config.task_budget]
        impossible: list[str] = []
        if over and config.budget_policy == "degrade":
            current = config.base_step
            for _ in range(40):
                over = [
                    chunk
                    for chunk in pending
                    if estimates[chunk.id].total > config.task_budget
                ]
                if not over:
                    break
                worst = max(estimates[chunk.id].total for chunk in over)
                factor = max(1.02, float(worst) / float(config.task_budget))
                candidate = min(config.max_step, current * factor)
                if candidate <= current + 1e-12:
                    impossible = [chunk.id for chunk in over]
                    break
                degraded = True
                current = candidate
                for chunk in pending:
                    estimates[chunk.id] = estimate_chunk(scene, config, chunk, current)
            effective_step = current
        elif over and config.budget_policy == "reject":
            impossible = []

        rejected: set[str] = set()
        if config.budget_policy == "reject":
            rejected = {
                chunk.id
                for chunk in pending
                if estimates[chunk.id].total > config.task_budget
            }
        elif over and impossible:
            rejected = set(impossible)

        trace.emit(
            "budget_plan",
            step=effective_step,
            step_scale=effective_step / config.base_step,
            degraded=degraded,
            estimated_total=sum(item.total for item in estimates.values()),
            waiting=initial_waiting,
            rejected=sorted(rejected, key=lambda value: int(value[1:])),
            impossible=impossible or None,
        )
        for chunk in chunks:
            estimate = estimates[chunk.id]
            trace.emit(
                "task_planned",
                chunk=chunk.id,
                estimate_samples=estimate.total,
                initial_estimate_samples=initial_estimates[chunk.id].total,
                max_segments=estimate.max_segments,
                over_budget=initial_estimates[chunk.id].total > config.task_budget,
            )

        for chunk in reused:
            result = results[chunk]
            trace.emit(
                "chunk_reused",
                chunk=chunk,
                content_hash=content_hash(result),
                samples=result.samples,
                reason="preloaded",
            )

        actions = "reject" if config.budget_policy == "reject" else "degrade"
        for chunk in pending:
            if initial_estimates[chunk.id].total <= config.task_budget:
                continue
            trace.emit(
                "budget_wait",
                chunk=chunk.id,
                estimate_samples=initial_estimates[chunk.id].total,
                budget=config.task_budget,
                action=actions,
            )
        if degraded:
            trace.emit(
                "budget_degraded",
                old_step=config.base_step,
                new_step=effective_step,
                scale=effective_step / config.base_step,
            )

        runnable = [chunk for chunk in pending if chunk.id not in rejected]

        for chunk in runnable:
            if self._cancel.is_set():
                break
            result = render_chunk(
                scene,
                config,
                chunk,
                effective_step,
                cancel_check=self._cancel.is_set,
            )
            if result.samples > config.task_budget:
                if config.budget_policy == "reject":
                    rejected.add(chunk.id)
                    continue
                rejected.add(chunk.id)
                continue
            results[chunk.id] = result
            statuses[chunk.id] = "completed"
            if on_progress is not None:
                on_progress(results, statuses, effective_step)

        for chunk in chunks:
            if chunk.id not in statuses:
                statuses[chunk.id] = "missing"

        _emit_task_events(
            trace,
            chunks,
            results,
            reused,
            rejected,
            config.parallelism,
            config.task_budget,
        )

        image, transmittance = assemble_chunks(
            scene.width, scene.height, chunks, results, fill=0.0
        )

        seam = {"status": "not_run"}
        error: DustRenderError | None = None
        if self._cancel.is_set():
            error = DustRenderError(E_CANCELLED, "render cancelled cooperatively", "$render")
        elif rejected:
            field_path = (
                "$config.task_budget"
                if config.budget_policy == "reject"
                else "$config.max_step"
            )
            message = (
                f"{len(rejected)} chunk task(s) exceed the per-task budget"
                if config.budget_policy == "reject"
                else "per-task budget cannot be met within max_step"
            )
            error = DustRenderError(
                E_BUDGET_EXCEEDED,
                message,
                field_path,
                {
                    "budget": config.task_budget,
                    "completed_chunks": [
                        chunk.id for chunk in chunks if statuses.get(chunk.id) == "completed"
                    ],
                    "rejected_chunks": sorted(rejected, key=lambda value: int(value[1:])),
                },
            )
            trace.emit(
                "budget_rejected",
                chunks=sorted(rejected, key=lambda value: int(value[1:])),
                budget=config.task_budget,
            )

        precision_doc: dict[str, Any] | None = None
        if error is None and config.seam_validation:
            seam, seam_error = _validate_seams(
                scene, config, effective_step, chunks, results, trace
            )
            if seam_error is not None:
                error = seam_error
        compare_enabled = (
            config.compare_precision if compare_precision is None else compare_precision
        )
        if error is None and compare_enabled:
            precision_doc = _compare_precision(scene, config, effective_step, trace)

        trace.emit(
            "render_end",
            status="ok" if error is None else error.code,
        )
        run = RenderRun(
            scene=scene,
            config=config,
            chunks=chunks,
            results=results,
            statuses=statuses,
            estimates=estimates,
            trace=trace,
            image=image,
            transmittance=transmittance,
            effective_step=effective_step,
            degraded=degraded,
            error=error,
            seam=seam,
            precision_compare=precision_doc,
            recovered={key for key, value in statuses.items() if value == "recovered"},
            reused=reused,
            rejected=rejected,
            impossible=impossible,
            initial_waiting=initial_waiting,
        )
        return run


def _emit_task_events(
    trace: TraceRecorder,
    chunks: list[Chunk],
    results: dict[str, ChunkResult],
    reused: set[str],
    rejected: set[str],
    parallelism: int,
    budget: int,
) -> None:
    import heapq

    pending = [
        chunk
        for chunk in chunks
        if chunk.id in results and chunk.id not in reused
    ]
    if not pending:
        return
    # Simulate only the already rendered results. Cost is a pure function of sample
    # counts, so the resulting event order is deterministic and independent of the OS.
    queue_index = 0
    next_index = 0
    events: list[tuple[int, int, int, int, Chunk | None, int]] = []
    for worker in range(min(parallelism, len(pending))):
        chunk = pending[next_index]
        next_index += 1
        heapq.heappush(events, (0, 0, queue_index, worker, chunk, 0))
        queue_index += 1
    while events:
        time, kind, serial, worker, chunk, _ = heapq.heappop(events)
        if chunk is None:
            continue
        if kind == 0:
            trace.emit("task_start", chunk=chunk.id, worker=worker)
            cost = max(0, results[chunk.id].samples)
            heapq.heappush(
                events,
                (time + cost, 1, queue_index, worker, chunk, 0),
            )
            queue_index += 1
        else:
            result = results[chunk.id]
            trace.emit(
                "task_complete",
                chunk=chunk.id,
                samples=result.samples,
                budget=budget,
                content_hash=content_hash(result),
            )
            if next_index < len(pending):
                following = pending[next_index]
                next_index += 1
                heapq.heappush(
                    events,
                    (time, 0, queue_index, worker, following, 0),
                )
                queue_index += 1


def _validate_seams(
    scene: Scene,
    config: RenderConfig,
    effective_step: float,
    chunks: list[Chunk],
    results: dict[str, ChunkResult],
    trace: TraceRecorder,
) -> tuple[dict[str, Any], DustRenderError | None]:
    truth_chunk = Chunk(0, "truth", 0, 0, 0, 0, scene.width, scene.height)
    truth = render_chunk(scene, config, truth_chunk, effective_step)
    trace.emit(
        "reference_complete",
        reason="single_chunk_truth",
        image_sha256=array_sha256(truth.rgb),
        transmittance_sha256=array_sha256(truth.transmittance),
    )
    doc, error = compare_seams(
        scene,
        chunks,
        results,
        truth,
        config.seam_tolerance,
    )
    trace.emit(
        "seam_check",
        status=doc["status"],
        tolerance=config.seam_tolerance,
        violations=doc["violation_count"],
        pairs=doc["chunk_pairs"],
    )
    if error is not None:
        trace.emit("seam_error", code=error.code, details=error.details)
    return doc, error


def _compare_precision(
    scene: Scene,
    config: RenderConfig,
    effective_step: float,
    trace: TraceRecorder,
) -> dict[str, Any]:
    alternate = "fixed" if config.precision == "float" else "float"
    alt_config = RenderConfig.from_dict(
        {**config.to_dict(), "precision": alternate, "compare_precision": False}
    )
    truth_chunk = Chunk(0, "truth", 0, 0, 0, 0, scene.width, scene.height)
    reference = render_chunk(scene, config, truth_chunk, effective_step)
    alternate_result = render_chunk(scene, alt_config, truth_chunk, effective_step)
    rgb_diff = np.abs(
        alternate_result.rgb.astype(np.float64) - reference.rgb.astype(np.float64)
    )
    trans_diff = np.abs(
        alternate_result.transmittance.astype(np.float64)
        - reference.transmittance.astype(np.float64)
    )
    flat = rgb_diff.reshape(-1)
    upper_bound = fixed_point_upper_bound(config, reference.primary_samples)
    doc = {
        "reference_precision": config.precision,
        "alternate_precision": alternate,
        "max_abs": float(np.max(flat)) if flat.size else 0.0,
        "mean_abs": float(np.mean(flat)) if flat.size else 0.0,
        "rms": float(np.sqrt(np.mean(flat * flat))) if flat.size else 0.0,
        "p95_abs": float(np.percentile(flat, 95)) if flat.size else 0.0,
        "scatter_max_abs": float(np.max(rgb_diff)) if rgb_diff.size else 0.0,
        "transmittance_max_abs": float(np.max(trans_diff)) if trans_diff.size else 0.0,
        "upper_bound": upper_bound,
        "pixels_over_upper_bound": int(np.sum(np.max(rgb_diff, axis=2) > upper_bound)),
        "error_map_sha256": array_sha256(rgb_diff.astype(np.float32)),
        "samples": reference.samples,
    }
    trace.emit(
        "precision_compare",
        alternate_precision=alternate,
        max_abs=doc["max_abs"],
        mean_abs=doc["mean_abs"],
        upper_bound=upper_bound,
    )
    return doc
