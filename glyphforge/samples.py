from __future__ import annotations

import copy
from dataclasses import replace
from pathlib import Path
from typing import Any

from .atlas import AtlasConfig
from .config import WorkbenchConfig
from .pipeline import run_pipeline
from .scheduler import BudgetConfig
from .util import atomic_write_json


def _point(x: float, y: float) -> dict[str, float]:
    return {"x": float(x), "y": float(y)}


def _line(x: float, y: float) -> dict[str, Any]:
    return {"type": "line", "to": _point(x, y)}


def _quadratic(cx: float, cy: float, x: float, y: float) -> dict[str, Any]:
    return {
        "type": "quadratic",
        "control": _point(cx, cy),
        "to": _point(x, y),
    }


def _cubic(
    c1x: float,
    c1y: float,
    c2x: float,
    c2y: float,
    x: float,
    y: float,
) -> dict[str, Any]:
    return {
        "type": "cubic",
        "control1": _point(c1x, c1y),
        "control2": _point(c2x, c2y),
        "to": _point(x, y),
    }


def _sample_segment_points(
    start: tuple[float, float],
    segments: list[dict[str, Any]],
) -> list[tuple[float, float]]:
    points = [start]
    current = start
    for segment in segments:
        target = (
            segment["to"]["x"],
            segment["to"]["y"],
        )
        if segment["type"] == "quadratic":
            control = (
                segment["control"]["x"],
                segment["control"]["y"],
            )
            for step in range(1, 17):
                t = step / 16.0
                x = (1 - t) ** 2 * current[0] + 2 * (1 - t) * t * control[0] + t * t * target[0]
                y = (1 - t) ** 2 * current[1] + 2 * (1 - t) * t * control[1] + t * t * target[1]
                points.append((x, y))
        elif segment["type"] == "cubic":
            c1 = (segment["control1"]["x"], segment["control1"]["y"])
            c2 = (segment["control2"]["x"], segment["control2"]["y"])
            for step in range(1, 17):
                t = step / 16.0
                x = (
                    (1 - t) ** 3 * current[0]
                    + 3 * (1 - t) ** 2 * t * c1[0]
                    + 3 * (1 - t) * t * t * c2[0]
                    + t ** 3 * target[0]
                )
                y = (
                    (1 - t) ** 3 * current[1]
                    + 3 * (1 - t) ** 2 * t * c1[1]
                    + 3 * (1 - t) * t * t * c2[1]
                    + t ** 3 * target[1]
                )
                points.append((x, y))
        else:
            points.append(target)
        current = target
    return points


def _contour(
    start: tuple[float, float],
    segments: list[dict[str, Any]],
) -> dict[str, Any]:
    samples = _sample_segment_points(start, segments)
    area = 0.0
    for first, second in zip(samples, samples[1:]):
        area += first[0] * second[1] - second[0] * first[1]
    direction = "ccw" if area > 0 else "cw"
    return {
        "start": _point(*start),
        "direction": direction,
        "segments": segments,
    }


def _glyph(
    *,
    advance: float,
    contours: list[dict[str, Any]] | None = None,
    unicode: list[int] | None = None,
    components: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    glyph: dict[str, Any] = {"advance": float(advance)}
    if contours:
        glyph["contours"] = contours
    if unicode:
        glyph["unicode"] = unicode
    if components:
        glyph["components"] = components
    return glyph


def _component(name: str, transform: list[float] | None = None) -> dict[str, Any]:
    value: dict[str, Any] = {"glyph": name}
    if transform is not None:
        value["transform"] = transform
    return value


def _circle_contour(
    center_x: float,
    center_y: float,
    radius: float,
    *,
    clockwise: bool,
    quadratic: bool = False,
) -> dict[str, Any]:
    points = [
        (center_x, center_y - radius),
        (center_x + radius, center_y),
        (center_x, center_y + radius),
        (center_x - radius, center_y),
    ]
    if clockwise:
        points = [
            points[0],
            points[3],
            points[2],
            points[1],
        ]
    start = points[0]
    segments: list[dict[str, Any]] = []
    if quadratic:
        control_scale = 1.2732395447351627
        for index in range(4):
            current = points[index]
            target = points[(index + 1) % 4]
            midpoint = (
                (current[0] + target[0]) * 0.5,
                (current[1] + target[1]) * 0.5,
            )
            vector_x = midpoint[0] - center_x
            vector_y = midpoint[1] - center_y
            control = (
                center_x + vector_x * control_scale,
                center_y + vector_y * control_scale,
            )
            segments.append(_quadratic(control[0], control[1], target[0], target[1]))
    else:
        for index in range(4):
            current = points[index]
            target = points[(index + 1) % 4]
            direction_x = 1.0 if target[0] > current[0] else (-1.0 if target[0] < current[0] else 0.0)
            direction_y = 1.0 if target[1] > current[1] else (-1.0 if target[1] < current[1] else 0.0)
            c1 = (
                current[0] + direction_x * radius * 0.5522847498,
                current[1] + direction_y * radius * 0.5522847498,
            )
            c2 = (
                target[0] - direction_x * radius * 0.5522847498,
                target[1] - direction_y * radius * 0.5522847498,
            )
            segments.append(_cubic(c1[0], c1[1], c2[0], c2[1], target[0], target[1]))
    return _contour(start, segments)


def _a_outer() -> dict[str, Any]:
    return _contour(
        (80, 0),
        [
            _line(520, 0),
            _line(450, 700),
            _line(150, 700),
            _line(80, 0),
        ],
    )


def _a_counter() -> dict[str, Any]:
    return _contour(
        (255, 160),
        [
            _line(300, 310),
            _line(345, 160),
            _line(255, 160),
        ],
    )


def _b_outer() -> dict[str, Any]:
    return _contour(
        (110, 0),
        [
            _line(490, 0),
            _cubic(570, 0, 570, 690, 490, 700),
            _line(120, 700),
            _cubic(40, 700, 40, 0, 110, 0),
        ],
    )


def _b_holes() -> list[dict[str, Any]]:
    return [
        _contour(
            (210, 430),
            [
                _line(210, 590),
                _line(440, 590),
                _line(440, 430),
                _line(210, 430),
            ],
        ),
        _contour(
            (210, 120),
            [
                _line(210, 300),
                _line(440, 300),
                _line(440, 120),
                _line(210, 120),
            ],
        ),
    ]


def _o_glyph(advance: float = 700) -> dict[str, Any]:
    return _glyph(
        advance=advance,
        contours=[
            _circle_contour(350, 350, 310, clockwise=False),
            _circle_contour(350, 350, 155, clockwise=True),
        ],
    )


def _eight_glyph() -> dict[str, Any]:
    return _glyph(
        advance=700,
        contours=[
            _circle_contour(350, 520, 180, clockwise=False),
            _circle_contour(350, 180, 200, clockwise=False),
            _circle_contour(350, 520, 85, clockwise=True, quadratic=True),
            _circle_contour(350, 180, 105, clockwise=True),
        ],
    )


def _e_glyph() -> dict[str, Any]:
    return _glyph(
        advance=620,
        unicode=[101],
        contours=[
            _circle_contour(310, 330, 270, clockwise=False),
            _contour(
                (160, 280),
                [
                    _line(175, 425),
                    _line(455, 425),
                    _line(470, 280),
                    _line(160, 280),
                ],
            ),
        ],
    )


def _acute_glyph() -> dict[str, Any]:
    return _glyph(
        advance=0,
        contours=[
            _contour(
                (330, 635),
                [
                    _quadratic(390, 760, 405, 850),
                    _line(455, 850),
                    _line(390, 635),
                    _line(330, 635),
                ],
            )
        ],
    )


def _macron_glyph() -> dict[str, Any]:
    return _glyph(
        advance=0,
        contours=[
            _contour(
                (220, 740),
                [
                    _line(480, 740),
                    _line(480, 800),
                    _line(220, 800),
                    _line(220, 740),
                ],
            )
        ],
    )


def _ring_glyph() -> dict[str, Any]:
    return _glyph(
        advance=0,
        contours=[
            _circle_contour(350, 790, 95, clockwise=False),
            _circle_contour(350, 790, 48, clockwise=True, quadratic=True),
        ],
    )


def _font(glyphs: dict[str, Any], unicode_map: dict[str, str]) -> dict[str, Any]:
    return {
        "format": "glyphforge.font/1",
        "units_per_em": 1000,
        "ascender": 900,
        "descender": -150,
        "glyphs": glyphs,
        "unicode_map": unicode_map,
    }


def build_font_specs() -> dict[str, dict[str, Any]]:
    a = _glyph(advance=600, unicode=[65, 97], contours=[_a_outer(), _a_counter()])
    b = _glyph(advance=640, unicode=[66, 98], contours=[_b_outer(), *_b_holes()])
    o = _o_glyph()
    o["unicode"] = [79, 111]
    e = _e_glyph()
    acute = _acute_glyph()
    macron = _macron_glyph()
    ring = _ring_glyph()
    aacute = _glyph(
        advance=600,
        unicode=[193],
        components=[_component("A"), _component("acute")],
    )
    amacron = _glyph(
        advance=600,
        unicode=[194],
        components=[_component("A"), _component("macron")],
    )
    aring = _glyph(
        advance=600,
        unicode=[197],
        components=[_component("A"), _component("ring")],
    )
    mixed = _font(
        {
            "A": a,
            "B": b,
            "O": o,
            "e": e,
            "acute": acute,
            "macron": macron,
            "ring": ring,
            "Aacute": aacute,
            "Amacron": amacron,
            "Aring": aring,
        },
        {
            "65": "A",
            "97": "A",
            "66": "B",
            "98": "B",
            "79": "O",
            "111": "O",
            "101": "e",
            "193": "Aacute",
            "194": "Amacron",
            "197": "Aring",
        },
    )
    holes = _font(
        {
            "eight": _eight_glyph(),
            "O": shift_unicode(_o_glyph(), [79, 111]),
            "B": b,
        },
        {"56": "eight", "79": "O", "111": "O", "66": "B", "98": "B"},
    )
    composites = _font(
        {
            "A": a,
            "Aacute": aacute,
            "Amacron": amacron,
            "Aring": aring,
            "acute": acute,
            "macron": macron,
            "ring": ring,
        },
        {
            "65": "A",
            "193": "Aacute",
            "194": "Amacron",
            "197": "Aring",
        },
    )
    return {
        "mixed.json": mixed,
        "holes.json": holes,
        "composites.json": composites,
    }


def shift_unicode(glyph: dict[str, Any], unicode: list[int]) -> dict[str, Any]:
    result = copy.deepcopy(glyph)
    result["unicode"] = unicode
    return result


def _case_configs() -> list[dict[str, Any]]:
    base_budget = BudgetConfig(
        parallelism=3,
        global_memory_bytes=8 * 1024 * 1024,
        max_task_samples=1_000_000,
        max_task_memory_bytes=1024 * 1024,
        sampling_batch_size=2048,
        min_sampling_batch_size=256,
    )
    atlas = AtlasConfig(width=512, height=256, padding=2)
    cases = [
        {
            "name": "mixed-24-nonzero-q255",
            "font": "mixed.json",
            "text": "AOB\u00c1",
            "pixel_size": 24.0,
            "subpixel": [0.0, 0.0],
            "quant_step": 1.0 / 255.0,
            "fill_rule": "nonzero",
            "tile_size": 5,
        },
        {
            "name": "mixed-32-evenodd-q128",
            "font": "mixed.json",
            "text": "AOB\u00c1",
            "pixel_size": 32.0,
            "subpixel": [0.5, 0.25],
            "quant_step": 1.0 / 128.0,
            "fill_rule": "evenodd",
            "tile_size": 7,
        },
        {
            "name": "holes-16-evenodd-q64",
            "font": "holes.json",
            "text": "8OB",
            "pixel_size": 16.0,
            "subpixel": [0.25, 0.0],
            "quant_step": 1.0 / 64.0,
            "fill_rule": "evenodd",
            "tile_size": 4,
        },
        {
            "name": "holes-28-nonzero-q255",
            "font": "holes.json",
            "text": "8OB",
            "pixel_size": 28.0,
            "subpixel": [0.0, 0.5],
            "quant_step": 1.0 / 255.0,
            "fill_rule": "nonzero",
            "tile_size": 6,
        },
        {
            "name": "composites-20-nonzero-q128",
            "font": "composites.json",
            "text": "A\u00c1\u00c2\u00c5",
            "pixel_size": 20.0,
            "subpixel": [0.5, 0.0],
            "quant_step": 1.0 / 128.0,
            "fill_rule": "nonzero",
            "tile_size": 5,
        },
        {
            "name": "composites-36-evenodd-q255",
            "font": "composites.json",
            "text": "A\u00c1\u00c2\u00c5",
            "pixel_size": 36.0,
            "subpixel": [0.25, 0.5],
            "quant_step": 1.0 / 255.0,
            "fill_rule": "evenodd",
            "tile_size": 8,
        },
    ]
    for case in cases:
        case["config"] = WorkbenchConfig(
            pixel_size=case["pixel_size"],
            subpixel_x=case["subpixel"][0],
            subpixel_y=case["subpixel"][1],
            supersample=4,
            quant_step=case["quant_step"],
            fill_rule=case["fill_rule"],
            tile_size=case["tile_size"],
            budget=replace(base_budget),
            atlas=replace(atlas),
        )
    return cases


def generate_samples(root: str | Path, *, seed: int = 20261121) -> dict[str, Any]:
    target = Path(root)
    fonts_dir = target / "fonts"
    configs_dir = target / "configs"
    runs_dir = target / "runs"
    fonts_dir.mkdir(parents=True, exist_ok=True)
    configs_dir.mkdir(parents=True, exist_ok=True)
    specs = build_font_specs()
    for filename, spec in specs.items():
        atomic_write_json(fonts_dir / filename, spec, pretty=True)

    from .parser import load_font

    cases = _case_configs()
    manifest_cases: list[dict[str, Any]] = []
    for case in cases:
        config: WorkbenchConfig = case["config"]
        config_path = configs_dir / f"{case['name']}.json"
        atomic_write_json(config_path, config.to_dict(), pretty=True)
        font_path = fonts_dir / case["font"]
        font = load_font(font_path, flatten_tolerance=config.curve_tolerance)
        result = run_pipeline(
            font,
            case["text"],
            config,
            output_dir=runs_dir / case["name"],
        )
        baselines = [
            {
                "glyph": glyph.name,
                "status": glyph.status,
                "coverage_hash": glyph.coverage_hash,
                "truth_hash": glyph.truth_hash,
                "tiles": glyph.tile_count,
                "quantization": glyph.summary.to_dict() if glyph.summary else None,
            }
            for glyph in result.glyphs
        ]
        manifest_cases.append(
            {
                "name": case["name"],
                "font": f"fonts/{case['font']}",
                "config": f"configs/{case['name']}.json",
                "text": case["text"],
                "pixel_size": case["pixel_size"],
                "subpixel_offset": case["subpixel"],
                "quant_step": case["quant_step"],
                "fill_rule": case["fill_rule"],
                "tile_size": case["tile_size"],
                "atlas_alpha_hash": result.atlas.alpha_hash,
                "trace": f"runs/{case['name']}/trace.jsonl",
                "baselines": baselines,
            }
        )
    manifest = {
        "format": "glyphforge.sample-manifest/1",
        "seed": seed,
        "generator": "GlyphForge deterministic local sample generator",
        "glyph_sets": list(specs),
        "cases": manifest_cases,
    }
    atomic_write_json(target / "manifest.json", manifest, pretty=True)
    return manifest
