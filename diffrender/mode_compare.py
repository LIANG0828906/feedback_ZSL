"""Forward-mode comparison reports for exact versus downsampled rendering."""

from __future__ import annotations

from typing import Any

import numpy as np
import torch

from .errors import fail
from .params import build_layout, raw_from_physical_values, scene_from_raw
from .renderer import RenderConfig, render_image
from .schema import Task, decode_target


def compare_downsample_modes(
    task: Task,
    *,
    factors: tuple[int, ...] = (1, 2, 4, 8, 16),
) -> dict[str, Any]:
    if not factors or factors[0] != 1:
        fail(
            "E_PARA",
            "downsample comparison factors must start with exact factor 1",
            "factors",
        )
    if list(factors) != sorted(set(factors)):
        fail(
            "E_PARA",
            "downsample factors must be strictly increasing",
            "factors",
        )
    if any(factor not in {1, 2, 4, 8, 16} for factor in factors):
        fail(
            "E_PARA",
            "downsample factors must be selected from 1, 2, 4, 8, 16",
            "factors",
            values=list(factors),
        )
    layout = build_layout(task.scene)
    raw = raw_from_physical_values(task.scene, layout, task.truth.values)
    scene = scene_from_raw(task.scene, layout, raw)
    cameras = {camera.id: camera for camera in task.scene.cameras}
    targets = [
        torch.tensor(
            decode_target(task.dataset_root / target.path, target),
            dtype=torch.float64,
        )
        for target in task.targets
    ]
    reports: list[dict[str, Any]] = []
    mae_values: list[float] = []
    rmse_values: list[float] = []
    max_values: list[float] = []
    for factor in factors:
        image_maes: list[float] = []
        image_rmses: list[float] = []
        image_maxima: list[float] = []
        for index, target in enumerate(task.targets):
            with torch.no_grad():
                image = render_image(
                    scene,
                    cameras[target.camera_id],
                    target,
                    RenderConfig(
                        shadow_samples=task.scene.soft_shadow_samples,
                        ray_epsilon=task.scene.ray_epsilon,
                        downsample_factor=factor,
                        base_seed=task.scene.target_seed,
                    ),
                    seed_offset=index,
                )
            difference = (image - targets[index]).abs()
            image_maes.append(float(difference.mean().item()))
            image_rmses.append(
                float(torch.sqrt(torch.mean(difference.square())).item())
            )
            image_maxima.append(float(difference.max().item()))
        mean_mae = float(np.mean(image_maes))
        mean_rmse = float(np.mean(image_rmses))
        max_abs = float(np.max(image_maxima))
        mae_values.append(mean_mae)
        rmse_values.append(mean_rmse)
        max_values.append(max_abs)
        reports.append(
            {
                "downsample_factor": factor,
                "rendered_resolution": [
                    max(1, task.targets[0].width // factor),
                    max(1, task.targets[0].height // factor),
                ],
                "mean_mae": mean_mae,
                "mean_rmse": mean_rmse,
                "max_abs": max_abs,
                "per_image_mae": image_maes,
            }
        )

    def monotonic(values: list[float]) -> bool:
        return all(
            later + 1.0e-15 >= earlier
            for earlier, later in zip(values, values[1:])
        )

    return {
        "task_id": task.scene.scene_id,
        "reference": "exact target images",
        "factors": list(factors),
        "rows": reports,
        "monotonic_mae": monotonic(mae_values),
        "monotonic_rmse": monotonic(rmse_values),
        "monotonic_max_abs": monotonic(max_values),
        "monotonic_non_decreasing": (
            monotonic(mae_values)
            and monotonic(rmse_values)
        ),
        "note": (
            "Difference metrics are measured against exact-mode targets; "
            "aggregate MAE and RMSE are the acceptance metrics and are "
            "non-decreasing with factor. Per-pixel max_abs is reported for "
            "diagnosis but can fluctuate at isolated high-dynamic-range pixels."
        ),
    }

