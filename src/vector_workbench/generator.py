from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
from typing import Any, Iterable, Sequence

from .geometry import (
    path_area_scene,
    path_bbox_scene,
    signed_path_area_scene,
    truth_curve_samples,
)
from .model import (
    CubicSegment,
    Injection,
    Scene,
    Stage,
    Style,
    VectorPath,
    canonical_json_hash,
)

KAPPA = 0.5522847498307936
DEFAULT_SEED = 20261123


def _q(value: float, coord_scale: int) -> int:
    return int(round(value * coord_scale))


def _rounded_pair(value: Any, coord_scale: int) -> list[int]:
    return [_q(float(value[0]), coord_scale), _q(float(value[1]), coord_scale)]


def _line_segment(
    start: Sequence[float], end: Sequence[float], coord_scale: int
) -> dict[str, list[int]]:
    a = (float(start[0]), float(start[1]))
    b = (float(end[0]), float(end[1]))
    c1 = (a[0] + (b[0] - a[0]) / 3.0, a[1] + (b[1] - a[1]) / 3.0)
    c2 = (a[0] + 2.0 * (b[0] - a[0]) / 3.0, a[1] + 2.0 * (b[1] - a[1]) / 3.0)
    return {
        "start": _rounded_pair(a, coord_scale),
        "c1": _rounded_pair(c1, coord_scale),
        "c2": _rounded_pair(c2, coord_scale),
        "end": _rounded_pair(b, coord_scale),
    }


def _cubic_segment(
    start: Sequence[float],
    c1: Sequence[float],
    c2: Sequence[float],
    end: Sequence[float],
    coord_scale: int,
) -> dict[str, list[int]]:
    return {
        "start": _rounded_pair(start, coord_scale),
        "c1": _rounded_pair(c1, coord_scale),
        "c2": _rounded_pair(c2, coord_scale),
        "end": _rounded_pair(end, coord_scale),
    }


def _circle_segments(
    cx: float, cy: float, radius: float, coord_scale: int
) -> list[dict[str, list[int]]]:
    k = KAPPA * radius
    points = [
        ((cx + radius, cy), (cx + radius, cy + k), (cx + k, cy + radius), (cx, cy + radius)),
        ((cx, cy + radius), (cx - k, cy + radius), (cx - radius, cy + k), (cx - radius, cy)),
        ((cx - radius, cy), (cx - radius, cy - k), (cx - k, cy - radius), (cx, cy - radius)),
        ((cx, cy - radius), (cx + k, cy - radius), (cx + radius, cy - k), (cx + radius, cy)),
    ]
    return [_cubic_segment(*item, coord_scale) for item in points]


def _rounded_rect_segments(
    x: float, y: float, width: float, height: float, radius: float, coord_scale: int
) -> list[dict[str, list[int]]]:
    r = min(radius, width / 2.0, height / 2.0)
    k = KAPPA * r
    right = x + width
    bottom = y + height
    return [
        _line_segment((x + r, y), (right - r, y), coord_scale),
        _cubic_segment(
            (right - r, y), (right - r + k, y), (right, y + r - k), (right, y + r), coord_scale
        ),
        _line_segment((right, y + r), (right, bottom - r), coord_scale),
        _cubic_segment(
            (right, bottom - r),
            (right, bottom - r + k),
            (right - r + k, bottom),
            (right - r, bottom),
            coord_scale,
        ),
        _line_segment((right - r, bottom), (x + r, bottom), coord_scale),
        _cubic_segment(
            (x + r, bottom), (x + r - k, bottom), (x, bottom - r + k), (x, bottom - r), coord_scale
        ),
        _line_segment((x, bottom - r), (x, y + r), coord_scale),
        _cubic_segment(
            (x, y + r), (x, y + r - k), (x + r - k, y), (x + r, y), coord_scale
        ),
    ]


def _polygon_segments(
    points: Sequence[Sequence[float]], coord_scale: int
) -> list[dict[str, list[int]]]:
    return [
        _line_segment(points[i], points[(i + 1) % len(points)], coord_scale)
        for i in range(len(points))
    ]


def _star_points(
    cx: float, cy: float, outer: float, inner: float, *, pentagram: bool = False
) -> list[tuple[float, float]]:
    points: list[tuple[float, float]] = []
    if pentagram:
        order = [0, 2, 4, 1, 3]
        for index in order:
            angle = math.radians(-90.0 + 72.0 * index)
            points.append((cx + outer * math.cos(angle), cy + outer * math.sin(angle)))
        return points
    for index in range(10):
        radius = outer if index % 2 == 0 else inner
        angle = math.radians(-90.0 + 36.0 * index)
        points.append((cx + radius * math.cos(angle), cy + radius * math.sin(angle)))
    return points


def _style(
    *,
    fill: tuple[float, float, float] = (0.15, 0.75, 0.95),
    stroke: tuple[float, float, float] = (0.95, 0.94, 0.86),
    width: float = 0.0,
    opacity: float = 1.0,
    blend: str = "normal",
    join: str = "round",
) -> dict[str, Any]:
    return {
        "fill_color": list(fill),
        "stroke_color": list(stroke),
        "stroke_width": width,
        "opacity": opacity,
        "blend": blend,
        "join": join,
    }


def _path(
    path_id: str,
    name: str,
    kind: str,
    style: dict[str, Any],
    segments: Iterable[dict[str, list[int]]],
    injection: dict[str, Any] | None = None,
) -> dict[str, Any]:
    return {
        "id": path_id,
        "name": name,
        "kind": kind,
        "closed": True,
        "style": style,
        "segments": list(segments),
        "injection": injection,
    }


def _scene(
    scene_id: str,
    seed: int,
    width: int,
    height: int,
    paths: list[dict[str, Any]],
    *,
    coord_scale: int = 1024,
    category: str = "normal",
    expected_error: str | None = None,
) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "scene_id": scene_id,
        "seed": seed,
        "coord_scale": coord_scale,
        "coord_bounds": [0, 0, width * coord_scale, height * coord_scale],
        "render_bounds": [0.0, 0.0, float(width), float(height)],
        "category": category,
        "expected_error": expected_error,
        "paths": paths,
    }


def _base_hud_paths(coord_scale: int) -> list[dict[str, Any]]:
    return [
        _path(
            "orbit_fill",
            "Orbit fill",
            "fill",
            _style(fill=(0.08, 0.72, 0.96), opacity=0.68, blend="normal"),
            _circle_segments(145, 160, 98, coord_scale),
        ),
        _path(
            "orbit_glow",
            "Orbit glow",
            "fill",
            _style(fill=(0.96, 0.20, 0.67), opacity=0.55, blend="add"),
            _circle_segments(210, 145, 65, coord_scale),
        ),
        _path(
            "lower_plate",
            "Lower plate",
            "fill",
            _style(fill=(0.22, 0.96, 0.52), opacity=0.42, blend="multiply"),
            _rounded_rect_segments(48, 225, 200, 55, 18, coord_scale),
        ),
        _path(
            "target_ring",
            "Target ring",
            "stroke",
            _style(stroke=(1.0, 0.72, 0.16), width=12, opacity=0.95),
            _circle_segments(348, 160, 92, coord_scale),
        ),
        _path(
            "target_star",
            "Target star",
            "fill",
            _style(fill=(0.95, 0.24, 0.18), opacity=0.72, blend="multiply"),
            _polygon_segments(_star_points(348, 160, 45, 19), coord_scale),
        ),
        _path(
            "target_core",
            "Target core",
            "stroke",
            _style(stroke=(0.88, 0.96, 1.0), width=4, opacity=0.82, blend="add"),
            _circle_segments(348, 160, 36, coord_scale),
        ),
    ]


def _badge_paths(coord_scale: int) -> list[dict[str, Any]]:
    shield_points = [
        (120, 68),
        (360, 68),
        (358, 220),
        (240, 302),
        (122, 220),
    ]
    return [
        _path(
            "shield_fill",
            "Shield fill",
            "fill",
            _style(fill=(0.08, 0.34, 0.82), opacity=0.78),
            _polygon_segments(shield_points, coord_scale),
        ),
        _path(
            "shield_outline",
            "Shield outline",
            "stroke",
            _style(stroke=(0.20, 0.88, 0.96), width=10, opacity=0.9, blend="add"),
            _polygon_segments(shield_points, coord_scale),
        ),
        _path(
            "chevron",
            "Chevron",
            "stroke",
            _style(stroke=(1.0, 0.58, 0.12), width=16, opacity=0.94),
            _polygon_segments([(170, 142), (240, 208), (310, 142)], coord_scale),
        ),
        _path(
            "crest",
            "Crest",
            "fill",
            _style(fill=(1.0, 0.82, 0.18), opacity=0.68, blend="add"),
            _polygon_segments(_star_points(240, 126, 39, 17), coord_scale),
        ),
        _path(
            "badge_core",
            "Badge core",
            "fill",
            _style(fill=(0.02, 0.06, 0.16), opacity=0.52),
            _circle_segments(240, 126, 17, coord_scale),
        ),
    ]


def _media_paths(coord_scale: int) -> list[dict[str, Any]]:
    return [
        _path(
            "card_a",
            "Card A",
            "fill",
            _style(fill=(0.08, 0.12, 0.23), opacity=0.86),
            _rounded_rect_segments(30, 34, 226, 272, 26, coord_scale),
        ),
        _path(
            "card_a_edge",
            "Card A edge",
            "stroke",
            _style(stroke=(0.12, 0.82, 0.88), width=7, opacity=0.85),
            _rounded_rect_segments(30, 34, 226, 272, 26, coord_scale),
        ),
        _path(
            "card_b",
            "Card B",
            "fill",
            _style(fill=(0.36, 0.08, 0.40), opacity=0.78),
            _rounded_rect_segments(282, 34, 248, 272, 26, coord_scale),
        ),
        _path(
            "card_b_edge",
            "Card B edge",
            "stroke",
            _style(stroke=(0.96, 0.32, 0.66), width=7, opacity=0.9, blend="add"),
            _rounded_rect_segments(282, 34, 248, 272, 26, coord_scale),
        ),
        _path(
            "play_glow",
            "Play glow",
            "fill",
            _style(fill=(0.18, 0.92, 0.56), opacity=0.48, blend="add"),
            _circle_segments(160, 142, 54, coord_scale),
        ),
        _path(
            "play_triangle",
            "Play triangle",
            "fill",
            _style(fill=(0.98, 0.97, 0.88), opacity=0.9),
            _polygon_segments([(143, 112), (143, 172), (194, 142)], coord_scale),
        ),
        _path(
            "timeline",
            "Timeline",
            "fill",
            _style(fill=(0.14, 0.84, 0.94), opacity=0.7, blend="multiply"),
            _rounded_rect_segments(320, 246, 174, 13, 6, coord_scale),
        ),
    ]


def build_scenes(seed: int = DEFAULT_SEED) -> list[dict[str, Any]]:
    coord_scale = 1024
    scenes = [
        _scene("hud_orbit", seed, 512, 320, _base_hud_paths(coord_scale)),
        _scene("badge_combo", seed + 1, 480, 340, _badge_paths(coord_scale)),
        _scene("media_cards", seed + 2, 560, 340, _media_paths(coord_scale)),
    ]

    anomaly_base = _base_hud_paths(coord_scale)[0]
    scenes.extend(
        [
            _scene(
                "anomaly_self_intersection",
                seed + 3,
                512,
                320,
                [
                    anomaly_base,
                    _path(
                        "self_star",
                        "Pentagram contour",
                        "fill",
                        _style(fill=(0.98, 0.30, 0.18), opacity=0.78),
                        _polygon_segments(
                            _star_points(340, 158, 92, 38, pentagram=True), coord_scale
                        ),
                        {"kind": "self_intersection"},
                    ),
                ],
                category="anomaly",
                expected_error="E_SELF",
            ),
            _scene(
                "anomaly_degenerate",
                seed + 4,
                512,
                320,
                [
                    anomaly_base,
                    _path(
                        "zero_area_loop",
                        "Collinear zero-area loop",
                        "fill",
                        _style(fill=(0.98, 0.42, 0.18), opacity=0.7),
                        _polygon_segments(
                            [(260, 170), (410, 170), (320, 170)], coord_scale
                        ),
                        {"kind": "degenerate"},
                    ),
                ],
                category="anomaly",
                expected_error="E_DEGEN",
            ),
            _scene(
                "anomaly_stroke_clip",
                seed + 5,
                512,
                320,
                [
                    anomaly_base,
                    _path(
                        "oversize_stroke",
                        "Oversize stroke",
                        "stroke",
                        _style(stroke=(1.0, 0.18, 0.48), width=9000.0, opacity=0.9),
                        _circle_segments(350, 160, 72, coord_scale),
                        {"kind": "clip"},
                    ),
                ],
                category="anomaly",
                expected_error="E_CLIP",
            ),
            _scene(
                "anomaly_stage_failure",
                seed + 6,
                512,
                320,
                [
                    anomaly_base,
                    _path(
                        "injected_stage",
                        "Injected stage failure",
                        "fill",
                        _style(fill=(0.48, 0.18, 0.98), opacity=0.76),
                        _circle_segments(350, 160, 72, coord_scale),
                        {"kind": "stage_failure", "stage": "coverage"},
                    ),
                ],
                category="anomaly",
                expected_error="E_STAGE",
            ),
        ]
    )
    return scenes


def _parse_scene(raw: dict[str, Any], source_path: str | None = None) -> Scene:
    paths: list[VectorPath] = []
    coord_scale = int(raw["coord_scale"])
    for path_raw in raw["paths"]:
        injection_raw = path_raw.get("injection")
        injection = None
        if injection_raw:
            stage_raw = injection_raw.get("stage")
            injection = Injection(
                kind=str(injection_raw["kind"]),
                stage=Stage(stage_raw) if stage_raw else None,
            )
        style_raw = path_raw["style"]
        style = Style(
            fill_color=tuple(float(v) for v in style_raw["fill_color"]),
            stroke_color=tuple(float(v) for v in style_raw["stroke_color"]),
            stroke_width=float(style_raw["stroke_width"]),
            opacity=float(style_raw["opacity"]),
            blend=str(style_raw["blend"]),
            join=str(style_raw["join"]),
        )
        segments = tuple(
            CubicSegment(
                start=tuple(int(v) for v in item["start"]),
                control1=tuple(int(v) for v in item["c1"]),
                control2=tuple(int(v) for v in item["c2"]),
                end=tuple(int(v) for v in item["end"]),
            )
            for item in path_raw["segments"]
        )
        paths.append(
            VectorPath(
                id=str(path_raw["id"]),
                name=str(path_raw["name"]),
                kind=str(path_raw["kind"]),
                style=style,
                segments=segments,
                injection=injection,
            )
        )
    return Scene(
        scene_id=str(raw["scene_id"]),
        seed=int(raw["seed"]),
        coord_scale=coord_scale,
        coord_bounds=tuple(int(v) for v in raw["coord_bounds"]),
        render_bounds=tuple(float(v) for v in raw["render_bounds"]),
        paths=tuple(paths),
        raw=raw,
        source_path=source_path,
    )


def build_truth(scene_raw: dict[str, Any], scene_file: str) -> dict[str, Any]:
    scene = _parse_scene(scene_raw, scene_file)
    declarations: dict[str, Any] = {}
    for path in scene.paths:
        bbox = path_bbox_scene(path, scene.coord_scale)
        declarations[path.id] = {
            "kind": path.kind,
            "bbox": list(bbox),
            "signed_area": signed_path_area_scene(path, scene.coord_scale),
            "area": path_area_scene(path, scene.coord_scale),
            "style": {
                "fill_color": list(path.style.fill_color),
                "stroke_color": list(path.style.stroke_color),
                "stroke_width": path.style.stroke_width,
                "opacity": path.style.opacity,
                "blend": path.style.blend,
                "join": path.style.join,
            },
            "closure_checkpoints": [
                {
                    "segment": index,
                    "point": [
                        segment.end[0] / scene.coord_scale,
                        segment.end[1] / scene.coord_scale,
                    ],
                }
                for index, segment in enumerate(path.segments)
            ],
            "curve_samples": truth_curve_samples(
                scene, path, (0.0, 0.13, 0.25, 0.37, 0.5, 0.63, 0.75, 0.87, 1.0)
            ),
        }
    bounds = scene.render_bounds
    return {
        "scene_id": scene.scene_id,
        "scene_file": scene_file,
        "scene_sha256": canonical_json_hash(scene_raw),
        "coord_scale": scene.coord_scale,
        "scene_bbox": list(bounds),
        "expected_size_at_scale_1": [
            max(1, int(math.ceil(bounds[2] - bounds[0] - 1e-9))),
            max(1, int(math.ceil(bounds[3] - bounds[1] - 1e-9))),
        ],
        "paths": declarations,
    }


def _write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(value, indent=2, ensure_ascii=False, sort_keys=False) + "\n"
    path.write_text(text, encoding="utf-8")


def generate_samples(out_dir: str | Path, seed: int = DEFAULT_SEED) -> dict[str, Any]:
    out = Path(out_dir)
    scenes_dir = out / "scenes"
    truth_dir = out / "truth"
    scenes_dir.mkdir(parents=True, exist_ok=True)
    truth_dir.mkdir(parents=True, exist_ok=True)

    scenes = build_scenes(seed)
    truth_scenes: dict[str, Any] = {}
    sample_entries: list[dict[str, Any]] = []
    style_contract = {
        "opacity": [0.0, 1.0],
        "stroke_width": [0.0, 10000.0],
        "blend_modes": ["normal", "add", "multiply"],
        "join_modes": ["round", "bevel", "miter"],
        "path_kinds": ["fill", "stroke"],
        "coord_scale": 1024,
    }
    for scene_raw in scenes:
        scene_id = str(scene_raw["scene_id"])
        filename = f"{scene_id}.json"
        relative = f"scenes/{filename}"
        _write_json(scenes_dir / filename, scene_raw)
        truth_scenes[scene_id] = build_truth(scene_raw, relative)
        sample_entries.append(
            {
                "scene_id": scene_id,
                "scene_file": relative,
                "category": scene_raw["category"],
                "expected_error": scene_raw.get("expected_error"),
                "seed": scene_raw["seed"],
            }
        )
    truth_manifest = {
        "schema_version": 1,
        "seed": seed,
        "style_contract": style_contract,
        "validation_tolerances": {
            "area_relative": 0.01,
            "curve_distance_at_scale_1": 0.75,
            "min_length_monotonic_ratios": [2.0, 1.0, 0.5, 0.25, 0.125],
        },
        "scenes": truth_scenes,
    }
    _write_json(truth_dir / "truth_manifest.json", truth_manifest)
    sample_manifest = {
        "schema_version": 1,
        "seed": seed,
        "truth_manifest": "truth/truth_manifest.json",
        "scenes": sample_entries,
    }
    _write_json(out / "samples.json", sample_manifest)
    return sample_manifest


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate deterministic vector scenes")
    parser.add_argument("--out", default="data", help="output data directory")
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED)
    args = parser.parse_args(argv)
    manifest = generate_samples(args.out, args.seed)
    print(
        f"generated {len(manifest['scenes'])} scenes with seed {args.seed} in "
        f"{Path(args.out).resolve()}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

