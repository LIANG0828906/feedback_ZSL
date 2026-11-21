from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any

from .errors import ProbeError, sh_invalid
from .scene import Scene, SceneDefaults


@dataclass(frozen=True)
class BakeConfig:
    spacing: float = 2.0
    sh_order: int = 3
    direction_count: int = 64
    direction_seed: int = 0
    compensation_strength: float = 0.35
    audit_direction_count: int = 16
    strategy: str = "auto"
    interpolation_neighbors: int = 8
    max_dense_ray_samples: int = 75_000
    max_dense_coefficient_bytes: int = 2_000_000
    chunk_size: int = 32
    validation_probe_limit: int = 24
    validation_direction_count: int = 192

    @classmethod
    def from_scene(cls, scene: Scene, **overrides: Any) -> "BakeConfig":
        defaults = scene.defaults
        values = {
            "spacing": defaults.probe_spacing,
            "sh_order": defaults.sh_order,
            "direction_count": defaults.direction_count,
            "direction_seed": defaults.direction_seed,
            "compensation_strength": defaults.compensation_strength,
            "strategy": defaults.strategy,
        }
        values.update({key: value for key, value in overrides.items() if value is not None})
        return cls(**values).validated()

    def validated(self) -> "BakeConfig":
        if isinstance(self.spacing, bool) or not isinstance(self.spacing, (int, float)):
            raise ProbeError(
                "E_SCENE_INVALID",
                "probe spacing must be numeric",
                {"path": "settings.probe_spacing"},
            )
        if not math.isfinite(float(self.spacing)) or self.spacing <= 0.0:
            raise ProbeError(
                "E_SCENE_INVALID",
                "probe spacing must be finite and positive",
                {"path": "settings.probe_spacing"},
            )
        if isinstance(self.sh_order, bool) or not isinstance(self.sh_order, int) or not 2 <= self.sh_order <= 6:
            raise sh_invalid("sh_order", "spherical-harmonic order must be an integer in [2, 6]")
        if (
            isinstance(self.direction_count, bool)
            or not isinstance(self.direction_count, int)
            or self.direction_count <= 0
        ):
            raise sh_invalid("directions", "direction set must not be empty")
        if (
            isinstance(self.audit_direction_count, bool)
            or not isinstance(self.audit_direction_count, int)
            or self.audit_direction_count <= 0
        ):
            raise sh_invalid("audit_directions", "audit direction set must not be empty")
        if not 0.0 <= float(self.compensation_strength) <= 2.0:
            raise ProbeError(
                "E_SCENE_INVALID",
                "compensation strength must be in [0, 2]",
                {"path": "settings.compensation_strength"},
            )
        if self.strategy not in {"auto", "dense", "sparse"}:
            raise ProbeError(
                "E_SCENE_INVALID",
                "strategy must be 'auto', 'dense', or 'sparse'",
                {"path": "settings.strategy"},
            )
        if self.chunk_size <= 0:
            raise ProbeError(
                "E_SCENE_INVALID", "chunk_size must be positive", {"path": "chunk_size"}
            )
        if self.interpolation_neighbors <= 0:
            raise ProbeError(
                "E_SCENE_INVALID",
                "interpolation_neighbors must be positive",
                {"path": "interpolation_neighbors"},
            )
        return self

    def fingerprint(self) -> dict[str, Any]:
        return {
            "spacing": float(self.spacing),
            "sh_order": self.sh_order,
            "direction_count": self.direction_count,
            "direction_seed": self.direction_seed,
            "compensation_strength": float(self.compensation_strength),
            "audit_direction_count": self.audit_direction_count,
            "strategy": self.strategy,
            "interpolation_neighbors": self.interpolation_neighbors,
            "max_dense_ray_samples": self.max_dense_ray_samples,
            "max_dense_coefficient_bytes": self.max_dense_coefficient_bytes,
            "chunk_size": self.chunk_size,
            "validation_probe_limit": self.validation_probe_limit,
            "validation_direction_count": self.validation_direction_count,
        }


@dataclass(frozen=True)
class StrategyAudit:
    chosen: str
    reason: str
    probe_count: int
    sh_order: int
    direction_count: int
    audit_direction_count: int
    sampled_probe_count: int
    interpolated_probe_count: int
    estimated_ray_samples: int
    estimated_sh_projections: int
    coefficient_bytes: int
    sampling_method: str
    truth_source: str
    validation_probe_ids: tuple[str, ...]
    dense_rms_error: float
    dense_max_error: float
    sparse_rms_error: float
    sparse_max_error: float
    selected_rms_error: float
    selected_max_error: float
    error_upper_bound: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "chosen": self.chosen,
            "reason": self.reason,
            "probe_count": self.probe_count,
            "sh_order": self.sh_order,
            "direction_count": self.direction_count,
            "audit_direction_count": self.audit_direction_count,
            "sampled_probe_count": self.sampled_probe_count,
            "interpolated_probe_count": self.interpolated_probe_count,
            "estimated_ray_samples": self.estimated_ray_samples,
            "estimated_sh_projections": self.estimated_sh_projections,
            "coefficient_bytes": self.coefficient_bytes,
            "sampling_method": self.sampling_method,
            "truth_source": self.truth_source,
            "validation_probe_ids": list(self.validation_probe_ids),
            "dense_rms_error": self.dense_rms_error,
            "dense_max_error": self.dense_max_error,
            "sparse_rms_error": self.sparse_rms_error,
            "sparse_max_error": self.sparse_max_error,
            "selected_rms_error": self.selected_rms_error,
            "selected_max_error": self.selected_max_error,
            "error_upper_bound": self.error_upper_bound,
            "estimated_cost_basis": {
                "ray_samples": self.estimated_ray_samples,
                "sh_projections": self.estimated_sh_projections,
                "coefficient_bytes": self.coefficient_bytes,
                "note": (
                    "Primary incremental cost is ray/object tests; SH projections and "
                    "read-only coefficient traffic are reported separately."
                ),
            },
        }


@dataclass(frozen=True)
class StrategyPlan:
    name: str
    source_indices: tuple[int, ...]
    stride: int
    audit: StrategyAudit

    @property
    def source_count(self) -> int:
        return len(self.source_indices)


@dataclass(frozen=True)
class InfluenceReport:
    changed_factors: tuple[str, ...]
    seed_probe_ids: tuple[str, ...]
    affected_probe_ids: tuple[str, ...]
    reasons: dict[str, str]
    conservative: bool
    conservative_reason: str | None = None

    @property
    def affected_count(self) -> int:
        return len(self.affected_probe_ids)

    @property
    def seed_count(self) -> int:
        return len(self.seed_probe_ids)

    def to_dict(self) -> dict[str, Any]:
        return {
            "changed_factors": list(self.changed_factors),
            "seed_count": self.seed_count,
            "affected_count": self.affected_count,
            "seed_probe_ids": list(self.seed_probe_ids),
            "affected_probe_ids": list(self.affected_probe_ids),
            "reasons": self.reasons,
            "conservative": self.conservative,
            "conservative_reason": self.conservative_reason,
        }


@dataclass
class BakeReport:
    scene_id: str
    version: int
    strategy: str
    probe_count: int
    reused_count: int
    recomputed_count: int
    max_coefficient_deviation: float
    coefficient_deviation_probe_count: int
    max_recomputed_truncation_error: float
    grid_change: dict[str, Any]
    influence: dict[str, Any]
    strategy_audit: dict[str, Any]
    grid_manifest: dict[str, Any]
    snapshot: dict[str, Any] = field(default_factory=dict)
    elapsed_seconds: float = 0.0

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": True,
            "scene_id": self.scene_id,
            "version": self.version,
            "strategy": self.strategy,
            "probe_count": self.probe_count,
            "reused_count": self.reused_count,
            "recomputed_count": self.recomputed_count,
            "max_coefficient_deviation": self.max_coefficient_deviation,
            "coefficient_deviation_probe_count": self.coefficient_deviation_probe_count,
            "coefficient_deviation_scope": "same_coordinate_hash",
            "max_recomputed_truncation_error": self.max_recomputed_truncation_error,
            "grid_change": self.grid_change,
            "influence": self.influence,
            "strategy_audit": self.strategy_audit,
            "grid_manifest": self.grid_manifest,
            "snapshot": self.snapshot,
            "elapsed_seconds": self.elapsed_seconds,
        }

