from __future__ import annotations

import numpy as np
import torch

from diffrender.errors import AppError
from diffrender.params import (
    build_layout,
    raw_from_physical_values,
    scene_from_raw,
)
from diffrender.renderer import RenderConfig, render_image


def test_exact_forward_matches_generated_targets(dataset):
    task = dataset.task("single")
    layout = build_layout(task.scene)
    raw = raw_from_physical_values(task.scene, layout, task.truth.values)
    scene = scene_from_raw(task.scene, layout, raw)
    cameras = {camera.id: camera for camera in task.scene.cameras}
    for index, target in enumerate(task.targets):
        image = render_image(
            scene,
            cameras[target.camera_id],
            target,
            RenderConfig(
                shadow_samples=task.scene.soft_shadow_samples,
                ray_epsilon=task.scene.ray_epsilon,
                downsample_factor=1,
                base_seed=task.scene.target_seed,
            ),
            seed_offset=index,
        )
        expected = torch.tensor(
            np.load(dataset.root / target.path, allow_pickle=False),
            dtype=torch.float64,
        )
        assert torch.equal(image, expected)


def test_gradients_for_geometry_and_material_are_finite_and_nonzero(dataset):
    task = dataset.task("single")
    layout = build_layout(task.scene)
    raw = raw_from_physical_values(
        task.scene, layout, task.truth.values
    ).requires_grad_(True)
    scene = scene_from_raw(task.scene, layout, raw)
    cameras = {camera.id: camera for camera in task.scene.cameras}
    loss = torch.zeros((), dtype=torch.float64)
    for index, target in enumerate(task.targets):
        image = render_image(
            scene,
            cameras[target.camera_id],
            target,
            RenderConfig(
                shadow_samples=task.scene.soft_shadow_samples,
                ray_epsilon=task.scene.ray_epsilon,
                base_seed=task.scene.target_seed,
            ),
            seed_offset=index,
        )
        loss = loss + image.square().mean()
    gradient = torch.autograd.grad(loss, raw)[0]
    assert torch.isfinite(gradient).all()
    for label in (
        "sphere_0.center.x",
        "sphere_0.center.y",
        "sphere_0.center.z",
        "sphere_0.radius",
        "sphere_0.albedo.r",
        "sphere_0.albedo.g",
        "sphere_0.albedo.b",
        "key_light.intensity",
    ):
        assert abs(float(gradient[layout.label_to_index[label]].item())) > 0.0


def test_render_parameter_errors_use_para_code(dataset):
    task = dataset.task("single")
    with pytest_raises("E_PARA", "shadow_samples"):
        RenderConfig(shadow_samples=0).validate()
    with pytest_raises("E_PARA", "downsample_factor"):
        RenderConfig(shadow_samples=1, downsample_factor=3).validate()
    with pytest_raises("E_PARA", "ray_epsilon"):
        RenderConfig(shadow_samples=1, ray_epsilon=0.0).validate()


class pytest_raises:
    def __init__(self, code: str, field_path: str) -> None:
        self.code = code
        self.field_path = field_path

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, traceback):
        assert exc is not None
        assert isinstance(exc, AppError)
        assert exc.code == self.code
        assert exc.field_path == self.field_path
        return True

