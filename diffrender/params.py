"""Differentiable scene parameter layout and physical parameter transforms."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterable

import torch
import torch.nn.functional as F

from .errors import fail
from .schema import PlaneSpec, SceneSpec, SphereSpec


def _softplus_inverse(value: float) -> float:
    value = max(float(value), 1.0e-8)
    if value < 20.0:
        return math.log(math.expm1(value))
    return value + math.log1p(-math.exp(-value))


def _logit(value: float) -> float:
    value = min(max(float(value), 1.0e-6), 1.0 - 1.0e-6)
    return math.log(value / (1.0 - value))


@dataclass(frozen=True)
class ParameterEntry:
    label: str
    start: int
    stop: int
    kind: str
    object_id: str


@dataclass(frozen=True)
class SceneParameterLayout:
    entries: tuple[ParameterEntry, ...]
    label_to_index: dict[str, int]
    size: int

    @property
    def labels(self) -> tuple[str, ...]:
        return tuple(
            label for entry in self.entries for label in entry.label.split("|")
        )


@dataclass(frozen=True)
class RenderScene:
    sphere_ids: tuple[str, ...]
    sphere_centers: torch.Tensor
    sphere_radii: torch.Tensor
    sphere_albedos: torch.Tensor
    plane_ids: tuple[str, ...]
    plane_points: torch.Tensor
    plane_normals: torch.Tensor
    plane_albedos: torch.Tensor
    light_ids: tuple[str, ...]
    light_centers: torch.Tensor
    light_normals: torch.Tensor
    light_half_sizes: torch.Tensor
    light_intensities: torch.Tensor
    background: torch.Tensor
    ambient: float
    ray_epsilon: float


def _add_entry(
    entries: list[ParameterEntry],
    label_to_index: dict[str, int],
    label: str,
    size: int,
    kind: str,
    object_id: str,
) -> int:
    start = entries[-1].stop if entries else 0
    entry = ParameterEntry(label, start, start + size, kind, object_id)
    entries.append(entry)
    cursor = start
    for scalar_label in label.split("|"):
        label_to_index[scalar_label] = cursor
        cursor += 1
    return start + size


def build_layout(scene: SceneSpec) -> SceneParameterLayout:
    entries: list[ParameterEntry] = []
    label_to_index: dict[str, int] = {}
    size = 0
    for geometry in scene.geometries:
        if isinstance(geometry, SphereSpec):
            size = _add_entry(
                entries,
                label_to_index,
                "|".join(f"{geometry.id}.center.{axis}" for axis in "xyz"),
                3,
                "center",
                geometry.id,
            )
            size = _add_entry(
                entries,
                label_to_index,
                f"{geometry.id}.radius",
                1,
                "radius",
                geometry.id,
            )
            size = _add_entry(
                entries,
                label_to_index,
                "|".join(f"{geometry.id}.albedo.{channel}" for channel in "rgb"),
                3,
                "albedo",
                geometry.id,
            )
        elif isinstance(geometry, PlaneSpec):
            size = _add_entry(
                entries,
                label_to_index,
                "|".join(f"{geometry.id}.albedo.{channel}" for channel in "rgb"),
                3,
                "albedo",
                geometry.id,
            )
    for light in scene.lights:
        size = _add_entry(
            entries,
            label_to_index,
            f"{light.id}.intensity",
            1,
            "intensity",
            light.id,
        )
    if len(label_to_index) != size:
        fail(
            "E_INTERNAL",
            "parameter layout construction produced inconsistent indices",
            "layout",
            labels=len(label_to_index),
            size=size,
        )
    return SceneParameterLayout(
        entries=tuple(entries),
        label_to_index=label_to_index,
        size=size,
    )


def raw_from_scene(scene: SceneSpec, layout: SceneParameterLayout) -> torch.Tensor:
    values: list[float] = []
    for geometry in scene.geometries:
        if isinstance(geometry, SphereSpec):
            values.extend(geometry.center)
            values.append(_softplus_inverse(geometry.radius))
            values.extend(_logit(v) for v in geometry.albedo)
        elif isinstance(geometry, PlaneSpec):
            values.extend(_logit(v) for v in geometry.albedo)
    for light in scene.lights:
        values.append(_softplus_inverse(light.intensity))
    raw = torch.tensor(values, dtype=torch.float64)
    if raw.numel() != layout.size:
        fail(
            "E_INTERNAL",
            "initial parameter vector does not match layout",
            "layout",
            expected=layout.size,
            actual=raw.numel(),
        )
    return raw


def raw_from_physical_values(
    scene: SceneSpec,
    layout: SceneParameterLayout,
    values: dict[str, float],
) -> torch.Tensor:
    missing = [label for label in layout.labels if label not in values]
    if missing:
        fail(
            "E_PARA",
            "physical parameter values are incomplete",
            "parameters",
            missing=missing,
        )
    raw = torch.empty(layout.size, dtype=torch.float64)
    for entry in layout.entries:
        labels = entry.label.split("|")
        for offset, label in enumerate(labels):
            value = float(values[label])
            if not math.isfinite(value):
                fail("E_PARA", "parameter value is not finite", label)
            if entry.kind == "radius":
                value = _softplus_inverse(value)
            elif entry.kind == "albedo":
                value = _logit(value)
            elif entry.kind == "intensity":
                value = _softplus_inverse(value)
            raw[entry.start + offset] = value
    return raw


def physical_values_from_raw(
    scene: SceneSpec,
    layout: SceneParameterLayout,
    raw: torch.Tensor,
) -> dict[str, float]:
    if raw.ndim != 1 or raw.numel() != layout.size:
        fail(
            "E_PARA",
            "raw parameter vector has an invalid shape",
            "raw_parameters",
            expected=[layout.size],
            actual=list(raw.shape),
        )
    if not torch.isfinite(raw).all():
        fail("E_PARA", "raw parameter vector contains non-finite values", "raw_parameters")
    values: list[float] = raw.detach().cpu().tolist()
    result: dict[str, float] = {}
    for geometry in scene.geometries:
        if isinstance(geometry, SphereSpec):
            for axis in "xyz":
                label = f"{geometry.id}.center.{axis}"
                result[label] = values[layout.label_to_index[label]]
            radius_raw = values[layout.label_to_index[f"{geometry.id}.radius"]]
            result[f"{geometry.id}.radius"] = float(
                F.softplus(torch.tensor(radius_raw, dtype=torch.float64)).item()
            )
            for channel in "rgb":
                label = f"{geometry.id}.albedo.{channel}"
                result[label] = float(torch.sigmoid(torch.tensor(values[layout.label_to_index[label]])).item())
        elif isinstance(geometry, PlaneSpec):
            for channel in "rgb":
                label = f"{geometry.id}.albedo.{channel}"
                result[label] = float(torch.sigmoid(torch.tensor(values[layout.label_to_index[label]])).item())
    for light in scene.lights:
        label = f"{light.id}.intensity"
        result[label] = float(
            F.softplus(torch.tensor(values[layout.label_to_index[label]], dtype=torch.float64)).item()
        )
    return result


def scene_from_raw(
    scene: SceneSpec,
    layout: SceneParameterLayout,
    raw: torch.Tensor,
) -> RenderScene:
    if raw.ndim != 1 or raw.numel() != layout.size:
        fail(
            "E_PARA",
            "raw parameter vector has an invalid shape",
            "raw_parameters",
            expected=[layout.size],
            actual=list(raw.shape),
        )
    if not torch.isfinite(raw).all():
        fail("E_PARA", "raw parameter vector contains non-finite values", "raw_parameters")

    spheres: list[SphereSpec] = []
    planes: list[PlaneSpec] = []
    for geometry in scene.geometries:
        if isinstance(geometry, SphereSpec):
            spheres.append(geometry)
        elif isinstance(geometry, PlaneSpec):
            planes.append(geometry)

    centers: list[torch.Tensor] = []
    radii: list[torch.Tensor] = []
    sphere_albedos: list[torch.Tensor] = []
    for sphere in spheres:
        center = torch.stack(
            [
                raw[layout.label_to_index[f"{sphere.id}.center.{axis}"]]
                for axis in "xyz"
            ]
        )
        radius = F.softplus(raw[layout.label_to_index[f"{sphere.id}.radius"]])
        albedo = torch.sigmoid(
            torch.stack(
                [
                    raw[layout.label_to_index[f"{sphere.id}.albedo.{channel}"]]
                    for channel in "rgb"
                ]
            )
        )
        centers.append(center)
        radii.append(radius)
        sphere_albedos.append(albedo)
    plane_albedos: list[torch.Tensor] = []
    for plane in planes:
        plane_albedos.append(
            torch.sigmoid(
                torch.stack(
                    [
                        raw[layout.label_to_index[f"{plane.id}.albedo.{channel}"]]
                        for channel in "rgb"
                    ]
                )
            )
        )
    intensities = [
        F.softplus(raw[layout.label_to_index[f"{light.id}.intensity"]])
        for light in scene.lights
    ]
    dtype = raw.dtype
    device = raw.device
    return RenderScene(
        sphere_ids=tuple(sphere.id for sphere in spheres),
        sphere_centers=(
            torch.stack(centers)
            if centers
            else torch.empty((0, 3), dtype=dtype, device=device)
        ),
        sphere_radii=(
            torch.stack(radii)
            if radii
            else torch.empty((0,), dtype=dtype, device=device)
        ),
        sphere_albedos=(
            torch.stack(sphere_albedos)
            if sphere_albedos
            else torch.empty((0, 3), dtype=dtype, device=device)
        ),
        plane_ids=tuple(plane.id for plane in planes),
        plane_points=torch.tensor(
            [plane.point for plane in planes], dtype=dtype, device=device
        ).reshape(len(planes), 3),
        plane_normals=torch.tensor(
            [plane.normal for plane in planes], dtype=dtype, device=device
        ).reshape(len(planes), 3),
        plane_albedos=(
            torch.stack(plane_albedos)
            if plane_albedos
            else torch.empty((0, 3), dtype=dtype, device=device)
        ),
        light_ids=tuple(light.id for light in scene.lights),
        light_centers=torch.tensor(
            [light.center for light in scene.lights], dtype=dtype, device=device
        ).reshape(len(scene.lights), 3),
        light_normals=torch.tensor(
            [light.normal for light in scene.lights], dtype=dtype, device=device
        ).reshape(len(scene.lights), 3),
        light_half_sizes=torch.tensor(
            [light.half_size for light in scene.lights], dtype=dtype, device=device
        ).reshape(len(scene.lights), 2),
        light_intensities=(
            torch.stack(intensities)
            if intensities
            else torch.empty((0,), dtype=dtype, device=device)
        ),
        background=torch.tensor(scene.background, dtype=dtype, device=device),
        ambient=scene.ambient,
        ray_epsilon=scene.ray_epsilon,
    )


def truth_values(
    scene: SceneSpec,
    layout: SceneParameterLayout,
    truth: dict[str, float],
) -> dict[str, float]:
    missing = [label for label in layout.labels if label not in truth]
    if missing:
        fail("E_SCHEMA", "analytic truth misses optimized parameters", "truth", missing=missing)
    return {label: truth[label] for label in layout.labels}


def parameter_index_map(layout: SceneParameterLayout) -> dict[str, int]:
    return dict(layout.label_to_index)


def labels_for_entries(
    layout: SceneParameterLayout,
    entries: Iterable[ParameterEntry],
) -> dict[int, str]:
    mapping: dict[int, str] = {}
    for entry in entries:
        for label in entry.label.split("|"):
            mapping[layout.label_to_index[label]] = label
    return mapping

