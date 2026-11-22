from __future__ import annotations

import os
import time
from collections.abc import Iterable
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any

from .bake import ARTIFACT_ORDER, CancellationToken, bake_tile
from .cache import SnapshotStore
from .dependencies import CacheKeys, DependencyGraph
from .errors import E_BAKE, E_CANCEL, PipelineError
from .loader import load_project, save_edits
from .model import (
    ABSOLUTE_MAX_WORKERS,
    SNAPSHOT_FORMAT,
    BakedTile,
    BakeParams,
    BakeResult,
    InvalidationReport,
)
from .util import read_json


class Pipeline:
    """Deterministic terrain bake coordinator.

    A successful run publishes a complete snapshot. Failed or cancelled runs
    leave ``pending.json`` plus already committed immutable blocks, which a new
    process can recover only when its global project hash and bake version
    match the pending target.
    """

    def __init__(
        self,
        project_dir: str | Path,
        cache_dir: str | Path,
        *,
        params: BakeParams | None = None,
        edits_file: str | Path | None = None,
    ) -> None:
        self.project_dir = Path(project_dir).resolve()
        self.cache_dir = Path(cache_dir).resolve()
        self.edits_file = Path(edits_file).resolve() if edits_file is not None else None
        self.project = load_project(self.project_dir, self.edits_file)
        self.params = params or BakeParams(subdivisions=self.project.default_subdivisions)
        self.params.validate()
        self.graph = DependencyGraph(self.project)
        self.keys = CacheKeys(self.project, self.graph, self.params.bake_version)
        self.store = SnapshotStore(self.cache_dir)
        opened = self.store.open(
            self.project.global_content_hash,
            self.params.bake_version,
            self.keys.expected_map(),
        )
        self.snapshot = opened.snapshot
        self.pending = opened.pending
        self.startup_reason = opened.startup_reason
        self._startup_recovered = opened.recovered
        self.last_invalidation: InvalidationReport | None = None
        self.last_result: BakeResult | None = None
        self._dirty_changed: set[str] = set()
        self._dirty_triggers: list[dict[str, Any]] = []

    @property
    def project_hash(self) -> str:
        return self.project.global_content_hash

    def apply_height_edit(self, tile_id: str, x: int, y: int, value: float) -> InvalidationReport:
        changed = self.project.apply_height_edit(tile_id, x, y, value)
        self._save_edits_if_configured()
        self._refresh_keys()
        trigger = {
            "kind": "height_edit",
            "tile_id": tile_id,
            "x": int(x),
            "y": int(y),
            "value": float(value),
            "changed_tiles": list(changed),
        }
        self._dirty_changed.update(changed)
        self._dirty_triggers.append(trigger)
        self.last_invalidation = self.graph.invalidate(
            sorted(self._dirty_changed),
            trigger=list(self._dirty_triggers),
            mode="incremental",
        )
        return self.last_invalidation

    def apply_material_edit(
        self,
        tile_id: str,
        layer: str,
        value: float,
    ) -> InvalidationReport:
        self.project.apply_material_edit(tile_id, layer, value)
        self._save_edits_if_configured()
        self._refresh_keys()
        trigger = {
            "kind": "material_edit",
            "tile_id": tile_id,
            "layer": layer,
            "value": float(value),
            "changed_tiles": [tile_id],
        }
        self._dirty_changed.add(tile_id)
        self._dirty_triggers.append(trigger)
        self.last_invalidation = self.graph.invalidate(
            sorted(self._dirty_changed),
            trigger=list(self._dirty_triggers),
            mode="incremental",
        )
        return self.last_invalidation

    def save_edits(self, path: str | Path | None = None) -> None:
        target = Path(path).resolve() if path is not None else self.edits_file
        if target is None:
            raise PipelineError(
                "E_STATE",
                "no edits file configured",
            )
        save_edits(self.project, target)

    def query_invalidation(
        self,
        changed_tiles: Iterable[str],
        *,
        trigger: list[dict[str, Any]] | None = None,
    ) -> InvalidationReport:
        """Return the dependency closure without modifying project content."""

        changed = sorted(set(changed_tiles))
        for tile_id in changed:
            if tile_id not in self.project.tiles:
                raise PipelineError(
                    "E_STATE",
                    "unknown tile id",
                    tile_id=tile_id,
                    field="tile_id",
                )
        return self.graph.invalidate(
            changed,
            trigger=trigger or [{"kind": "query", "changed_tiles": changed}],
            mode="query",
        )

    def run(
        self,
        *,
        mode: str = "auto",
        fault_tiles: Iterable[str] | None = None,
        cancel_token: CancellationToken | None = None,
        verify_truth: bool | None = None,
        write_diagnostics: bool = True,
    ) -> BakeResult:
        if mode not in {"auto", "incremental", "full"}:
            raise PipelineError(
                "E_STATE",
                f"unknown run mode {mode!r}",
                field="mode",
            )
        self.params.validate()
        self._refresh_keys()
        fault_set = set(fault_tiles or ())
        verify = (not self.project.has_edits) if verify_truth is None else bool(verify_truth)
        started = time.perf_counter()

        force_full = mode == "full"
        if force_full:
            self.store.discard_all()
            self.snapshot = None
            self.pending = None
            self._startup_recovered = {}
            self.last_invalidation = None

        report = self._select_invalidation(force_full=force_full)
        expected_keys = self.keys.expected_map()
        snapshot_entries = self._snapshot_entries(self.snapshot)

        recovered: dict[str, dict[str, Any]] = {}
        if self._startup_recovered:
            recovered.update(self._startup_recovered)
        if self.pending is not None:
            recovered.update(self.store.recovered_from_pending(self.pending, expected_keys))

        affected_set = set(report.affected)
        entries: dict[str, dict[str, Any]] = {}
        reused_ids: list[str] = []
        recovered_ids: list[str] = []
        to_bake: list[str] = []
        needs_full_fallback = False
        for tile_id in self.graph.topological_order:
            key = expected_keys[tile_id]
            recovered_entry = recovered.get(tile_id)
            if recovered_entry is not None and recovered_entry.get("cache_key") == key:
                entries[tile_id] = recovered_entry
                recovered_ids.append(tile_id)
                continue
            snapshot_entry = snapshot_entries.get(tile_id)
            if (
                tile_id not in affected_set
                and snapshot_entry is not None
                and snapshot_entry.get("cache_key") == key
            ):
                entries[tile_id] = snapshot_entry
                reused_ids.append(tile_id)
                continue
            to_bake.append(tile_id)

        if any(tile_id not in affected_set for tile_id in to_bake):
            needs_full_fallback = True

        if needs_full_fallback:
            report = self._full_report(
                "incomplete_incremental_base",
                mode="full",
            )
            affected_set = set(report.affected)
            entries = {}
            reused_ids = []
            recovered_ids = []
            to_bake = []
            for tile_id in self.graph.topological_order:
                key = expected_keys[tile_id]
                recovered_entry = recovered.get(tile_id)
                if recovered_entry is not None and recovered_entry.get("cache_key") == key:
                    entries[tile_id] = recovered_entry
                    recovered_ids.append(tile_id)
                else:
                    to_bake.append(tile_id)

        if to_bake:
            pending = self.store.begin_pending(
                project_hash=self.project.global_content_hash,
                bake_version=self.params.bake_version,
                expected_keys=expected_keys,
                changed=list(report.changed),
                affected=list(report.affected),
                trigger=list(report.trigger),
                mode=report.mode,
                startup_reason=report.startup_reason,
            )
            self.pending = pending
            recovered = self.store.recovered_from_pending(pending, expected_keys)
            if recovered:
                entries = {}
                reused_ids = []
                recovered_ids = []
                to_bake = []
                for tile_id in self.graph.topological_order:
                    key = expected_keys[tile_id]
                    recovered_entry = recovered.get(tile_id)
                    if recovered_entry is not None and recovered_entry.get("cache_key") == key:
                        entries[tile_id] = recovered_entry
                        recovered_ids.append(tile_id)
                        continue
                    snapshot_entry = snapshot_entries.get(tile_id)
                    if (
                        tile_id not in affected_set
                        and snapshot_entry is not None
                        and snapshot_entry.get("cache_key") == key
                    ):
                        entries[tile_id] = snapshot_entry
                        reused_ids.append(tile_id)
                        continue
                    to_bake.append(tile_id)

        rebuilt_ids: list[str] = []
        failed: dict[str, dict[str, Any]] = {}
        timings_ms: dict[str, float] = {}
        cancelled = False
        effective_workers = self._effective_workers(len(to_bake))

        if to_bake:
            waves = self.graph.wave_order(to_bake)
            with ThreadPoolExecutor(
                max_workers=effective_workers, thread_name_prefix="terrain-bake"
            ) as executor:
                for wave in waves:
                    if cancel_token is not None and cancel_token.cancelled:
                        cancelled = True
                        break
                    futures = {
                        tile_id: executor.submit(
                            self._bake_one,
                            tile_id,
                            expected_keys[tile_id],
                            fault_set,
                            cancel_token,
                            verify,
                            report.mode,
                        )
                        for tile_id in wave
                    }
                    for tile_id in sorted(futures):
                        try:
                            baked, elapsed_ms = futures[tile_id].result()
                        except PipelineError as exc:
                            failed[tile_id] = exc.to_dict()
                            if exc.code == E_CANCEL:
                                cancelled = True
                            continue
                        except Exception as exc:  # defensive worker boundary
                            failed[tile_id] = {
                                "code": E_BAKE,
                                "message": "unhandled tile bake exception",
                                "tile_id": tile_id,
                                "field": None,
                                "details": {"exception_type": type(exc).__name__},
                            }
                            continue
                        timings_ms[tile_id] = elapsed_ms
                        try:
                            entry = self.store.commit_block(baked)
                        except PipelineError as exc:
                            failed[tile_id] = exc.to_dict()
                            continue
                        entries[tile_id] = entry.to_dict()
                        rebuilt_ids.append(tile_id)
                    if cancelled:
                        break

        all_tiles_present = all(tile_id in entries for tile_id in self.graph.topological_order)
        success = not failed and not cancelled and all_tiles_present
        total_ms = (time.perf_counter() - started) * 1000.0

        if success:
            if (
                self.snapshot is not None
                and self.snapshot.get("project_hash") == self.project.global_content_hash
                and self.snapshot.get("bake_version") == self.params.bake_version
                and not rebuilt_ids
                and not recovered_ids
            ):
                # Pure cache hit: preserve the existing committed snapshot.
                pass
            else:
                snapshot = self._build_snapshot(entries, expected_keys)
                self.store.write_snapshot(snapshot)
                self.snapshot = snapshot
                self.pending = None
            self._startup_recovered = {}
        else:
            if not cancelled and not failed:
                failed["__run__"] = {
                    "code": E_BAKE,
                    "message": "one or more tiles were not produced",
                    "tile_id": None,
                    "field": None,
                    "details": {"missing_tiles": sorted(set(self.graph.tile_ids) - set(entries))},
                }
            if cancelled:
                failed.setdefault(
                    "__run__",
                    {
                        "code": E_CANCEL,
                        "message": "run cancelled; completed blocks remain in the pending ledger",
                        "tile_id": None,
                        "field": None,
                        "details": {},
                    },
                )

        error: dict[str, Any] | None = None
        if not success:
            first_key = min(failed)
            first = failed[first_key]
            error = {
                "code": first.get("code", E_BAKE),
                "message": first.get("message", "bake failed"),
                "tile_id": first.get("tile_id"),
                "field": first.get("field"),
                "failed_tiles": sorted(key for key in failed if not key.startswith("__")),
                "details": first.get("details", {}),
            }

        result = BakeResult(
            success=success,
            snapshot=self.snapshot if success else None,
            invalidations=report,
            rebuilt=sorted(rebuilt_ids, key=self._topo_index),
            reused=sorted(reused_ids, key=self._topo_index),
            recovered=sorted(recovered_ids, key=self._topo_index),
            failed=dict(sorted(failed.items())),
            timings_ms=dict(sorted(timings_ms.items())),
            total_ms=total_ms,
            effective_workers=effective_workers,
            startup_reason=report.startup_reason,
            error=error,
        )
        self.last_result = result
        self.last_invalidation = report if not success else None
        if success:
            self._dirty_changed.clear()
            self._dirty_triggers.clear()
        if write_diagnostics:
            self.store.write_diagnostics(result.diagnostics_dict())
            self.store.write_diagnostics(report.to_dict(), name="last_invalidation.json")
        return result

    def _bake_one(
        self,
        tile_id: str,
        cache_key: str,
        fault_tiles: set[str],
        cancel_token: CancellationToken | None,
        verify_truth: bool,
        operation_tag: str,
    ) -> tuple[BakedTile, float]:
        tile = self.project.tiles[tile_id]
        neighbors = self.project.neighbors(tile)
        neighbor_heights = {
            direction: neighbor.height
            for direction, neighbor in neighbors.items()
            if direction in {"east", "south"}
        }
        started = time.perf_counter()
        baked = bake_tile(
            tile,
            self.params,
            cache_key,
            self.keys.dependency_maps[tile_id],
            neighbor_heights,
            verify_truth=verify_truth,
            cancel_token=cancel_token,
            operation_tag=operation_tag,
            fault_tiles=fault_tiles,
        )
        elapsed_ms = (time.perf_counter() - started) * 1000.0
        return baked, elapsed_ms

    def _select_invalidation(self, *, force_full: bool) -> InvalidationReport:
        if force_full:
            return self._full_report("explicit_full_rebuild", mode="full")
        snapshot_matches = (
            self.snapshot is not None
            and self.snapshot.get("project_hash") == self.project.global_content_hash
            and self.snapshot.get("bake_version") == self.params.bake_version
        )
        if snapshot_matches:
            return self.graph.invalidate(
                [],
                trigger=[{"kind": "cache_hit", "reason": "snapshot_matches"}],
                mode="incremental",
            )
        if self.last_invalidation is not None:
            return self.last_invalidation
        if (
            self.pending is not None
            and self.pending.get("project_hash") == self.project.global_content_hash
        ):
            changed = self.pending.get("changed", [])
            trigger = self.pending.get("trigger", [])
            if not isinstance(changed, list):
                changed = []
            if not isinstance(trigger, list):
                trigger = [{"kind": "pending_recovery"}]
            report = self.graph.invalidate(
                changed,
                trigger=trigger,
                mode=str(self.pending.get("mode", "incremental")),
                startup_reason=self.startup_reason,
                force_all=not changed,
            )
            return report
        reason = self.startup_reason or (
            "empty_cache" if self.snapshot is None else "snapshot_hash_mismatch"
        )
        return self._full_report(reason, mode="full")

    def _full_report(self, reason: str, *, mode: str) -> InvalidationReport:
        return self.graph.invalidate(
            [],
            trigger=[{"kind": "full_rebuild", "reason": reason}],
            mode=mode,
            startup_reason=reason
            if reason not in {"empty_cache", "explicit_full_rebuild"}
            else None,
            force_all=True,
        )

    def _snapshot_entries(self, snapshot: dict[str, Any] | None) -> dict[str, dict[str, Any]]:
        if not isinstance(snapshot, dict):
            return {}
        result: dict[str, dict[str, Any]] = {}
        for entry in snapshot.get("tiles", []):
            if isinstance(entry, dict) and isinstance(entry.get("tile_id"), str):
                result[entry["tile_id"]] = entry
        return result

    def _build_snapshot(
        self,
        entries: dict[str, dict[str, Any]],
        expected_keys: dict[str, str],
    ) -> dict[str, Any]:
        ordered_entries: list[dict[str, Any]] = []
        for tile_id in self.graph.topological_order:
            entry = entries[tile_id]
            if entry.get("cache_key") != expected_keys[tile_id]:
                raise PipelineError(
                    "E_CACHE",
                    "cannot publish snapshot with a mismatched block key",
                    tile_id=tile_id,
                )
            ordered_entries.append(entry)
        return {
            "format": SNAPSHOT_FORMAT,
            "project_hash": self.project.global_content_hash,
            "global_content_hash": self.project.global_content_hash,
            "bake_version": self.params.bake_version,
            "bake_params": self.params.output_dict(),
            "grid_tiles": list(self.project.grid_tiles),
            "tile_count": len(ordered_entries),
            "artifact_order": list(ARTIFACT_ORDER),
            "tiles": ordered_entries,
        }

    def _refresh_keys(self) -> None:
        self.keys = CacheKeys(self.project, self.graph, self.params.bake_version)

    def _save_edits_if_configured(self) -> None:
        if self.edits_file is not None:
            save_edits(self.project, self.edits_file)

    def _topo_index(self, tile_id: str) -> int:
        return self.graph.topological_order.index(tile_id)

    def _effective_workers(self, task_count: int) -> int:
        if task_count <= 0:
            return 0
        cpu = os.cpu_count() or 1
        return max(
            1,
            min(
                int(self.params.max_workers),
                int(cpu),
                ABSOLUTE_MAX_WORKERS,
                int(task_count),
            ),
        )


def inspect_cache(cache_dir: str | Path) -> dict[str, Any]:
    """Read a completed snapshot without starting a bake."""

    path = Path(cache_dir).resolve() / "snapshot.json"
    if not path.is_file():
        raise PipelineError("E_CACHE", "snapshot is missing", details={"path": path.as_posix()})
    try:
        payload = read_json(path)
    except Exception as exc:
        raise PipelineError(
            "E_CACHE",
            "snapshot is unreadable",
            details={"path": path.as_posix(), "reason": type(exc).__name__},
        ) from exc
    if not isinstance(payload, dict) or payload.get("format") != SNAPSHOT_FORMAT:
        raise PipelineError("E_CACHE", "snapshot format is invalid")
    return payload
