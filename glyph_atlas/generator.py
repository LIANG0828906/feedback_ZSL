"""Seeded, offline sample font/config generator."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

from .pipeline import run_pipeline
from .snapshot import atomic_write_json, canonical_json_bytes, ensure_writable_directory


def generate_samples(root: str | Path, seed: int = 1947) -> dict[str, Any]:
    output_root = Path(root)
    ensure_writable_directory(output_root)
    phase = (seed % 8) / 8.0
    sets = [
        (
            "mixed_nonzero",
            _mixed_nonzero_font(seed),
            _mixed_config(phase),
        ),
        (
            "evenodd_composite",
            _evenodd_font(seed),
            _evenodd_config(1.0 - phase),
        ),
    ]
    summaries: list[dict[str, Any]] = []
    for name, font_document, config_document in sets:
        set_dir = output_root / name
        ensure_writable_directory(set_dir)
        font_path = set_dir / "font.json"
        config_path = set_dir / "config.json"
        atomic_write_json(font_path, font_document)
        atomic_write_json(config_path, config_document)
        report = run_pipeline(config_path)
        baseline_bytes = report.baseline_path.read_bytes()
        summaries.append(
            {
                "name": name,
                "font": "font.json",
                "config": "config.json",
                "font_sha256": report.font.canonical_hash(),
                "config_sha256": report.baseline["config_sha256"],
                "baseline": f"{name}/output/baseline.json",
                "baseline_sha256": hashlib.sha256(baseline_bytes).hexdigest(),
                "targets": [
                    {
                        "name": run.schedule.target.name,
                        "atlas_sha256": run.atlas.atlas_sha256,
                        "metrics_sha256": run.atlas.metrics_sha256,
                        "source_coverage_sha256": run.atlas.source_coverage_sha256,
                        "trace_sha256": run.trace_sha256,
                    }
                    for run in report.targets
                ],
            }
        )
    summary = {
        "format": "glyph-atlas-sample-summary/1",
        "seed": seed,
        "sets": summaries,
    }
    summary_path = output_root / "summary.json"
    atomic_write_json(summary_path, summary)
    return summary


def _mixed_config(phase: float) -> dict[str, Any]:
    return {
        "format": "glyph-atlas-config/1",
        "font": "font.json",
        "glyphs": ["space", "O", "A", "Aring", "Oacute"],
        "targets": [
            {
                "name": "ui18",
                "pixel_size": 18,
                "subpixel_x": round(phase * 0.5, 6),
                "subpixel_y": round((1.0 - phase) * 0.25, 6),
            },
            {
                "name": "ui32",
                "pixel_size": 32,
                "subpixel_x": 0.25,
                "subpixel_y": 0.5,
            },
        ],
        "tile": {"width": 10, "height": 10},
        "parallelism": 2,
        "samples": {"x": 4, "y": 4},
        "quantization_step": 1,
        "fill_rule": "nonzero",
        "budget": {
            "max_task_sampling_points": 2000000,
            "max_task_memory_bytes": 67108864,
            "max_total_sampling_points": 20000000,
            "max_total_memory_bytes": 536870912,
        },
        "atlas": {"width": 512, "height": 256, "padding": 2},
        "coverage_tolerance": 0,
        "output_dir": "output",
        "snapshot_dir": "snapshots",
        "temp_dir": ".tmp",
    }


def _evenodd_config(phase: float) -> dict[str, Any]:
    return {
        "format": "glyph-atlas-config/1",
        "font": "font.json",
        "glyphs": ["space", "B", "Bdieresis", "H"],
        "targets": [
            {
                "name": "menu24",
                "pixel_size": 24,
                "subpixel_x": round(phase * 0.5, 6),
                "subpixel_y": 0.0,
            },
            {
                "name": "hud40",
                "pixel_size": 40,
                "subpixel_x": 0.125,
                "subpixel_y": 0.375,
            },
        ],
        "tile": {"width": 11, "height": 9},
        "parallelism": 2,
        "samples": {"x": 4, "y": 4},
        "quantization_step": 1,
        "fill_rule": "evenodd",
        "budget": {
            "max_task_sampling_points": 2000000,
            "max_task_memory_bytes": 67108864,
            "max_total_sampling_points": 20000000,
            "max_total_memory_bytes": 536870912,
        },
        "atlas": {"width": 512, "height": 256, "padding": 2},
        "coverage_tolerance": 0,
        "output_dir": "output",
        "snapshot_dir": "snapshots",
        "temp_dir": ".tmp",
    }


def _mixed_nonzero_font(seed: int) -> dict[str, Any]:
    outer_shift = float(seed % 3)
    return {
        "format": "glyph-font/1",
        "name": "mixed-nonzero-local",
        "units_per_em": 1000,
        "default_fill_rule": "nonzero",
        "curve_tolerance": 0.5,
        "glyphs": [
            _blank("space", 32, 320),
            _round_o(outer_shift),
            _triangle_a(),
            _ring(),
            _acute(),
            _composite("Aring", 197, 700, [("A", 1, 0, 0), ("ring", 0.42, 350, 725)]),
            _composite("Oacute", 211, 700, [("O", 1, 0, 0), ("acute", 1, 0, 0)]),
        ],
    }


def _evenodd_font(seed: int) -> dict[str, Any]:
    shift = float(seed % 2)
    return {
        "format": "glyph-font/1",
        "name": "evenodd-composite-local",
        "units_per_em": 1000,
        "default_fill_rule": "evenodd",
        "curve_tolerance": 0.5,
        "glyphs": [
            _blank("space", 32, 320),
            _rounded_b(shift),
            _dot(),
            _composite("Bdieresis", 196, 700, [("B", 1, 0, 0), ("dot", 1, 240, 745), ("dot", 1, 460, 745)]),
            _h_glyph(),
        ],
    }


def _blank(name: str, codepoint: int, advance: int) -> dict[str, Any]:
    return {
        "name": name,
        "unicode": codepoint,
        "advance_width": advance,
        "blank": True,
        "contours": [],
        "components": [],
    }


def _round_o(shift: float) -> dict[str, Any]:
    outer = {
        "direction": "counterclockwise",
        "points": [
            [600, 350],
            [600, 543 + shift],
            [543, 700],
            [350, 700],
            [157, 700],
            [100, 543 + shift],
            [100, 350],
            [100, 157 + shift],
            [157, 0],
            [350, 0],
            [543, 0],
            [600, 157 + shift],
        ],
        "segments": [
            {"type": "cubic", "control1": 1, "control2": 2, "to": 3},
            {"type": "cubic", "control1": 4, "control2": 5, "to": 6},
            {"type": "cubic", "control1": 7, "control2": 8, "to": 9},
            {"type": "cubic", "control1": 10, "control2": 11, "to": 0},
        ],
    }
    inner = {
        "direction": "clockwise",
        "points": [
            [520, 350],
            [520, 255 - shift],
            [350, 180],
            [180, 255 - shift],
            [180, 350],
            [180, 445 + shift],
            [350, 520],
            [520, 445 + shift],
        ],
        "segments": [
            {"type": "quadratic", "control": 1, "to": 2},
            {"type": "quadratic", "control": 3, "to": 4},
            {"type": "quadratic", "control": 5, "to": 6},
            {"type": "quadratic", "control": 7, "to": 0},
        ],
    }
    return {
        "name": "O",
        "unicode": 79,
        "advance_width": 700,
        "fill_rule": "nonzero",
        "contours": [outer, inner],
    }


def _triangle_a() -> dict[str, Any]:
    return {
        "name": "A",
        "unicode": 65,
        "advance_width": 700,
        "fill_rule": "nonzero",
        "contours": [
            {
                "direction": "counterclockwise",
                "points": [[100, 0], [600, 0], [350, 700]],
                "segments": [
                    {"type": "line", "to": 1},
                    {"type": "quadratic", "control": 1, "to": 2},
                    {"type": "line", "to": 0},
                ],
            },
            {
                "direction": "clockwise",
                "points": [[220, 140], [350, 480], [480, 140]],
                "segments": [
                    {"type": "line", "to": 1},
                    {"type": "line", "to": 2},
                    {"type": "line", "to": 0},
                ],
            },
        ],
    }


def _ring() -> dict[str, Any]:
    return {
        "name": "ring",
        "advance_width": 600,
        "contours": [
            {
                "direction": "counterclockwise",
                "points": [
                    [100, 0],
                    [100, 80],
                    [0, 100],
                    [-100, 80],
                    [-100, 0],
                    [-100, -80],
                    [0, -100],
                    [100, -80],
                ],
                "segments": [
                    {"type": "quadratic", "control": 1, "to": 2},
                    {"type": "quadratic", "control": 3, "to": 4},
                    {"type": "quadratic", "control": 5, "to": 6},
                    {"type": "quadratic", "control": 7, "to": 0},
                ],
            },
            {
                "direction": "clockwise",
                "points": [
                    [48, 0],
                    [48, -28],
                    [0, -48],
                    [-48, -28],
                    [-48, 0],
                    [-48, 28],
                    [0, 48],
                    [48, 28],
                ],
                "segments": [
                    {"type": "quadratic", "control": 1, "to": 2},
                    {"type": "quadratic", "control": 3, "to": 4},
                    {"type": "quadratic", "control": 5, "to": 6},
                    {"type": "quadratic", "control": 7, "to": 0},
                ],
            },
        ],
    }


def _acute() -> dict[str, Any]:
    return {
        "name": "acute",
        "advance_width": 300,
        "contours": [
            {
                "direction": "counterclockwise",
                "points": [[300, 620], [360, 660], [430, 870], [370, 910]],
                "segments": [
                    {"type": "line", "to": 1},
                    {"type": "cubic", "control1": 1, "control2": 2, "to": 3},
                    {"type": "line", "to": 0},
                ],
            }
        ],
    }


def _dot() -> dict[str, Any]:
    return {
        "name": "dot",
        "advance_width": 180,
        "contours": [
            {
                "direction": "counterclockwise",
                "points": [[40, 0], [80, 40], [40, 80], [0, 40]],
                "segments": [
                    {"type": "line", "to": 1},
                    {"type": "line", "to": 2},
                    {"type": "line", "to": 3},
                    {"type": "line", "to": 0},
                ],
            }
        ],
    }


def _rounded_b(shift: float) -> dict[str, Any]:
    outer = {
        "direction": "counterclockwise",
        "points": [
            [50, 0],
            [620, 0],
            [620, 700],
            [50, 700],
        ],
        "segments": [
            {"type": "line", "to": 1},
            {"type": "line", "to": 2},
            {"type": "line", "to": 3},
            {"type": "line", "to": 0},
        ],
    }
    # The two counters intentionally have the same orientation as the outer
    # contour. They become holes only under the even-odd rule.
    upper = {
        "direction": "counterclockwise",
        "points": [
            [170, 380 + shift],
            [420, 380 + shift],
            [420, 585 + shift],
            [170, 585 + shift],
        ],
        "segments": [
            {"type": "line", "to": 1},
            {"type": "line", "to": 2},
            {"type": "line", "to": 3},
            {"type": "line", "to": 0},
        ],
    }
    lower = {
        "direction": "counterclockwise",
        "points": [
            [260, 110 + shift],
            [500, 110 + shift],
            [500, 315 + shift],
            [260, 315 + shift],
        ],
        "segments": [
            {"type": "line", "to": 1},
            {"type": "line", "to": 2},
            {"type": "line", "to": 3},
            {"type": "line", "to": 0},
        ],
    }
    return {
        "name": "B",
        "unicode": 66,
        "advance_width": 700,
        "fill_rule": "evenodd",
        "contours": [outer, upper, lower],
    }


def _h_glyph() -> dict[str, Any]:
    return {
        "name": "H",
        "unicode": 72,
        "advance_width": 700,
        "fill_rule": "evenodd",
        "contours": [
            {
                "direction": "counterclockwise",
                "points": [
                    [100, 0],
                    [220, 0],
                    [220, 280],
                    [480, 280],
                    [480, 0],
                    [600, 0],
                    [600, 700],
                    [480, 700],
                    [480, 420],
                    [220, 420],
                    [220, 700],
                    [100, 700],
                ],
                "segments": [
                    {"type": "line", "to": 1},
                    {"type": "line", "to": 2},
                    {"type": "line", "to": 3},
                    {"type": "line", "to": 4},
                    {"type": "line", "to": 5},
                    {"type": "line", "to": 6},
                    {"type": "line", "to": 7},
                    {"type": "line", "to": 8},
                    {"type": "line", "to": 9},
                    {"type": "line", "to": 10},
                    {"type": "line", "to": 11},
                    {"type": "line", "to": 0},
                ],
            }
        ],
    }


def _composite(
    name: str,
    codepoint: int,
    advance: int,
    components: list[tuple[str, float, float, float]],
) -> dict[str, Any]:
    return {
        "name": name,
        "unicode": codepoint,
        "advance_width": advance,
        "components": [
            {
                "glyph": glyph,
                "transform": {
                    "a": scale,
                    "b": 0,
                    "c": 0,
                    "d": scale,
                    "e": dx,
                    "f": dy,
                },
            }
            for glyph, scale, dx, dy in components
        ],
    }

