"""Deterministic local sample generation with no external assets."""

from __future__ import annotations

import json
from collections import OrderedDict
from pathlib import Path
from typing import Any

import numpy as np

from .hashing import atomic_write_json, file_sha256, tensor_content_hash
from .params import (
    ParameterStore,
    group_specs_to_ordered,
    resolve_group_specs,
)
from .rendering import full_render
from .scene import load_scene


PRESETS: tuple[dict[str, Any], ...] = (
    {
        "name": "coupled_small",
        "width": 16,
        "height": 12,
        "coupling_level": 0,
        "group_mode": "category",
        "seed_offset": 11,
    },
    {
        "name": "coupled_medium",
        "width": 24,
        "height": 18,
        "coupling_level": 1,
        "group_mode": "object",
        "seed_offset": 29,
    },
    {
        "name": "coupled_large",
        "width": 32,
        "height": 24,
        "coupling_level": 2,
        "group_mode": "fine",
        "seed_offset": 47,
    },
)


def _group_raw(mode: str) -> list[OrderedDict[str, Any]]:
    if mode == "category":
        groups = [
            ("camera", "camera", ["camera.*"]),
            ("geometry", "geometry", ["geometry.*"]),
            ("material", "material", ["material.*"]),
            ("light", "light", ["light.*"]),
        ]
    elif mode == "object":
        groups = [
            ("camera", "camera", ["camera.*"]),
            ("geometry.primary", "geometry", ["geometry.primary.*", "geometry.floor.*"]),
            ("geometry.accent", "geometry", ["geometry.accent.*"]),
            (
                "material.primary",
                "material",
                ["material.primary.*", "material.floor.*"],
            ),
            ("material.accent", "material", ["material.accent.*"]),
            ("light", "light", ["light.*"]),
        ]
    elif mode == "fine":
        groups = [
            ("camera", "camera", ["camera.*"]),
            ("geometry.primary", "geometry", ["geometry.primary.*"]),
            ("geometry.accent", "geometry", ["geometry.accent.*"]),
            ("geometry.floor", "geometry", ["geometry.floor.*"]),
            ("material.primary", "material", ["material.primary.*"]),
            ("material.accent", "material", ["material.accent.*"]),
            ("material.floor", "material", ["material.floor.*"]),
            ("light", "light", ["light.*"]),
        ]
    else:
        raise ValueError(f"unknown group mode {mode}")
    return [
        OrderedDict((("id", group_id), ("kind", kind), ("parameters", patterns)))
        for group_id, kind, patterns in groups
    ]


def _scene_raw(
    *,
    name: str,
    width: int,
    height: int,
    coupling_level: int,
) -> OrderedDict[str, Any]:
    if coupling_level == 0:
        centers = ([-0.82, -0.10, 0.00], [0.82, 0.06, -0.05])
        light_direction = [-0.55, -0.82, -0.22]
        softness = 0.025
    elif coupling_level == 1:
        centers = ([-0.58, -0.06, 0.10], [0.48, 0.02, -0.12])
        light_direction = [-0.42, -0.76, -0.30]
        softness = 0.045
    else:
        centers = ([-0.36, -0.02, 0.16], [0.34, 0.05, -0.18])
        light_direction = [-0.30, -0.68, -0.42]
        softness = 0.065
    return OrderedDict(
        (
            ("schema_version", 1),
            ("name", name),
            ("resolution", [width, height]),
            (
                "camera",
                OrderedDict(
                    (
                        ("position", [0.18, 0.52, 4.15]),
                        ("target", [0.0, -0.08, 0.0]),
                        ("up", [0.0, 1.0, 0.0]),
                        ("fov_deg", 48.0),
                    )
                ),
            ),
            (
                "geometry",
                OrderedDict(
                    (
                        (
                            "spheres",
                            [
                                OrderedDict(
                                    (
                                        ("name", "primary"),
                                        ("center", centers[0]),
                                        ("radius", 0.63),
                                    )
                                ),
                                OrderedDict(
                                    (
                                        ("name", "accent"),
                                        ("center", centers[1]),
                                        ("radius", 0.53),
                                    )
                                ),
                            ],
                        ),
                        (
                            "plane",
                            OrderedDict(
                                (
                                    ("name", "floor"),
                                    ("point", [0.0, -1.02, 0.0]),
                                    ("normal", [0.0, 1.0, 0.0]),
                                )
                            ),
                        ),
                    )
                ),
            ),
            (
                "materials",
                OrderedDict(
                    (
                        ("primary", OrderedDict((("albedo", [0.84, 0.22, 0.12]),))),
                        ("accent", OrderedDict((("albedo", [0.12, 0.34, 0.88]),))),
                        ("floor", OrderedDict((("albedo", [0.48, 0.50, 0.54]),))),
                    )
                ),
            ),
            (
                "light",
                OrderedDict(
                    (
                        ("direction", light_direction),
                        ("intensity", 2.55),
                    )
                ),
            ),
            ("shadow_softness", softness),
            ("coupling_level", coupling_level),
            ("target", OrderedDict((("path", "target.npy"),))),
        )
    )


def _target_values(
    store: ParameterStore,
    rng: np.random.Generator,
    *,
    coupling_level: int,
) -> OrderedDict[str, OrderedDict[str, Any]]:
    jitter = 0.008 + 0.006 * coupling_level
    updates: OrderedDict[str, OrderedDict[str, Any]] = OrderedDict()
    for group_id in store.group_order:
        group_values: OrderedDict[str, Any] = OrderedDict()
        for path in store.parameter_order[group_id]:
            current = store.parameters[path].detach().cpu().numpy().copy()
            if path.startswith("camera.") and path != "camera.fov_deg":
                value = current + rng.normal(0.0, jitter * 0.35, size=current.shape)
            elif path.endswith(".fov_deg"):
                value = np.asarray(float(current) + rng.normal(0.0, 0.7))
            elif ".radius" in path:
                value = np.asarray(float(current) + rng.normal(0.0, 0.025))
            elif path.startswith("geometry."):
                value = current + rng.normal(0.0, jitter * 0.55, size=current.shape)
            elif path.startswith("material."):
                value = np.clip(current + rng.normal(0.0, 0.055, size=current.shape), 0.02, 1.0)
            elif path == "light.direction":
                value = current + rng.normal(0.0, 0.025, size=current.shape)
            elif path == "light.intensity":
                value = np.asarray(float(current) + rng.normal(0.0, 0.08))
            else:
                value = current
            group_values[path] = value.tolist() if np.ndim(value) else float(value)
        updates[group_id] = group_values
    return updates


def generate_sample(
    output_root: str | Path,
    *,
    seed: int,
    preset_index: int,
) -> OrderedDict[str, Any]:
    preset = PRESETS[preset_index]
    sample_dir = Path(output_root).resolve() / str(preset["name"])
    sample_dir.mkdir(parents=True, exist_ok=True)
    raw = _scene_raw(
        name=str(preset["name"]),
        width=int(preset["width"]),
        height=int(preset["height"]),
        coupling_level=int(preset["coupling_level"]),
    )
    scene_path = sample_dir / "scene.json"
    groups_path = sample_dir / "groups.json"
    target_path = sample_dir / "target.npy"
    atomic_write_json(scene_path, raw)
    group_raw = _group_raw(str(preset["group_mode"]))
    atomic_write_json(
        groups_path,
        OrderedDict((("schema_version", 1), ("groups", group_raw))),
    )

    zero_target = np.zeros((int(preset["height"]), int(preset["width"]), 3), dtype=np.float64)
    scene = load_scene(scene_path, target_override=zero_target)
    groups = resolve_group_specs(scene, {"schema_version": 1, "groups": group_raw})
    store = ParameterStore(scene, groups)
    rng = np.random.default_rng((int(seed) * 131 + int(preset["seed_offset"])) & 0xFFFFFFFF)
    updates = _target_values(store, rng, coupling_level=int(preset["coupling_level"]))
    for group_id in store.group_order:
        store.update_group(group_id, updates[group_id], increment_version=False)
    rendered_nodes, _ = full_render(store, scene)
    image = rendered_nodes["lighting"]["rgb"].detach().cpu().numpy()
    np.save(target_path, np.ascontiguousarray(image, dtype=np.float64), allow_pickle=False)

    manifest = OrderedDict(
        (
            ("schema_version", 1),
            ("sample", str(preset["name"])),
            ("seed", int(seed)),
            ("preset_index", int(preset_index)),
            ("resolution", [int(preset["width"]), int(preset["height"])]),
            ("coupling_level", int(preset["coupling_level"])),
            ("group_mode", str(preset["group_mode"])),
            ("group_count", len(groups)),
            ("scene", "scene.json"),
            ("groups", "groups.json"),
            ("target", "target.npy"),
            ("scene_hash", file_sha256(scene_path)),
            ("groups_hash", file_sha256(groups_path)),
            ("target_hash", file_sha256(target_path)),
            ("target_content_hash", tensor_content_hash(torch_tensor_from_numpy(image))),
        )
    )
    atomic_write_json(sample_dir / "manifest.json", manifest)
    return manifest


def torch_tensor_from_numpy(array: np.ndarray):
    import torch

    return torch.as_tensor(np.array(array, copy=True), dtype=torch.float64)


def generate_samples(
    output_root: str | Path,
    *,
    seed: int = 20261121,
) -> OrderedDict[str, Any]:
    manifests = [
        generate_sample(output_root, seed=seed, preset_index=index)
        for index in range(len(PRESETS))
    ]
    result = OrderedDict(
        (
            ("schema_version", 1),
            ("seed", int(seed)),
            ("samples", manifests),
        )
    )
    atomic_write_json(Path(output_root).resolve() / "manifest.json", result)
    return result

