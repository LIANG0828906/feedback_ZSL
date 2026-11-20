"""Fair dense-versus-sparse error comparison with an identical direction set."""

from __future__ import annotations

import time
from dataclasses import replace
from typing import Any

import numpy as np

from .bake import BakeEngine
from .scene import Scene
from .sh import irradiance_for_normals
from .strategy import STRATEGY_DENSE, STRATEGY_SPARSE, BakeConfig


def _relative_probe_error(
    candidate_coefficients: np.ndarray,
    reference_coefficients: np.ndarray,
    directions: np.ndarray,
) -> dict[str, float]:
    candidate = irradiance_for_normals(candidate_coefficients, directions)
    reference = irradiance_for_normals(reference_coefficients, directions)
    difference = np.linalg.norm((candidate - reference).reshape(candidate.shape[0], -1), axis=1)
    denominator = np.maximum(
        np.linalg.norm(reference.reshape(reference.shape[0], -1), axis=1), 1e-12
    )
    relative = difference / denominator
    return {
        "mean_relative_error": float(np.mean(relative)),
        "max_relative_error": float(np.max(relative)),
        "p95_relative_error": float(np.percentile(relative, 95)),
    }


def compare_strategies(
    scene: Scene,
    base_config: BakeConfig,
    *,
    engine: BakeEngine | None = None,
) -> dict[str, Any]:
    """Compare both strategies against per-probe full-band sampling.

    The comparison never lowers the direction count and never truncates the
    reference. Dense and sparse both use exactly the same deterministic directions.
    """

    actual_engine = engine or BakeEngine()
    base_config.validate()
    directions = base_config.resolved_directions()
    dense_config = replace(base_config, strategy=STRATEGY_DENSE)
    sparse_config = replace(base_config, strategy=STRATEGY_SPARSE)
    reference_config = replace(base_config, strategy=STRATEGY_DENSE, sh_order=6)
    started = time.perf_counter()
    reference = actual_engine.bake(scene, reference_config)
    dense = actual_engine.bake(scene, dense_config)
    sparse = actual_engine.bake(scene, sparse_config)
    elapsed = time.perf_counter() - started
    dense_error = _relative_probe_error(
        dense.state.artifacts.coefficients,
        reference.state.artifacts.coefficients,
        directions,
    )
    sparse_error = _relative_probe_error(
        sparse.state.artifacts.coefficients,
        reference.state.artifacts.coefficients,
        directions,
    )
    return {
        "scene_id": scene.scene_id,
        "probe_count": dense.state.grid.count,
        "direction_count": int(directions.shape[0]),
        "sh_order": base_config.sh_order,
        "shared_direction_hash": dense.state.config.factor_hashes()["directions"],
        "reference": "per-probe order-6 SH over the same direction set",
        "dense": {
            "decision": dense.state.decision.to_dict(),
            "sampled_probes": int(np.sum(dense.state.artifacts.sampled_mask)),
            **dense_error,
            "estimated_error_upper_bound": dense.actual_error_upper_bound,
            "elapsed_seconds": dense.elapsed_seconds,
        },
        "sparse": {
            "decision": sparse.state.decision.to_dict(),
            "sampled_probes": int(np.sum(sparse.state.artifacts.sampled_mask)),
            **sparse_error,
            "estimated_error_upper_bound": sparse.actual_error_upper_bound,
            "elapsed_seconds": sparse.elapsed_seconds,
        },
        "comparison_elapsed_seconds": elapsed,
        "direction_coverage_reduced": False,
        "reference_uses_truncated_data": False,
    }

