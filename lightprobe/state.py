from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Mapping

from .bake_types import BakeConfig, StrategyPlan
from .directions import DirectionSet
from .grid import ProbeGrid
from .record import ProbeRecord
from .scene import Scene
from .strategy import InterpolationIndex
from .util import sha256_json


@dataclass(frozen=True)
class BakeState:
    scene: Scene
    grid: ProbeGrid
    directions: DirectionSet
    audit_directions: DirectionSet
    config: BakeConfig
    strategy: StrategyPlan
    interpolation: InterpolationIndex
    records: Mapping[str, ProbeRecord]
    factor_versions: Mapping[str, str]

    def content_hash(self) -> str:
        return sha256_json(
            {
                "scene_id": self.scene.scene_id,
                "grid": self.grid.content_hash,
                "directions": self.directions.fingerprint,
                "sh_order": self.config.sh_order,
                "strategy": self.strategy.name,
                "factor_versions": dict(self.factor_versions),
                "probes": [
                    [probe_id, self.records[probe_id].content_hash]
                    for probe_id in sorted(self.records)
                ],
            }
        )

    def to_summary(self) -> dict[str, Any]:
        return {
            "scene_id": self.scene.scene_id,
            "probe_count": self.grid.probe_count,
            "spacing": self.grid.spacing,
            "bounds": {
                "min": list(self.scene.bounds_min),
                "max": list(self.scene.bounds_max),
            },
            "sh_order": self.config.sh_order,
            "direction_count": self.config.direction_count,
            "compensation_strength": self.config.compensation_strength,
            "strategy": self.strategy.name,
            "factor_versions": dict(self.factor_versions),
            "content_hash": self.content_hash(),
        }


def factor_versions(
    scene: Scene,
    grid: ProbeGrid,
    directions: DirectionSet,
    audit_directions: DirectionSet,
    config: BakeConfig,
    strategy: str,
) -> dict[str, str]:
    return {
        "grid": grid.content_hash,
        "occluders": sha256_json(
            [
                {
                    "geometry": item.fingerprint(),
                    "material": scene.materials[item.material].fingerprint(),
                }
                for item in scene.sorted_occluders()
            ]
        ),
        "materials": sha256_json(
            [material.fingerprint() for material in scene.sorted_materials()]
        ),
        "sky": scene.sky.content_hash(),
        "directions": directions.fingerprint,
        "audit_directions": audit_directions.fingerprint,
        "sh_order": sha256_json(config.sh_order),
        "compensation_strength": sha256_json(config.compensation_strength),
        "strategy": sha256_json(strategy),
    }

