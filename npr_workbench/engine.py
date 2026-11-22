"""Preview/exact orchestration, tiled rendering and recovery."""

from __future__ import annotations

import re
import threading
import time
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .checkpoint import CheckpointStore, LoadedSession, RunIdentity
from .consistency import compare_preview_exact, compare_three_paths
from .errors import (
    TileRenderError,
    VersionConflictError,
)
from .model import (
    Camera,
    ExactRunResult,
    NprFrame,
    RenderParams,
    Scene,
    Snapshot,
)
from .npr import compose_style, outline_from_levels, posterize
from .parameters import ParameterStore
from .raster import shade_scene
from .strokes import (
    StrokePlan,
    build_stroke_plan,
    rasterize_strokes_tile,
    verify_plan_seed_contract,
)
from .tiles import (
    BlockResult,
    FaultInjector,
    ResourceGovernor,
    Tile,
    make_tiles,
    render_tile,
)


class RenderControl:
    def __init__(self, pause_after_commits: int | None = None) -> None:
        self._lock = threading.Lock()
        self._pause = threading.Event()
        self._cancel = threading.Event()
        self.pause_after_commits = pause_after_commits
        self._commits_seen = 0

    def request_pause(self) -> None:
        self._pause.set()

    def request_cancel(self) -> None:
        self._cancel.set()
        self._pause.set()

    def clear(self) -> None:
        with self._lock:
            self._pause.clear()
            self._cancel.clear()
            self._commits_seen = 0

    def note_commit(self) -> None:
        self._commits_seen += 1
        if (
            self.pause_after_commits is not None
            and self._commits_seen >= self.pause_after_commits
        ):
            self._pause.set()

    @property
    def pause_requested(self) -> bool:
        return self._pause.is_set()

    @property
    def cancel_requested(self) -> bool:
        return self._cancel.is_set()


def _session_id(scene: Scene) -> str:
    safe_name = re.sub(r"[^A-Za-z0-9_.-]+", "-", scene.name).strip("-") or "scene"
    return f"{safe_name}-{scene.content_hash()[:12]}"


class NprEngine:
    def __init__(
        self,
        scene: Scene,
        params: RenderParams,
        camera: Camera,
        *,
        persist_dir: str | Path = "persist/checkpoints",
        diagnostics_dir: str | Path = "diagnostics",
        session_id: str | None = None,
        initial_version: int = 1,
    ) -> None:
        self.scene = scene
        self.content_hash = scene.content_hash()
        self.store = ParameterStore(
            self.content_hash,
            camera,
            params,
            initial_version=initial_version,
        )
        self.persist_dir = Path(persist_dir)
        self.diagnostics_dir = Path(diagnostics_dir)
        self.session_id = session_id or _session_id(scene)
        self.checkpoint_root = self.persist_dir / self.session_id
        self.last_preview: NprFrame | None = None
        self.last_exact: NprFrame | None = None
        self.last_consistency_report: object | None = None
        self._plan_cache: dict[str, StrokePlan] = {}
        self._cache_lock = threading.RLock()
        self._render_lock = threading.Lock()
        self.last_divergence: object | None = None

    def _get_plan(self, snapshot: Snapshot) -> StrokePlan:
        key = snapshot.identity_hash()
        with self._cache_lock:
            cached = self._plan_cache.get(key)
            if cached is not None:
                return cached
            plan = build_stroke_plan(
                self.scene,
                snapshot.camera,
                snapshot.params,
                self.content_hash,
            )
            verify_plan_seed_contract(plan, self.content_hash)
            self._plan_cache[key] = plan
            # Keep a small, deterministic cache; identity keys, not wall time.
            if len(self._plan_cache) > 8:
                oldest = next(iter(self._plan_cache))
                if oldest != key:
                    self._plan_cache.pop(oldest, None)
            return plan

    def render_preview(self, snapshot: Snapshot | None = None) -> NprFrame:
        started = time.perf_counter()
        latest = self.store.latest()
        if snapshot is not None and snapshot.version != latest.version:
            raise VersionConflictError(
                latest.version,
                snapshot.version,
                "preview path only accepts the latest parameter version",
            )
        snap = latest
        plan = self._get_plan(snap)
        params = snap.params
        shade = shade_scene(
            self.scene,
            snap.camera,
            params.preview_width,
            params.preview_height,
            quality="preview",
        )
        levels, poster_rgb = posterize(shade.rgb, params.levels)
        outline = outline_from_levels(levels)
        brush_rgba, coverage, _, _ = rasterize_strokes_tile(
            plan,
            params.preview_width,
            params.preview_height,
            0,
            0,
            params.preview_width,
            params.preview_height,
            sample_factor=params.preview_stroke_sample_factor,
            cache_budget=params.stroke_cache_budget,
        )
        styled_rgb = compose_style(poster_rgb, brush_rgba, outline)
        frame = NprFrame(
            kind="preview",
            snapshot=snap,
            base_rgb=(shade.rgb * 255.0 + 0.5).astype(np.uint8),
            poster_rgb=poster_rgb,
            levels=levels,
            outline=outline,
            brush_rgba=brush_rgba,
            styled_rgb=styled_rgb,
            coverage=coverage,
            duration_ms=(time.perf_counter() - started) * 1000.0,
            stroke_count=plan.count,
            stroke_fingerprint_root=plan.fingerprint_root,
        )
        frame.validate_shapes()
        self.last_preview = frame
        return frame

    def _validate_exact_snapshot(
        self,
        snapshot: Snapshot,
        preview: NprFrame | None,
    ) -> tuple[Snapshot, NprFrame | None]:
        # Lock the requested snapshot now.  A later UI edit increments the
        # store version but cannot mutate this immutable snapshot object.
        locked = snapshot
        latest = self.store.latest()
        if (
            locked.version != latest.version
            or locked.identity_hash() != latest.identity_hash()
        ):
            raise VersionConflictError(
                latest.version,
                locked.version,
                "exact render must lock the latest parameter snapshot at start",
            )
        candidate_preview = preview if preview is not None else self.last_preview
        if candidate_preview is not None:
            if candidate_preview.snapshot.version != locked.version:
                raise VersionConflictError(
                    candidate_preview.snapshot.version,
                    locked.version,
                    "exact render snapshot does not match the last preview",
                )
            if candidate_preview.snapshot.identity_hash() != locked.identity_hash():
                raise VersionConflictError(
                    candidate_preview.snapshot.version,
                    locked.version,
                    "camera/parameters differ despite equal version number",
                )
        return locked, candidate_preview

    def render_exact(
        self,
        snapshot: Snapshot | None = None,
        *,
        preview: NprFrame | None = None,
        control: RenderControl | None = None,
        fault_injector: FaultInjector | None = None,
        use_checkpoint: bool = True,
        check_preview: bool = True,
    ) -> ExactRunResult:
        with self._render_lock:
            started = time.perf_counter()
            requested = snapshot if snapshot is not None else self.store.latest()
            candidate_preview = preview if preview is not None else self.last_preview
            locked, candidate_preview = self._validate_exact_snapshot(
                requested, candidate_preview if check_preview else None
            )
            plan = self._get_plan(locked)
            if candidate_preview is not None:
                verify_plan_seed_contract(
                    plan,
                    self.content_hash,
                    expected_root=candidate_preview.stroke_fingerprint_root,
                )
            tiles = make_tiles(
                locked.params.exact_width,
                locked.params.exact_height,
                locked.params.tile_width,
                locked.params.tile_height,
            )
            if not use_checkpoint:
                blocks = self._render_all_tiles(
                    locked,
                    plan,
                    tiles,
                    control=control,
                    fault_injector=fault_injector,
                )
                if control is not None and (control.pause_requested or control.cancel_requested):
                    return ExactRunResult(
                        snapshot=locked,
                        complete=False,
                        paused=control.pause_requested and not control.cancel_requested,
                        canceled=control.cancel_requested,
                        committed_tiles=(),
                        failed_tiles=(),
                        total_tiles=len(tiles),
                        frame=None,
                        checkpoint_dir="",
                    )
                frame = self._assemble_from_block_results(
                    locked,
                    plan,
                    blocks,
                    kind="exact",
                    duration_ms=(time.perf_counter() - started) * 1000.0,
                    commit_order=tuple(block.tile_id for block in blocks),
                )
            else:
                identity = RunIdentity.from_snapshot(locked)
                checkpoint = CheckpointStore(self.checkpoint_root, "session")
                session = checkpoint.load_or_begin(identity, len(tiles))
                existing = set(session.blocks)
                missing = [tile for tile in tiles if tile.tile_id not in existing]
                new_commit_failures = self._render_missing_tiles(
                    checkpoint,
                    session,
                    locked,
                    plan,
                    missing,
                    control=control,
                    fault_injector=fault_injector,
                )
                complete = len(session.blocks) == len(tiles)
                if not complete:
                    return ExactRunResult(
                        snapshot=locked,
                        complete=False,
                        paused=bool(control and control.pause_requested and not control.cancel_requested),
                        canceled=bool(control and control.cancel_requested),
                        committed_tiles=session.committed_tile_ids,
                        failed_tiles=tuple(sorted(new_commit_failures)),
                        total_tiles=len(tiles),
                        frame=None,
                        checkpoint_dir=str(checkpoint.session_dir),
                        commit_order=tuple(session.commit_order),
                        checkpoint_invalidated=session.invalidated,
                        invalid_reason=session.invalid_reason,
                    )
                payloads = checkpoint.read_all_blocks(session)
                frame = self._assemble_from_payloads(
                    locked,
                    plan,
                    payloads,
                    kind="exact",
                    duration_ms=(time.perf_counter() - started) * 1000.0,
                    commit_order=tuple(session.commit_order),
                    failed_tiles=tuple(sorted(new_commit_failures)),
                )
            frame.validate_shapes()
            if candidate_preview is not None:
                diagnostic_dir = (
                    self.diagnostics_dir
                    / f"diverge-v{locked.version}-{locked.identity_hash()[:10]}"
                )
                try:
                    self.last_consistency_report = compare_preview_exact(
                        candidate_preview,
                        frame,
                        diagnostic_dir=diagnostic_dir,
                        raise_on_diverge=True,
                    )
                except Exception as exc:
                    self.last_divergence = exc
                    raise
            self.last_exact = frame
            return ExactRunResult(
                snapshot=locked,
                complete=True,
                paused=False,
                canceled=False,
                committed_tiles=tuple(sorted(frame.tile_commit_log)),
                failed_tiles=frame.failed_tiles,
                total_tiles=len(tiles),
                frame=frame,
                checkpoint_dir=str(self.checkpoint_root)
                if use_checkpoint
                else "",
                commit_order=frame.tile_commit_log,
                checkpoint_invalidated=bool(
                    use_checkpoint and session.invalidated
                )
                if use_checkpoint
                else False,
                invalid_reason=session.invalid_reason if use_checkpoint else None,
            )

    def _render_all_tiles(
        self,
        snapshot: Snapshot,
        plan: StrokePlan,
        tiles: tuple[Tile, ...],
        *,
        control: RenderControl | None,
        fault_injector: FaultInjector | None,
    ) -> list[BlockResult]:
        params = snapshot.params
        governor = ResourceGovernor(params)
        results: list[BlockResult] = []
        if params.max_workers == 1:
            for tile in tiles:
                if control is not None and (control.pause_requested or control.cancel_requested):
                    break
                try:
                    result = render_tile(
                        self.scene,
                        snapshot.camera,
                        params,
                        plan,
                        tile,
                        content_hash=self.content_hash,
                        version=snapshot.version,
                        sample_factor=params.exact_stroke_sample_factor,
                        governor=governor,
                        fault_injector=fault_injector,
                    )
                except Exception:
                    continue
                results.append(result)
            return results

        max_in_flight = max(1, params.max_workers * 2)
        with ThreadPoolExecutor(
            max_workers=params.max_workers, thread_name_prefix="npr-tile"
        ) as pool:
            queue: list[tuple[Tile, Future[BlockResult]]] = []
            next_index = 0

            def submit_next() -> None:
                nonlocal next_index
                if next_index >= len(tiles):
                    return
                tile = tiles[next_index]
                future = pool.submit(
                    render_tile,
                    self.scene,
                    snapshot.camera,
                    params,
                    plan,
                    tile,
                    content_hash=self.content_hash,
                    version=snapshot.version,
                    sample_factor=params.exact_stroke_sample_factor,
                    governor=governor,
                    fault_injector=fault_injector,
                )
                queue.append((tile, future))
                next_index += 1

            for _ in range(min(max_in_flight, len(tiles))):
                submit_next()
            while queue:
                if control is not None and control.cancel_requested:
                    for _, future in queue:
                        future.cancel()
                    break
                tile, future = queue.pop(0)
                try:
                    result = future.result()
                except Exception:
                    result = None
                if result is not None:
                    results.append(result)
                if control is not None and control.cancel_requested:
                    break
                if control is not None and control.pause_requested:
                    break
                submit_next()
        return results

    def _render_missing_tiles(
        self,
        checkpoint: CheckpointStore,
        session: LoadedSession,
        snapshot: Snapshot,
        plan: StrokePlan,
        missing: list[Tile],
        *,
        control: RenderControl | None,
        fault_injector: FaultInjector | None,
    ) -> set[int]:
        params = snapshot.params
        governor = ResourceGovernor(params)
        failed: set[int] = set()

        def commit_if_allowed(result: BlockResult) -> bool:
            if control is not None and control.cancel_requested:
                return False
            checkpoint.commit_block(session, result)
            if control is not None:
                control.note_commit()
            return True

        if params.max_workers == 1:
            for tile in missing:
                if control is not None and (control.pause_requested or control.cancel_requested):
                    break
                try:
                    result = render_tile(
                        self.scene,
                        snapshot.camera,
                        params,
                        plan,
                        tile,
                        content_hash=self.content_hash,
                        version=snapshot.version,
                        sample_factor=params.exact_stroke_sample_factor,
                        governor=governor,
                        fault_injector=fault_injector,
                    )
                except Exception:
                    failed.add(tile.tile_id)
                    continue
                if not commit_if_allowed(result):
                    break
                if control is not None and control.pause_requested:
                    break
            return failed

        max_in_flight = max(1, params.max_workers * 2)
        with ThreadPoolExecutor(
            max_workers=params.max_workers, thread_name_prefix="npr-tile"
        ) as pool:
            queue: list[tuple[Tile, Future[BlockResult]]] = []
            next_index = 0

            def submit_next() -> None:
                nonlocal next_index
                if next_index >= len(missing):
                    return
                tile = missing[next_index]
                future = pool.submit(
                    render_tile,
                    self.scene,
                    snapshot.camera,
                    params,
                    plan,
                    tile,
                    content_hash=self.content_hash,
                    version=snapshot.version,
                    sample_factor=params.exact_stroke_sample_factor,
                    governor=governor,
                    fault_injector=fault_injector,
                )
                queue.append((tile, future))
                next_index += 1

            for _ in range(min(max_in_flight, len(missing))):
                submit_next()
            stop = False
            while queue and not stop:
                if control is not None and control.cancel_requested:
                    for _, future in queue:
                        future.cancel()
                    break
                tile, future = queue.pop(0)
                try:
                    result = future.result()
                except Exception:
                    failed.add(tile.tile_id)
                    result = None
                if result is not None and not commit_if_allowed(result):
                    stop = True
                if control is not None and control.cancel_requested:
                    stop = True
                if control is not None and control.pause_requested:
                    stop = True
                if not stop:
                    submit_next()
        return failed

    def _assemble_from_payloads(
        self,
        snapshot: Snapshot,
        plan: StrokePlan,
        payloads: dict[int, dict[str, np.ndarray]],
        *,
        kind: str,
        duration_ms: float,
        commit_order: tuple[int, ...],
        failed_tiles: tuple[int, ...] = (),
    ) -> NprFrame:
        blocks: list[BlockResult] = []
        for tile_id in sorted(payloads):
            data = payloads[tile_id]
            stroke_ids = tuple(int(value) for value in np.asarray(data["stroke_ids"]).tolist())
            blocks.append(
                BlockResult(
                    tile_id=int(data["tile_id"]),
                    x0=int(data["x0"]),
                    y0=int(data["y0"]),
                    x1=int(data["x0"]) + data["levels"].shape[1],
                    y1=int(data["y0"]) + data["levels"].shape[0],
                    base_rgb=data["base_rgb"],
                    poster_rgb=data["poster_rgb"],
                    levels=data["levels"],
                    outline=data["outline"].astype(bool),
                    brush_rgba=data["brush_rgba"],
                    styled_rgb=data["styled_rgb"],
                    coverage=data["coverage"].astype(bool),
                    stroke_ids=stroke_ids,
                    stroke_point_count=int(np.count_nonzero(data["coverage"])),
                    content_hash=str(data["content_hash"].item()),
                    version=int(data["version"].item()),
                )
            )
        return self._assemble_from_block_results(
            snapshot,
            plan,
            blocks,
            kind=kind,
            duration_ms=duration_ms,
            commit_order=commit_order,
            failed_tiles=failed_tiles,
        )

    def _assemble_from_block_results(
        self,
        snapshot: Snapshot,
        plan: StrokePlan,
        blocks: list[BlockResult],
        *,
        kind: str,
        duration_ms: float,
        commit_order: tuple[int, ...],
        failed_tiles: tuple[int, ...] = (),
    ) -> NprFrame:
        params = snapshot.params
        h, w = params.exact_height, params.exact_width
        base_rgb = np.zeros((h, w, 3), dtype=np.uint8)
        poster_rgb = np.zeros((h, w, 3), dtype=np.uint8)
        levels = np.zeros((h, w), dtype=np.uint8)
        outline = np.zeros((h, w), dtype=bool)
        brush_rgba = np.zeros((h, w, 4), dtype=np.uint8)
        styled_rgb = np.zeros((h, w, 3), dtype=np.uint8)
        coverage = np.zeros((h, w), dtype=bool)
        covered = np.zeros((h, w), dtype=np.uint8)
        for block in blocks:
            if block.content_hash != self.content_hash:
                raise VersionConflictError(
                    snapshot.version,
                    block.version,
                    "assembled block content hash does not match scene",
                )
            if block.version != snapshot.version:
                raise VersionConflictError(
                    snapshot.version,
                    block.version,
                    "assembled block parameter version does not match locked snapshot",
                )
            y0, y1 = block.y0, block.y1
            x0, x1 = block.x0, block.x1
            if base_rgb[y0:y1, x0:x1].shape[:2] != block.base_rgb.shape[:2]:
                raise ValueError(f"tile {block.tile_id} has mismatched dimensions")
            base_rgb[y0:y1, x0:x1] = block.base_rgb
            poster_rgb[y0:y1, x0:x1] = block.poster_rgb
            levels[y0:y1, x0:x1] = block.levels
            outline[y0:y1, x0:x1] = block.outline
            brush_rgba[y0:y1, x0:x1] = block.brush_rgba
            styled_rgb[y0:y1, x0:x1] = block.styled_rgb
            coverage[y0:y1, x0:x1] = block.coverage
            covered[y0:y1, x0:x1] += 1
        if not np.all(covered == 1):
            missing = np.argwhere(covered != 1)
            x, y = int(missing[0][1]), int(missing[0][0])
            raise ValueError(f"incomplete exact frame at pixel ({x}, {y})")
        return NprFrame(
            kind=kind,
            snapshot=snapshot,
            base_rgb=base_rgb,
            poster_rgb=poster_rgb,
            levels=levels,
            outline=outline,
            brush_rgba=brush_rgba,
            styled_rgb=styled_rgb,
            coverage=coverage,
            duration_ms=duration_ms,
            stroke_count=plan.count,
            stroke_fingerprint_root=plan.fingerprint_root,
            tile_commit_log=commit_order,
            failed_tiles=failed_tiles,
        )

    def render_one_shot(
        self,
        snapshot: Snapshot | None = None,
        *,
        fault_injector: FaultInjector | None = None,
    ) -> NprFrame:
        started = time.perf_counter()
        locked = snapshot if snapshot is not None else self.store.latest()
        plan = self._get_plan(locked)
        tiles = make_tiles(
            locked.params.exact_width,
            locked.params.exact_height,
            locked.params.tile_width,
            locked.params.tile_height,
        )
        blocks = self._render_all_tiles(
            locked, plan, tiles, control=None, fault_injector=fault_injector
        )
        if len(blocks) != len(tiles):
            raise TileRenderError(
                next(tile.tile_id for tile in tiles if tile.tile_id not in {b.tile_id for b in blocks}),
                "one-shot render has failed tiles",
            )
        frame = self._assemble_from_block_results(
            locked,
            plan,
            blocks,
            kind="one_shot",
            duration_ms=(time.perf_counter() - started) * 1000.0,
            commit_order=tuple(block.tile_id for block in blocks),
        )
        frame.validate_shapes()
        return frame

    def verify_paths(
        self,
        interactive: NprFrame,
        resumed: NprFrame,
        one_shot: NprFrame,
        *,
        diagnostic_dir: str | Path | None = None,
    ) -> dict[str, object]:
        if diagnostic_dir is None:
            diagnostic_dir = (
                self.diagnostics_dir
                / f"paths-v{interactive.version}-{interactive.snapshot.identity_hash()[:10]}"
            )
        return compare_three_paths(
            interactive,
            resumed,
            one_shot,
            diagnostic_dir=diagnostic_dir,
        )

    def resume(
        self,
        snapshot: Snapshot | None = None,
        *,
        control: RenderControl | None = None,
        fault_injector: FaultInjector | None = None,
    ) -> ExactRunResult:
        return self.render_exact(
            snapshot,
            control=control,
            fault_injector=fault_injector,
            use_checkpoint=True,
            check_preview=False,
        )

