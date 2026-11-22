"""Deterministic local fixtures with analytic shape-boundary truth."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .pngio import write_png
from .util import array_content_hash, atomic_write_json

GENERATOR_VERSION = "deterministic-samples-v1"
DEFAULT_SEED = 0x5EED2026


@dataclass(frozen=True)
class Circle:
    name: str
    cx: int
    cy: int
    radius: int
    fill: int
    outline: int


@dataclass(frozen=True)
class Rectangle:
    name: str
    x0: int
    y0: int
    x1: int
    y1: int
    fill: int
    outline: int


Shape = Circle | Rectangle


@dataclass(frozen=True)
class SampleSpec:
    name: str
    width: int
    height: int
    channels: int
    pattern: str
    shapes: tuple[Shape, ...]
    description: str


FULL_SPECS = (
    SampleSpec(
        "gradient_gray_512",
        512,
        384,
        1,
        "gradient",
        (Circle("circle_a", 158, 132, 72, 176, 24),),
        "smooth single-channel gradient with a circled boundary",
    ),
    SampleSpec(
        "texture_rgb_768",
        768,
        512,
        3,
        "texture",
        (Rectangle("rect_a", 92, 76, 492, 354, 68, 228),),
        "high-frequency RGB texture with a rectangular boundary",
    ),
    SampleSpec(
        "shapes_gray_1280",
        1280,
        720,
        1,
        "mixed",
        (
            Circle("circle_b", 268, 248, 126, 58, 236),
            Rectangle("rect_b", 520, 128, 1054, 510, 102, 242),
            Circle("circle_c", 1000, 536, 88, 198, 30),
        ),
        "mixed smooth/texture single-channel scene with known geometry",
    ),
    SampleSpec(
        "mixed_rgb_2048",
        2048,
        1152,
        3,
        "mixed",
        (
            Circle("circle_d", 420, 346, 188, 224, 24),
            Rectangle("rect_c", 746, 174, 1454, 632, 52, 244),
            Circle("circle_e", 1668, 760, 142, 112, 250),
            Rectangle("rect_d", 122, 814, 672, 1052, 184, 18),
        ),
        "thousands-scale RGB mixed media image with four analytic boundaries",
    ),
)

QUICK_SPECS = (
    SampleSpec(
        "quick_gradient_gray",
        192,
        128,
        1,
        "gradient",
        (Circle("circle_q0", 64, 58, 30, 172, 28),),
        "quick smooth grayscale fixture",
    ),
    SampleSpec(
        "quick_texture_rgb",
        260,
        180,
        3,
        "texture",
        (Rectangle("rect_q0", 38, 32, 178, 142, 56, 226),),
        "quick high-frequency RGB fixture",
    ),
    SampleSpec(
        "quick_mixed_rgb",
        320,
        224,
        3,
        "mixed",
        (
            Circle("circle_q1", 88, 92, 40, 228, 20),
            Rectangle("rect_q1", 174, 52, 286, 176, 66, 238),
        ),
        "quick mixed scene with circle and rectangle truth",
    ),
)


def _base_gradient(width: int, height: int, channels: int) -> np.ndarray:
    x = np.linspace(0.0, 1.0, width, dtype=np.float64)[None, :]
    y = np.linspace(0.0, 1.0, height, dtype=np.float64)[:, None]
    base = 28.0 + 152.0 * x + 58.0 * y
    if channels == 1:
        return base
    wave = 14.0 * np.sin(2.0 * np.pi * x) + 10.0 * np.cos(2.0 * np.pi * y)
    return np.stack((base, base * 0.83 + 24.0 + wave, base * 0.66 + 52.0 - wave), axis=2)


def _add_texture(image: np.ndarray, rng: np.random.Generator) -> np.ndarray:
    height, width = image.shape[:2]
    yy, xx = np.mgrid[0:height, 0:width]
    checker = (((xx // 5) + (yy // 5)) & 1).astype(np.float64) * 2.0 - 1.0
    diagonal = np.sin((xx * 0.71 + yy * 0.37) * np.pi / 4.0)
    noise = rng.integers(-16, 17, size=(height, width), dtype=np.int16).astype(np.float64)
    base_texture = 34.0 * checker + 18.0 * diagonal + noise
    if image.ndim == 3:
        texture = base_texture[..., None] * np.array([1.0, 0.87, 0.72]) + noise[..., None] * np.array([0.0, 0.18, -0.12])
    else:
        texture = base_texture
    return image + texture


def _draw_shapes(
    image: np.ndarray,
    shapes: Iterable[Shape],
) -> tuple[np.ndarray, dict[str, np.ndarray], list[dict[str, Any]]]:
    result = image.copy()
    height, width = result.shape[:2]
    edge_mask = np.zeros((height, width), dtype=bool)
    boundary_arrays: dict[str, np.ndarray] = {}
    descriptors: list[dict[str, Any]] = []
    yy, xx = np.mgrid[0:height, 0:width]

    for shape in shapes:
        if isinstance(shape, Circle):
            distance_sq = (xx - shape.cx) ** 2 + (yy - shape.cy) ** 2
            fill_mask = distance_sq <= shape.radius**2
            inner_radius = max(shape.radius - 1, 0)
            inner_mask = distance_sq <= inner_radius**2
            boundary_mask = fill_mask & ~inner_mask
            descriptor = {
                "name": shape.name,
                "type": "circle",
                "center": [shape.cx, shape.cy],
                "radius": shape.radius,
                "bbox": [
                    max(0, shape.cx - shape.radius),
                    max(0, shape.cy - shape.radius),
                    min(width - 1, shape.cx + shape.radius),
                    min(height - 1, shape.cy + shape.radius),
                ],
            }
        else:
            x0 = max(0, min(width - 1, shape.x0))
            x1 = max(0, min(width - 1, shape.x1))
            y0 = max(0, min(height - 1, shape.y0))
            y1 = max(0, min(height - 1, shape.y1))
            inside = (xx >= x0) & (xx <= x1) & (yy >= y0) & (yy <= y1)
            boundary_mask = inside & ((xx == x0) | (xx == x1) | (yy == y0) | (yy == y1))
            fill_mask = inside
            descriptor = {
                "name": shape.name,
                "type": "rectangle",
                "bbox": [x0, y0, x1, y1],
            }

        fill = np.asarray(shape.fill, dtype=np.float64)
        outline = np.asarray(shape.outline, dtype=np.float64)
        if result.ndim == 3:
            fill = np.broadcast_to(fill, (3,))
            outline = np.broadcast_to(outline, (3,))
        result[fill_mask] = fill
        result[boundary_mask] = outline
        edge_mask |= boundary_mask
        coordinates = np.argwhere(boundary_mask).astype(np.int32)
        boundary_arrays[shape.name] = coordinates
        descriptor["edge_pixel_count"] = int(coordinates.shape[0])
        descriptor["boundary_array"] = shape.name
        descriptors.append(descriptor)

    return result, boundary_arrays, descriptors


def _make_image(spec: SampleSpec, seed: int) -> tuple[np.ndarray, np.ndarray, dict[str, np.ndarray], list[dict[str, Any]]]:
    rng = np.random.default_rng(seed)
    image = _base_gradient(spec.width, spec.height, spec.channels)
    if spec.pattern == "texture":
        image = _add_texture(image, rng)
    elif spec.pattern == "mixed":
        split = spec.width // 2
        textured = _add_texture(image, rng)
        transition = np.clip((np.arange(spec.width) - split + 80) / 160.0, 0.0, 1.0)
        if image.ndim == 3:
            transition = transition[None, :, None]
        else:
            transition = transition[None, :]
        image = image * (1.0 - transition) + textured * transition
    image, boundary_arrays, descriptors = _draw_shapes(image, spec.shapes)
    pixels = np.clip(np.rint(image), 0, 255).astype(np.uint8)
    if spec.channels == 1 and pixels.ndim == 3:
        pixels = pixels[:, :, 0]
    edge_mask = np.zeros((spec.height, spec.width), dtype=bool)
    for coordinates in boundary_arrays.values():
        edge_mask[coordinates[:, 0], coordinates[:, 1]] = True
    return pixels, edge_mask, boundary_arrays, descriptors


def generate_samples(
    output_dir: str | Path,
    *,
    preset: str = "full",
    seed: int = DEFAULT_SEED,
) -> dict[str, Any]:
    """Generate PNG fixtures, raw baselines, analytic truth, and a manifest."""

    if preset not in {"full", "quick"}:
        from .errors import parameter_error

        raise parameter_error("preset", "preset must be 'full' or 'quick'", value=preset)
    specs = FULL_SPECS if preset == "full" else QUICK_SPECS
    root = Path(output_dir)
    root.mkdir(parents=True, exist_ok=True)
    entries = []

    for index, spec in enumerate(specs):
        image_seed = seed + index * 1009
        pixels, edge_mask, boundary_arrays, descriptors = _make_image(spec, image_seed)
        image_path = root / f"{spec.name}.png"
        baseline_path = root / f"{spec.name}.baseline.npy"
        truth_path = root / f"{spec.name}.truth.npz"
        write_png(image_path, pixels)
        np.save(baseline_path, pixels, allow_pickle=False)
        truth_payload = {"edge_mask": edge_mask}
        truth_payload.update({f"boundary_{name}": coords for name, coords in boundary_arrays.items()})
        np.savez_compressed(truth_path, **truth_payload)
        entries.append(
            {
                "name": spec.name,
                "description": spec.description,
                "width": spec.width,
                "height": spec.height,
                "channels": spec.channels,
                "dtype": "uint8",
                "seed": image_seed,
                "pattern": spec.pattern,
                "image_path": image_path.name,
                "baseline_path": baseline_path.name,
                "truth_path": truth_path.name,
                "content_hash": array_content_hash(pixels),
                "edge_pixel_count": int(edge_mask.sum()),
                "shapes": descriptors,
            }
        )

    manifest = {
        "generator_version": GENERATOR_VERSION,
        "seed": seed,
        "preset": preset,
        "sample_count": len(entries),
        "samples": entries,
    }
    atomic_write_json(root / "manifest.json", manifest)
    return manifest


def load_manifest(samples_dir: str | Path) -> dict[str, Any]:
    import json

    path = Path(samples_dir) / "manifest.json"
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        from .errors import WorkbenchError

        raise WorkbenchError("E_DECODE", "sample manifest is unavailable", {"path": str(path), "reason": str(exc)}) from exc
