from __future__ import annotations

from typing import Mapping

from .bake_types import BakeConfig
from .directions import DirectionSet
from .grid import ProbeGrid
from .record import ProbeRecord
from .scene import Scene
from .util import sha256_json


def common_factor_version(
    scene: Scene,
    grid: ProbeGrid,
    directions: DirectionSet,
    audit_directions: DirectionSet,
    config: BakeConfig,
    strategy: str,
) -> str:
    return sha256_json(
        {
            "grid": grid.content_hash,
            "directions": directions.fingerprint,
            "audit_directions": audit_directions.fingerprint,
            "sh_order": config.sh_order,
            "sky": scene.sky.content_hash(),
            "strategy": strategy,
            "audit_count": config.audit_direction_count,
        }
    )


def expected_influence_version(
    record: ProbeRecord,
    *,
    scene: Scene,
    grid: ProbeGrid,
    directions: DirectionSet,
    audit_directions: DirectionSet,
    config: BakeConfig,
    strategy: str,
    records: Mapping[str, ProbeRecord],
    common_version: str | None = None,
) -> str:
    common = common_version or common_factor_version(
        scene, grid, directions, audit_directions, config, strategy
    )
    hit_object_versions = []
    for index in sorted(set(record.first_hit_ids)):
        if index < 0:
            continue
        object_id = scene.occluder_ids[index]
        object_hash = scene.object_hash(object_id)
        hit_object_versions.append(
            {"object_id": object_id, "object_hash": object_hash or "missing"}
        )

    source_versions = []
    for source_id in record.source_ids:
        source = records.get(source_id)
        source_versions.append(
            {
                "probe_id": source_id,
                "payload_hash": "missing" if source is None else source.payload_hash,
            }
        )

    compensation = {"strength": "none", "neighbors": []}
    if record.leak_score > 1.0e-12:
        neighbors = []
        for neighbor_id in record.comp_source_ids:
            neighbor = records.get(neighbor_id)
            neighbors.append(
                {
                    "probe_id": neighbor_id,
                    "payload_hash": "missing" if neighbor is None else neighbor.payload_hash,
                }
            )
        compensation = {
            "strength": round(float(config.compensation_strength), 12),
            "neighbors": neighbors,
        }

    return sha256_json(
        {
            "common": common,
            "hit_objects": hit_object_versions,
            "sources": source_versions,
            "compensation": compensation,
        }
    )


def record_reusable(
    record: ProbeRecord,
    *,
    scene: Scene,
    grid: ProbeGrid,
    directions: DirectionSet,
    audit_directions: DirectionSet,
    config: BakeConfig,
    strategy: str,
    records: Mapping[str, ProbeRecord],
    common_version: str | None = None,
) -> tuple[bool, str]:
    if not record.verify_content_hash():
        return False, "content_hash_mismatch"
    expected = expected_influence_version(
        record,
        scene=scene,
        grid=grid,
        directions=directions,
        audit_directions=audit_directions,
        config=config,
        strategy=strategy,
        records=records,
        common_version=common_version,
    )
    if expected != record.influence_version:
        return False, "influence_version_mismatch"
    return True, "ok"

