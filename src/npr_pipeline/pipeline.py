from __future__ import annotations

import math
import os
from dataclasses import dataclass, field
from typing import Any, Callable

from .deps import InvalidationPlan, batch_dependencies, compute_invalidation
from .errors import (
    E_CANCELLED,
    E_COMMIT_FAILED,
    E_RECOVERY_INVALID,
    PipelineError,
    fail,
)
from .params import RenderParams
from .scene import Scene
from .storage import Store, encode_batch
from .strokes import (
    LAYER_NAMES,
    BatchData,
    LayerData,
    Stroke,
    generate_outline,
    generate_shadow,
    generate_smear,
)


@dataclass
class BatchTrace:
    layer: int
    batch: str
    chunk: str
    zone_id: str
    affected: bool
    reused: bool
    resumed: bool
    removed: bool
    stroke_count: int
    content_hash: str
    reasons: list[str]


@dataclass
class _BatchAction:
    layer: int
    batch: BatchData
    record: dict[str, Any]
    reused: bool
    resumed: bool
    reasons: list[str]


@dataclass
class RenderResult:
    manifest: dict[str, Any]
    committed: bool
    batch_traces: list[BatchTrace] = field(default_factory=list)
    affected_batches: int = 0
    reused_batches: int = 0
    rewritten_batches: int = 0
    resumed_batches: int = 0
    removed_batches: int = 0
    layer_stats: dict[str, Any] = field(default_factory=dict)

    @property
    def version(self) -> int:
        return int(self.manifest["version"])

    @property
    def version_hash(self) -> str:
        return str(self.manifest["content_hash"])


def _stroke_from_document(raw: dict[str, Any]) -> Stroke:
    points = tuple((float(item[0]), float(item[1])) for item in raw["points"])
    color_raw = raw["color"]
    color = (
        int(color_raw[0]),
        int(color_raw[1]),
        int(color_raw[2]),
        int(color_raw[3]),
    )
    return Stroke(
        points=points,
        width=float(raw["width"]),
        color=color,
        opacity=float(raw["opacity"]),
        kind=str(raw["kind"]),
        zone_id=str(raw["zone_id"]),
        path_id=None if raw.get("path_id") is None else str(raw["path_id"]),
    )


def _load_zone_strokes(
    store: Store,
    manifest: dict[str, Any] | None,
    layer: int,
) -> dict[str, tuple[Stroke, ...]]:
    result: dict[str, list[Stroke]] = {}
    if manifest is None:
        return {}
    for record in manifest.get("batches", []):
        if int(record.get("layer", -1)) != layer:
            continue
        document = store.load_chunk_document(record)
        zone_id = str(record["zone_id"])
        result.setdefault(zone_id, []).extend(
            _stroke_from_document(raw) for raw in document.get("strokes", [])
        )
    return {zone_id: tuple(values) for zone_id, values in result.items()}


def _merge_layer(
    scene: Scene,
    generated: LayerData | None,
    old_strokes: dict[str, tuple[Stroke, ...]],
) -> dict[str, tuple[Stroke, ...]]:
    result: dict[str, tuple[Stroke, ...]] = {}
    generated_by_zone = generated.strokes_by_zone if generated is not None else {}
    for zone in scene.ordered_zones:
        if generated is not None and zone.id in generated_by_zone:
            result[zone.id] = generated_by_zone[zone.id]
        else:
            result[zone.id] = old_strokes.get(zone.id, ())
    return result


def _record_for_batch(
    store: Store,
    batch: BatchData,
    deps: dict[str, str],
) -> dict[str, Any]:
    data, _document, digest = encode_batch(batch, deps)
    del data
    return {
        "layer": batch.layer,
        "batch": batch.batch_id,
        "chunk": batch.chunk_id,
        "zone_id": batch.zone_id,
        "file": store.batch_relative(batch.layer, batch.batch_id, digest),
        "hash": digest,
        "factor_versions": dict(sorted(deps.items())),
        "stroke_count": len(batch.strokes),
        "bbox": list(batch.bbox),
    }


def _verify_staged_record(store: Store, record: dict[str, Any]) -> bool:
    try:
        document = store.load_chunk_document(record)
    except PipelineError:
        return False
    return (
        document.get("batch") == record.get("batch")
        and document.get("layer") == record.get("layer")
        and document.get("factor_versions") == record.get("factor_versions")
        and len(document.get("strokes", [])) == record.get("stroke_count")
    )


class RenderPipeline:
    def __init__(self, store: Store):
        self.store = store

    def _resume_state(
        self,
        previous: dict[str, Any] | None,
        scene: Scene,
        params: RenderParams,
        factors: dict[str, str],
    ) -> dict[str, Any] | None:
        state = self.store.read_staging()
        if state is None:
            return None
        expected_version = 1 if previous is None else int(previous["version"]) + 1
        base_version = None if previous is None else previous["version"]
        base_hash = None if previous is None else previous["manifest_hash"]
        if (
            state.get("target_version") != expected_version
            or state.get("base_version") != base_version
            or state.get("base_manifest_hash") != base_hash
            or state.get("scene_id") != scene.scene_id
            or state.get("params_hash") != params.params_hash
            or state.get("factors") != dict(sorted(factors.items()))
        ):
            self.store.clear_staging()
            self.store.cleanup_uncommitted()
            return None
        completed = state.get("completed")
        if not isinstance(completed, list):
            self.store.clear_staging()
            self.store.cleanup_uncommitted()
            return None
        valid: list[dict[str, Any]] = []
        invalid_found = False
        for record in completed:
            if not isinstance(record, dict):
                invalid_found = True
                continue
            if not isinstance(record.get("batch"), str):
                invalid_found = True
                continue
            if _verify_staged_record(self.store, record):
                valid.append(record)
            else:
                invalid_found = True
        state = dict(state)
        state["completed"] = valid
        if invalid_found:
            self.store.write_staging(state)
            self.store.cleanup_uncommitted(
                {str(record["file"]) for record in valid if isinstance(record.get("file"), str)}
            )
        return state

    def _prepare_staging(
        self,
        previous: dict[str, Any] | None,
        scene: Scene,
        params: RenderParams,
        factors: dict[str, str],
        expected_batches: list[str],
        resume_state: dict[str, Any] | None,
    ) -> dict[str, Any]:
        if resume_state is not None:
            state = dict(resume_state)
            state["expected_batches"] = sorted(expected_batches)
            self.store.write_staging(state)
            return state
        state = {
            "target_version": 1 if previous is None else int(previous["version"]) + 1,
            "base_version": None if previous is None else previous["version"],
            "base_manifest_hash": None if previous is None else previous["manifest_hash"],
            "scene_id": scene.scene_id,
            "params_hash": params.params_hash,
            "factors": dict(sorted(factors.items())),
            "expected_batches": sorted(expected_batches),
            "completed": [],
        }
        self.store.write_staging(state)
        return state

    def render(
        self,
        scene: Scene,
        params: RenderParams,
        *,
        resume: bool = True,
        cancel_event: Any | None = None,
        fail_after_batches: int | None = None,
        crash_after_batches: int | None = None,
        before_batch: Callable[[int, str], None] | None = None,
    ) -> RenderResult:
        with self.store.acquire_work_lock():
            previous = self.store.current_manifest()
            factors = params.factor_versions(scene)
            plan = compute_invalidation(previous, factors, scene.draw_order)
            resume_state = (
                self._resume_state(previous, scene, params, factors) if resume else None
            )
            if resume_state is None and not resume:
                self.store.clear_staging()

            staging = self._prepare_staging(
                previous,
                scene,
                params,
                factors,
                [],
                resume_state,
            )

            old_by_batch = {
                str(record["batch"]): record
                for record in ([] if previous is None else previous.get("batches", []))
            }
            old_by_layer_zone: dict[tuple[int, str], list[dict[str, Any]]] = {}
            for record in old_by_batch.values():
                key = (int(record["layer"]), str(record["zone_id"]))
                old_by_layer_zone.setdefault(key, []).append(record)
            staged_by_batch = {
                str(record["batch"]): record
                for record in staging.get("completed", [])
                if isinstance(record, dict)
            }

            generated_layers: dict[int, LayerData] = {}
            if plan.layers_to_generate[0]:
                generated_layers[0] = generate_outline(
                    scene, params, zones=plan.layers_to_generate[0]
                )
            old_outline = _load_zone_strokes(self.store, previous, 0)
            current_outline = _merge_layer(scene, generated_layers.get(0), old_outline)
            if plan.layers_to_generate[1]:
                generated_layers[1] = generate_shadow(
                    scene,
                    params,
                    current_outline,
                    zones=plan.layers_to_generate[1],
                )
            old_shadow = _load_zone_strokes(self.store, previous, 1)
            current_shadow = _merge_layer(scene, generated_layers.get(1), old_shadow)
            if plan.layers_to_generate[2]:
                generated_layers[2] = generate_smear(
                    scene,
                    params,
                    current_shadow,
                    zones=plan.layers_to_generate[2],
                )

            records: list[dict[str, Any]] = []
            traces: list[BatchTrace] = []
            layer_zone_stats: dict[int, dict[str, dict[str, int]]] = {
                layer: {} for layer in LAYER_NAMES
            }
            old_stats = previous.get("layer_stats", {}) if previous is not None else {}
            for layer in LAYER_NAMES:
                old_layer_stats = old_stats.get(str(layer), {})
                old_zone_stats = old_layer_stats.get("zones", {})
                for zone_id in scene.draw_order:
                    if zone_id in plan.layers_to_generate[layer]:
                        generated = generated_layers.get(layer)
                        if generated is not None:
                            layer_zone_stats[layer][zone_id] = {
                                "stroke_count": len(
                                    generated.strokes_by_zone.get(zone_id, ())
                                ),
                                "excluded_strokes": int(
                                    generated.excluded_by_zone.get(zone_id, 0)
                                ),
                            }
                    elif isinstance(old_zone_stats.get(zone_id), dict):
                        layer_zone_stats[layer][zone_id] = {
                            "stroke_count": int(
                                old_zone_stats[zone_id].get("stroke_count", 0)
                            ),
                            "excluded_strokes": int(
                                old_zone_stats[zone_id].get("excluded_strokes", 0)
                            ),
                        }
                    else:
                        layer_zone_stats[layer][zone_id] = {
                            "stroke_count": 0,
                            "excluded_strokes": 0,
                        }

            try:
                actions: list[_BatchAction] = []
                new_batch_ids: set[str] = set()
                for layer in LAYER_NAMES:
                    generated = generated_layers.get(layer)
                    generated_zones = plan.layers_to_generate[layer]
                    if generated is not None:
                        for batch in generated.batches:
                            if batch.zone_id not in generated_zones:
                                continue
                            new_batch_ids.add(batch.batch_id)
                            deps = batch_dependencies(layer, batch.zone_id, factors)
                            candidate = _record_for_batch(self.store, batch, deps)
                            old_record = old_by_batch.get(batch.batch_id)
                            staged_record = staged_by_batch.get(batch.batch_id)
                            reused = False
                            resumed = False
                            reasons = list(plan.reason_for(layer, batch.zone_id))
                            record: dict[str, Any]
                            if (
                                staged_record is not None
                                and staged_record.get("hash") == candidate["hash"]
                                and staged_record.get("factor_versions")
                                == candidate["factor_versions"]
                            ):
                                record = staged_record
                                reused = True
                                resumed = True
                                reasons.append("resumed_from_staging")
                            elif (
                                old_record is not None
                                and old_record.get("hash") == candidate["hash"]
                            ):
                                record = dict(old_record)
                                record["factor_versions"] = candidate["factor_versions"]
                                record["bbox"] = candidate["bbox"]
                                record["stroke_count"] = candidate["stroke_count"]
                                reused = True
                                reasons.append("identical_content")
                            else:
                                record = candidate
                            actions.append(
                                _BatchAction(
                                    layer=layer,
                                    batch=batch,
                                    record=record,
                                    reused=reused,
                                    resumed=resumed,
                                    reasons=sorted(set(reasons)),
                                )
                            )
                write_ids = {
                    str(action.record["batch"])
                    for action in actions
                    if not action.reused
                }
                completed_ids = {
                    str(record["batch"])
                    for record in staging.get("completed", [])
                    if isinstance(record, dict) and isinstance(record.get("batch"), str)
                }
                staging["expected_batches"] = sorted(write_ids | completed_ids)
                self.store.write_staging(staging)

                write_sequence = 0
                for action in actions:
                    if action.reused:
                        continue
                    self._check_cancel(cancel_event)
                    if before_batch is not None:
                        before_batch(write_sequence, action.batch.batch_id)
                    self._check_cancel(cancel_event)
                    record = self.store.write_batch(
                        action.batch,
                        action.record["factor_versions"],
                    )
                    if record["hash"] != action.record["hash"]:
                        fail(
                            E_COMMIT_FAILED,
                            f"batch:{action.batch.batch_id}",
                            "deterministic_hash_mismatch",
                        )
                    action.record = record
                    write_sequence += 1
                    staging["completed"].append(record)
                    staging["completed"].sort(key=lambda item: item["batch"])
                    self.store.write_staging(staging)
                    if (
                        fail_after_batches is not None
                        and write_sequence >= fail_after_batches
                    ):
                        fail(
                            E_COMMIT_FAILED,
                            f"batch:{action.batch.batch_id}",
                            "injected_chunk_write_failure",
                        )
                    if (
                        crash_after_batches is not None
                        and write_sequence >= crash_after_batches
                    ):
                        os._exit(91)

                for action in actions:
                    records.append(action.record)
                    traces.append(
                        BatchTrace(
                            layer=action.layer,
                            batch=action.batch.batch_id,
                            chunk=action.batch.chunk_id,
                            zone_id=action.batch.zone_id,
                            affected=True,
                            reused=action.reused,
                            resumed=action.resumed,
                            removed=False,
                            stroke_count=int(action.record["stroke_count"]),
                            content_hash=str(action.record["hash"]),
                            reasons=action.reasons,
                        )
                    )
                for layer in LAYER_NAMES:
                    generated_zones = plan.layers_to_generate[layer]
                    for zone_id in scene.draw_order:
                        if zone_id in generated_zones:
                            continue
                        for record in old_by_layer_zone.get((layer, zone_id), []):
                            new_batch_ids.add(str(record["batch"]))
                            records.append(dict(record))
                            traces.append(
                                BatchTrace(
                                    layer=layer,
                                    batch=str(record["batch"]),
                                    chunk=str(record["chunk"]),
                                    zone_id=zone_id,
                                    affected=False,
                                    reused=True,
                                    resumed=False,
                                    removed=False,
                                    stroke_count=int(record["stroke_count"]),
                                    content_hash=str(record["hash"]),
                                    reasons=["unchanged_dependency_set"],
                                )
                            )
                removed = sorted(set(old_by_batch) - new_batch_ids)
                for batch_id in removed:
                    record = old_by_batch[batch_id]
                    traces.append(
                        BatchTrace(
                            layer=int(record["layer"]),
                            batch=batch_id,
                            chunk=str(record["chunk"]),
                            zone_id=str(record["zone_id"]),
                            affected=True,
                            reused=False,
                            resumed=False,
                            removed=True,
                            stroke_count=0,
                            content_hash="",
                            reasons=["batch_no_longer_generated"],
                        )
                    )

                layer_stats = self._build_layer_stats(
                    records, layer_zone_stats, scene
                )
                manifest = self.store.build_manifest(
                    scene, factors, records, layer_stats, previous
                )
                if (
                    previous is not None
                    and manifest["content_hash"] == previous["content_hash"]
                    and manifest["factors"] == previous["factors"]
                    and manifest["scene_id"] == previous["scene_id"]
                ):
                    self.store.clear_staging()
                    result = RenderResult(
                        manifest=previous,
                        committed=False,
                        batch_traces=traces,
                        affected_batches=sum(1 for item in traces if item.affected),
                        reused_batches=sum(1 for item in traces if item.reused),
                        rewritten_batches=0,
                        resumed_batches=sum(1 for item in traces if item.resumed),
                        removed_batches=len(removed),
                        layer_stats=previous["layer_stats"],
                    )
                    return result
                self.store.commit_manifest(manifest)
                self.store.clear_staging()
                result = RenderResult(
                    manifest=manifest,
                    committed=True,
                    batch_traces=traces,
                    affected_batches=sum(1 for item in traces if item.affected),
                    reused_batches=sum(1 for item in traces if item.reused),
                    rewritten_batches=sum(
                        1 for item in traces if not item.reused and not item.removed
                    ),
                    resumed_batches=sum(1 for item in traces if item.resumed),
                    removed_batches=len(removed),
                    layer_stats=layer_stats,
                )
                result.batch_traces.sort(
                    key=lambda item: (item.layer, item.batch, item.removed)
                )
                return result
            except PipelineError as exc:
                self.store.clear_staging()
                self.store.cleanup_uncommitted()
                if exc.code == E_RECOVERY_INVALID:
                    raise
                raise
            except BaseException:
                self.store.clear_staging()
                self.store.cleanup_uncommitted()
                raise

    def _check_cancel(self, cancel_event: Any | None) -> None:
        if cancel_event is not None and cancel_event.is_set():
            fail(E_CANCELLED, "render", "cancelled_at_batch_boundary")

    def _build_layer_stats(
        self,
        records: list[dict[str, Any]],
        layer_zone_stats: dict[int, dict[str, dict[str, int]]],
        scene: Scene,
    ) -> dict[str, Any]:
        result: dict[str, Any] = {}
        for layer in LAYER_NAMES:
            layer_records = [record for record in records if int(record["layer"]) == layer]
            zones: dict[str, Any] = {}
            for zone_id in scene.draw_order:
                values = layer_zone_stats[layer].get(
                    zone_id, {"stroke_count": 0, "excluded_strokes": 0}
                )
                zones[zone_id] = {
                    "stroke_count": int(values["stroke_count"]),
                    "excluded_strokes": int(values["excluded_strokes"]),
                    "batch_count": sum(
                        1 for record in layer_records if record["zone_id"] == zone_id
                    ),
                }
            result[str(layer)] = {
                "layer_name": LAYER_NAMES[layer],
                "stroke_count": sum(item["stroke_count"] for item in zones.values()),
                "excluded_strokes": sum(
                    item["excluded_strokes"] for item in zones.values()
                ),
                "batch_count": len(layer_records),
                "chunk_count": len({str(item["chunk"]) for item in layer_records}),
                "zones": dict(sorted(zones.items())),
            }
        return result


__all__ = ["BatchTrace", "RenderPipeline", "RenderResult"]
