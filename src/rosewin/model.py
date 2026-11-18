"""Validation and parsed in-memory model for rose window scenes."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import SceneValidationError, ValidationIssue

SCHEMA = "cathedral-rose/v1"


@dataclass(frozen=True)
class Facet:
    stable_id: int
    quadrant: int
    facet: int
    vertices: np.ndarray
    triangles: np.ndarray
    transmission_rgb: np.ndarray
    transmittance: float
    glaze_reflectance: float
    lead_width: float
    lead_depth: float


@dataclass(frozen=True)
class SceneModel:
    raw: dict[str, Any]
    seed: int
    facets: tuple[Facet, ...]
    camera: dict[str, Any]
    directional_light: dict[str, Any]
    interior_light: dict[str, Any]
    glass: dict[str, Any]
    texture: dict[str, Any]
    render: dict[str, Any]
    projection: dict[str, Any]
    window_radius: float


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _vec3(value: Any) -> bool:
    return isinstance(value, list) and len(value) == 3 and all(_is_number(item) for item in value)


def validate_scene(scene: dict[str, Any]) -> list[ValidationIssue]:
    """Collect every structural issue; ordering is deterministic."""

    issues: list[ValidationIssue] = []
    if scene.get("schema") != SCHEMA:
        issues.append(ValidationIssue("E_SCHEMA", f"schema must be {SCHEMA}"))
    if not isinstance(scene.get("seed"), int):
        issues.append(ValidationIssue("E_SEED", "seed must be an integer"))

    for section in ("camera", "directional_light", "interior_light", "glass", "texture", "render", "projection"):
        if not isinstance(scene.get(section), dict):
            issues.append(ValidationIssue("E_SECTION", f"section {section} missing or not an object"))

    quadrants = scene.get("quadrants")
    if not isinstance(quadrants, list) or not quadrants:
        issues.append(ValidationIssue("E_QUADRANT_EMPTY", "quadrants must be a non-empty list"))
        return _sorted_issues(issues)

    seen_quadrants: set[int] = set()
    seen_facet_keys: set[tuple[int, int]] = set()
    for quadrant_block in quadrants:
        if not isinstance(quadrant_block, dict) or not isinstance(quadrant_block.get("quadrant"), int):
            issues.append(ValidationIssue("E_QUADRANT_INVALID", "quadrant block lacks integer id"))
            continue
        quadrant = quadrant_block["quadrant"]
        if quadrant in seen_quadrants:
            issues.append(
                ValidationIssue("E_QUADRANT_DUP", f"quadrant id {quadrant} appears more than once", quadrant)
            )
        seen_quadrants.add(quadrant)

        facets = quadrant_block.get("facets")
        if not isinstance(facets, list) or not facets:
            issues.append(ValidationIssue("E_FACET_EMPTY", "facets must be a non-empty list", quadrant))
            continue
        for facet_block in facets:
            facet_id = facet_block.get("facet") if isinstance(facet_block, dict) else None
            label = facet_id if isinstance(facet_id, int) else None
            if not isinstance(facet_id, int):
                issues.append(ValidationIssue("E_FACET_INVALID", "facet id must be an integer", quadrant, label))
                continue
            if (quadrant, facet_id) in seen_facet_keys:
                issues.append(
                    ValidationIssue(
                        "E_FACET_DUP",
                        f"facet id {facet_id} duplicated inside quadrant {quadrant}",
                        quadrant,
                        facet_id,
                    )
                )
            seen_facet_keys.add((quadrant, facet_id))
            issues.extend(_validate_facet(quadrant, facet_id, facet_block))

    return _sorted_issues(issues)


def _sorted_issues(issues: list[ValidationIssue]) -> list[ValidationIssue]:
    return sorted(
        issues,
        key=lambda item: (
            item.quadrant if item.quadrant is not None else -1,
            item.facet if item.facet is not None else -1,
            item.code,
            item.reason,
        ),
    )


def _validate_facet(quadrant: int, facet_id: int, block: dict[str, Any]) -> list[ValidationIssue]:
    issues: list[ValidationIssue] = []
    vertices = block.get("vertices")
    triangles = block.get("triangles")
    if not isinstance(vertices, list) or len(vertices) < 3:
        issues.append(ValidationIssue("E_FACET_INVALID", "vertices missing or shorter than 3", quadrant, facet_id))
        return issues
    if not all(isinstance(vertex, list) and len(vertex) == 3 and all(_is_number(v) for v in vertex) for vertex in vertices):
        issues.append(ValidationIssue("E_FACET_INVALID", "vertices must be numeric xyz triples", quadrant, facet_id))
        return issues
    if not isinstance(triangles, list) or not triangles:
        issues.append(ValidationIssue("E_FACET_INVALID", "triangles missing or empty", quadrant, facet_id))
        return issues

    vertex_count = len(vertices)
    for triangle in triangles:
        if not isinstance(triangle, list) or len(triangle) != 3 or not all(isinstance(i, int) for i in triangle):
            issues.append(ValidationIssue("E_FACET_INVALID", "triangle must be three integer indices", quadrant, facet_id))
            continue
        if any(index < 0 or index >= vertex_count for index in triangle):
            issues.append(
                ValidationIssue(
                    "E_INDEX_OUT_OF_RANGE",
                    f"triangle index outside [0,{vertex_count - 1}]: {triangle}",
                    quadrant,
                    facet_id,
                )
            )
            continue
        points = np.asarray([vertices[index] for index in triangle], dtype=np.float64)
        if float(np.linalg.norm(np.cross(points[1] - points[0], points[2] - points[0]))) <= 1e-12:
            issues.append(
                ValidationIssue("E_TRIANGLE_DEGENERATE", f"degenerate triangle {triangle}", quadrant, facet_id)
            )

    scalar_rules = (
        ("transmittance", "E_TRANSMITTANCE_RANGE", 0.0, 1.0),
        ("glaze_reflectance", "E_REFLECTANCE_RANGE", 0.0, 1.0),
        ("lead_width", "E_LEAD_WIDTH_RANGE", 0.0, 1.0),
        ("lead_depth", "E_LEAD_DEPTH_RANGE", 0.0, 1.0),
    )
    for field_name, code, low, high in scalar_rules:
        value = block.get(field_name)
        if not _is_number(value) or not (low <= float(value) <= high):
            issues.append(
                ValidationIssue(code, f"{field_name}={value!r} outside [{low},{high}]", quadrant, facet_id)
            )

    color = block.get("transmission_rgb")
    if not _vec3(color) or any(not (0.0 <= channel <= 1.0) for channel in color):
        issues.append(
            ValidationIssue("E_TRANSMISSION_COLOR_RANGE", "transmission_rgb must be three channels in [0,1]", quadrant, facet_id)
        )

    return issues


def parse_scene(scene: dict[str, Any]) -> SceneModel:
    """Validate then convert raw JSON into typed, sorted arrays."""

    issues = validate_scene(scene)
    if issues:
        raise SceneValidationError(issues)

    facets: list[Facet] = []
    stable_id = 0
    for block in sorted(scene["quadrants"], key=lambda item: item["quadrant"]):
        quadrant = block["quadrant"]
        for facet_block in sorted(block["facets"], key=lambda item: item["facet"]):
            stable_id += 1
            facets.append(
                Facet(
                    stable_id=stable_id,
                    quadrant=quadrant,
                    facet=facet_block["facet"],
                    vertices=np.asarray(facet_block["vertices"], dtype=np.float64),
                    triangles=np.asarray(facet_block["triangles"], dtype=np.int64),
                    transmission_rgb=np.asarray(facet_block["transmission_rgb"], dtype=np.float64),
                    transmittance=float(facet_block["transmittance"]),
                    glaze_reflectance=float(facet_block["glaze_reflectance"]),
                    lead_width=float(facet_block["lead_width"]),
                    lead_depth=float(facet_block["lead_depth"]),
                )
            )

    return SceneModel(
        raw=scene,
        seed=int(scene["seed"]),
        facets=tuple(facets),
        camera=scene["camera"],
        directional_light=scene["directional_light"],
        interior_light=scene["interior_light"],
        glass=scene["glass"],
        texture=scene["texture"],
        render=scene["render"],
        projection=scene["projection"],
        window_radius=float(scene["window"]["radius"]),
    )
