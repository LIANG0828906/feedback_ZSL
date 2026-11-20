"""Scripted local acceptance flow for incremental publication and query pinning."""

from __future__ import annotations

import dataclasses
from pathlib import Path
from typing import Any

from .bake import BakeEngine
from .canonical import TraceLog, write_json_atomic
from .errors import ErrorCode, ProbeError
from .publish import PublishedStore
from .scene import BoxOccluder, Scene, Sky
from .snapshot import SnapshotStore
from .strategy import BakeConfig


def _move_occluder(scene: Scene, occluder_id: str) -> Scene:
    updated = []
    found = False
    for occluder in scene.occluders:
        if occluder.occluder_id == occluder_id and isinstance(occluder, BoxOccluder):
            shift = (0.35, 0.0, -0.25)
            updated.append(
                BoxOccluder(
                    occluder.occluder_id,
                    tuple(occluder.lower[index] + shift[index] for index in range(3)),
                    tuple(occluder.upper[index] + shift[index] for index in range(3)),
                )
            )
            found = True
        else:
            updated.append(occluder)
    if not found:
        raise ProbeError(ErrorCode.SCENE_INVALID, f"sample occluder {occluder_id} not found")
    return dataclasses.replace(scene, occluders=tuple(updated))


def run_incremental_demo(
    scene: Scene,
    output_root: str | Path,
    config: BakeConfig,
) -> dict[str, Any]:
    root = Path(output_root)
    root.mkdir(parents=True, exist_ok=True)
    trace = TraceLog(root / "trace.jsonl")
    engine = BakeEngine(trace=trace)
    publisher = PublishedStore()
    snapshot_store = SnapshotStore(root / "snapshots")
    versions: list[dict[str, Any]] = []

    baseline = engine.bake(scene, config)
    snapshot_v1 = publisher.publish(baseline.state, version=1)
    snapshot_store.commit_state(baseline.state, version=1)
    baseline_query = snapshot_v1.query_at(
        (
            (scene.bounds_min[0] + scene.bounds_max[0]) * 0.5,
            (scene.bounds_min[1] + scene.bounds_max[1]) * 0.5,
            (scene.bounds_min[2] + scene.bounds_max[2]) * 0.5,
        )
    )
    versions.append(
        {
            "version": 1,
            "kind": "baseline",
            "probe_count": baseline.state.grid.count,
            "content_hash": baseline.state.content_hash,
            "query_version": baseline_query.version,
            "query_heat": baseline_query.heat,
        }
    )

    occluder_id = next(
        (
            item.occluder_id
            for item in scene.occluders
            if isinstance(item, BoxOccluder) and item.occluder_id != "back_wall"
        ),
        scene.occluders[0].occluder_id,
    )
    moved_scene = _move_occluder(scene, occluder_id)
    local = engine.bake(moved_scene, config, previous=baseline.state)
    snapshot_v2 = publisher.publish(local.state, version=2)
    snapshot_store.commit_state(local.state, version=2)
    versions.append(
        {
            "version": 2,
            "kind": "local_occluder_change",
            "changed_occluder": occluder_id,
            **local.to_dict(include_probe_lists=True),
            "query_version": snapshot_v2.query_probe(0).version,
        }
    )
    if local.plan is None or local.plan.recomputed.size >= local.state.grid.count:
        raise ProbeError(
            ErrorCode.INTERP_FAILED,
            "sample local occluder edit did not produce a partial invalidation",
        )

    compensation_config = dataclasses.replace(
        config,
        compensation_strength=min(1.0, config.compensation_strength + 0.15),
    )
    compensation = engine.bake(moved_scene, compensation_config, previous=local.state)
    snapshot_v3 = publisher.publish(compensation.state, version=3)
    snapshot_store.commit_state(compensation.state, version=3)
    versions.append(
        {
            "version": 3,
            "kind": "compensation_change",
            **compensation.to_dict(include_probe_lists=True),
            "query_version": snapshot_v3.query_probe(0).version,
        }
    )
    if compensation.plan is None or compensation.plan.recomputed.size >= compensation.state.grid.count:
        raise ProbeError(
            ErrorCode.INTERP_FAILED,
            "sample compensation edit did not reuse unaffected probes",
        )

    changed_sky = Sky(
        moved_scene.sky.zenith,
        moved_scene.sky.horizon,
        moved_scene.sky.ground,
        moved_scene.sky.intensity * 1.1,
    )
    sky_scene = dataclasses.replace(moved_scene, sky=changed_sky)
    global_change = engine.bake(
        sky_scene, compensation_config, previous=compensation.state
    )
    snapshot_v4 = publisher.publish(global_change.state, version=4)
    snapshot_store.commit_state(global_change.state, version=4)
    versions.append(
        {
            "version": 4,
            "kind": "global_sky_change",
            **global_change.to_dict(),
            "query_version": snapshot_v4.query_probe(0).version,
        }
    )
    if global_change.plan is None or "sky" not in global_change.plan.reasons:
        raise ProbeError(ErrorCode.INTERP_FAILED, "global sky invalidation reason was not recorded")

    stale = None
    try:
        publisher.query_at(tuple(snapshot_v2.grid.centers[0]), version=2)
    except ProbeError as exc:
        stale = exc.to_dict()
    if stale is None or stale["code"] != ErrorCode.VERSION_STALE:
        raise ProbeError(ErrorCode.VERSION_STALE, "expected stale-version query failure")

    old_snapshot_query = snapshot_v1.query_at(tuple(snapshot_v1.grid.centers[0]))
    result = {
        "scene_id": scene.scene_id,
        "versions": versions,
        "history_versions": list(publisher.history_versions()),
        "stale_query": stale,
        "old_snapshot_object_still_readable": {
            "version": old_snapshot_query.version,
            "content_hash": old_snapshot_query.content_hash,
        },
        "trace_log": str(root / "trace.jsonl"),
        "snapshot_root": str(root / "snapshots"),
        "clean_exit": True,
    }
    write_json_atomic(root / "incremental_summary.json", result)
    trace.event("acceptance_incremental_completed", summary=result)
    return result

