from __future__ import annotations

import threading
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .checkpoint import CheckpointLoad, CheckpointStore
from .errors import WorkbenchError
from .flatten import FlatContour, flatten_contour
from .models import Contour, Glyph
from .params import RenderParams
from .raster import (
    RasterComponent,
    RasterResult,
    RasterSpec,
    make_raster_spec,
    rasterize_flat_contour,
    result_from_components,
)
from .state import SegmentState, StateMachine


class CrashInjected(RuntimeError):
    """Test/CLI signal raised after a durable segment commit."""


@dataclass
class TaskOutcome:
    index: int
    flat: FlatContour | None = None
    component: RasterComponent | None = None
    error: WorkbenchError | None = None


@dataclass
class EngineResult:
    raster: RasterResult
    states: dict[str, Any]
    status: str
    failures: list[dict[str, Any]] = field(default_factory=list)
    loaded_from_checkpoint: int = 0
    checkpoint_invalidated: bool = False
    checkpoint_reason: str | None = None
    paused: bool = False
    deviation: dict[str, Any] | None = None
    total_segments: int = 0
    flat_contours: list[FlatContour | None] = field(default_factory=list)

    @property
    def complete(self) -> bool:
        return self.status == "complete"

    @property
    def commit_sequence(self) -> list[int]:
        return list(self.raster.commit_sequence)

    def summary(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "glyph_id": self.raster.glyph_id,
            "width": self.raster.width,
            "height": self.raster.height,
            "area": self.raster.area,
            "committed_count": self.states["committed_count"],
            "total_segments": self.states["total"],
            "commit_sequence": self.commit_sequence,
            "loaded_from_checkpoint": self.loaded_from_checkpoint,
            "checkpoint_invalidated": self.checkpoint_invalidated,
            "checkpoint_reason": self.checkpoint_reason,
            "paused": self.paused,
            "failures": list(self.failures),
            "deviation": self.deviation,
        }


def _run_contour_task(
    index: int,
    contour: Contour,
    params: RenderParams,
    spec: RasterSpec,
) -> TaskOutcome:
    try:
        flat = flatten_contour(contour, params, validate_params=False)
        component = rasterize_flat_contour(flat, spec, params)
        return TaskOutcome(index=index, flat=flat, component=component)
    except WorkbenchError as exc:
        return TaskOutcome(index=index, error=exc)
    except Exception as exc:  # noqa: BLE001 - isolate a single segment failure.
        return TaskOutcome(
            index=index,
            error=WorkbenchError(
                "E_STATE",
                f"unexpected segment failure: {type(exc).__name__}: {exc}",
                {"contour_index": contour.index, "segment_index": index},
            ),
        )


def _default_result(glyph: Glyph, params: RenderParams) -> RasterResult:
    spec = make_raster_spec(glyph, params)
    return result_from_components(
        glyph_id=glyph.glyph_id,
        params=params,
        spec=spec,
        components=[],
        commit_sequence=[],
    )


def process_glyph(
    glyph: Glyph,
    params: RenderParams,
    *,
    persist_root: str | Path | None = None,
    force_full: bool = False,
    pause_event: threading.Event | None = None,
    on_commit: Callable[[int, EngineResult], None] | None = None,
    on_progress: Callable[[EngineResult], None] | None = None,
    reusable: dict[int, tuple[FlatContour, RasterComponent]] | None = None,
    preserve_other_snapshots: bool = False,
) -> EngineResult:
    """Process a glyph from a validated checkpoint, or recompute from scratch.

    Contour flattening and rasterization run in a bounded thread pool. Results
    are consumed and checkpointed in contour order, so thread scheduling cannot
    alter the commit sequence.
    """

    params.validate()
    if not glyph.contours:
        raise WorkbenchError(
            "E_SCHEMA", "glyph has no contours", glyph_id=glyph.glyph_id
        )
    total = len(glyph.contours)
    machine = StateMachine(total)
    store: CheckpointStore | None = None
    checkpoint_load = CheckpointLoad(
        valid=False,
        components=[],
        flats=[],
        committed_count=0,
        commit_sequence=[],
        persisted_states=None,
    )
    if persist_root is not None:
        store = CheckpointStore(persist_root, glyph, params)
        if force_full:
            store.reset()
        else:
            checkpoint_load = store.validate_or_invalidate()
    components: list[RasterComponent] = []
    flats: dict[int, FlatContour] = {}
    loaded_count = 0
    if checkpoint_load.valid and checkpoint_load.committed_count > 0:
        loaded_count = checkpoint_load.committed_count
        machine.restore_committed(loaded_count)
        for position, (component, flat) in enumerate(
            zip(checkpoint_load.components, checkpoint_load.flats)
        ):
            components.append(component)
            flats[position] = flat
        sequence = list(range(loaded_count))
    else:
        sequence = []

    raster_result = result_from_components(
        glyph_id=glyph.glyph_id,
        params=params,
        spec=make_raster_spec(glyph, params),
        components=components,
        commit_sequence=sequence,
    )
    failures: list[dict[str, Any]] = []
    if pause_event is not None and pause_event.is_set():
        return _finish_result(
            glyph,
            params,
            machine,
            raster_result,
            loaded_count,
            checkpoint_load,
            failures,
            paused=True,
            flats=flats,
        )

    pending = [
        (index, contour)
        for index, contour in enumerate(glyph.contours)
        if index >= loaded_count
    ]
    spec = raster_result.spec
    for _, contour in pending:
        spec.check_buffer(params, contour.index)
    outcomes: dict[int, TaskOutcome] = {}
    computed_pending: list[tuple[int, Contour]] = []
    expected_bit_bytes = (spec.fine_width * spec.fine_height + 7) // 8
    for index, contour in pending:
        candidate = (reusable or {}).get(index)
        if (
            candidate is not None
            and candidate[0].tolerance == params.tolerance
            and candidate[0].polyline_point_count > 2
            and candidate[1].packed_bits.size == expected_bit_bytes
        ):
            outcomes[index] = TaskOutcome(
                index=index, flat=candidate[0], component=candidate[1]
            )
        else:
            computed_pending.append((index, contour))
    if computed_pending:
        worker_count = min(params.max_workers, len(computed_pending))
        with ThreadPoolExecutor(
            max_workers=worker_count, thread_name_prefix="glyph-segment"
        ) as executor:
            futures = {
                executor.submit(
                    _run_contour_task, index, contour, params, spec
                ): index
                for index, contour in computed_pending
            }
            for future in as_completed(futures):
                outcome = future.result()
                outcomes[outcome.index] = outcome

    if store is not None:
        store.persist_states(machine.to_dict())
    for index, _ in pending:
        outcome = outcomes[index]
        if outcome.error is not None or outcome.flat is None or outcome.component is None:
            error = outcome.error or WorkbenchError(
                "E_STATE", "segment produced no output", segment_index=index
            )
            machine.fail(index, error.code, error.message)
            failures.append(
                {
                    "segment_index": index,
                    "contour_index": glyph.contours[index].index,
                    "code": error.code,
                    "message": error.message,
                    "context": error.context,
                }
            )
            if store is not None:
                store.persist_states(machine.to_dict())
            continue
        machine.transition(index, SegmentState.FLATTENED)
        machine.set_metadata(index, flat_count=outcome.flat.polyline_point_count)
        if store is not None:
            store.persist_states(machine.to_dict())
        machine.transition(index, SegmentState.FILLED)
        if store is not None:
            store.persist_states(machine.to_dict())

    paused = False
    for index, _ in pending:
        if index != machine.committed_count:
            break
        record = machine.records()[index]
        if record.state is not SegmentState.FILLED:
            break
        outcome = outcomes[index]
        assert outcome.component is not None
        assert outcome.flat is not None
        if store is not None:
            store.commit_segment(
                index,
                outcome.flat,
                outcome.component,
                spec,
                machine.to_dict(),
            )
        machine.transition(index, SegmentState.COMMITTED)
        machine.set_metadata(
            index,
            flat_count=outcome.component.flat_count,
            cache_hash=outcome.component.content_hash,
        )
        components.append(outcome.component)
        flats[index] = outcome.flat
        raster_result = result_from_components(
            glyph_id=glyph.glyph_id,
            params=params,
            spec=spec,
            components=components,
            commit_sequence=list(range(len(components))),
        )
        if on_progress is not None:
            on_progress(
                _finish_result(
                    glyph,
                    params,
                    machine,
                    raster_result,
                    loaded_count,
                    checkpoint_load,
                    failures,
                    paused=False,
                    flats=flats,
                )
            )
        if on_commit is not None:
            provisional = _finish_result(
                glyph,
                params,
                machine,
                raster_result,
                loaded_count,
                checkpoint_load,
                failures,
                paused=False,
                flats=flats,
            )
            on_commit(index, provisional)
        if pause_event is not None and pause_event.is_set():
            paused = True
            break
    if store is not None:
        store.persist_states(machine.to_dict())
    result = _finish_result(
        glyph,
        params,
        machine,
        raster_result,
        loaded_count,
        checkpoint_load,
        failures,
        paused=paused,
        flats=flats,
    )
    if (
        store is not None
        and result.complete
        and not preserve_other_snapshots
    ):
        store.prune_other_snapshots()
    return result


def _finish_result(
    glyph: Glyph,
    params: RenderParams,
    machine: StateMachine,
    raster_result: RasterResult,
    loaded_count: int,
    checkpoint_load: CheckpointLoad,
    failures: list[dict[str, Any]],
    *,
    paused: bool,
    flats: dict[int, FlatContour],
) -> EngineResult:
    states = machine.to_dict()
    complete = (
        states["committed_count"] == states["total"]
        and not failures
        and not paused
    )
    if complete:
        status = "complete"
    elif paused:
        status = "paused"
    else:
        status = "failed"
    deviation = None
    if len(flats) == len(glyph.contours):
        ordered = [flats[index] for index in range(len(glyph.contours))]
        from .flatten import deviation_statistics

        deviation = deviation_statistics(
            glyph, ordered, scale=params.pixels_per_unit
        ).to_dict()
    return EngineResult(
        raster=raster_result,
        states=states,
        status=status,
        failures=failures,
        loaded_from_checkpoint=loaded_count,
        checkpoint_invalidated=checkpoint_load.invalidated,
        checkpoint_reason=checkpoint_load.reason,
        paused=paused,
        deviation=deviation,
        total_segments=len(glyph.contours),
        flat_contours=[flats.get(index) for index in range(len(glyph.contours))],
    )


def process_full(
    glyph: Glyph,
    params: RenderParams,
    **kwargs: Any,
) -> EngineResult:
    return process_glyph(glyph, params, force_full=True, **kwargs)


def process_incremental(
    glyph: Glyph,
    params: RenderParams,
    persist_root: str | Path,
    **kwargs: Any,
) -> EngineResult:
    return process_glyph(glyph, params, persist_root=persist_root, **kwargs)

