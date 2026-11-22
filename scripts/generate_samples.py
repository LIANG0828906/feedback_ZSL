#!/usr/bin/env python3
"""Generate deterministic multi-version scenes and parsing truth."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import random
from pathlib import Path
from typing import Any

DEFAULT_SEED = 20261123
FRAME_COUNT = 18
WINDOW_SIZE = 3
WIDTH = 40
HEIGHT = 24
FPS_NUM = 24
FPS_DEN = 1
TICKS_PER_SECOND = 60000
TICKS_PER_FRAME = 2500


def _entry(source_path: str, internal_path: str, transform: str) -> dict[str, str]:
    return {
        "source_path": source_path,
        "internal_path": internal_path,
        "transform": transform,
    }


def _srgb_to_linear(value: float) -> float:
    if value <= 0.04045:
        return value / 12.92
    return ((value + 0.055) / 1.055) ** 2.4


def _linear_to_srgb(value: float) -> float:
    value = min(1.0, max(0.0, value))
    if value <= 0.0031308:
        return value * 12.92
    return 1.055 * (value ** (1.0 / 2.4)) - 0.055


def _quantize(value: float) -> int:
    return int(math.floor(min(1.0, max(0.0, value)) * 255.0 + 0.5))


def _encode_rgb(
    linear: tuple[float, float, float],
    alpha: float,
    transfer: str,
    alpha_mode: str,
) -> list[float]:
    stored = (
        tuple(channel * alpha for channel in linear)
        if alpha_mode == "premultiplied"
        else linear
    )
    if transfer == "linear":
        return [float(channel) for channel in stored]
    return [float(_linear_to_srgb(channel)) for channel in stored]


def _make_layer_specs(seed: int) -> list[dict[str, Any]]:
    rng = random.Random(seed)
    colors = []
    for low, high in (
        (18, 46),
        (52, 92),
        (86, 132),
        (142, 205),
        (98, 174),
    ):
        colors.append(
            (
                rng.randrange(low, high + 1),
                rng.randrange(low, high + 1),
                rng.randrange(low, high + 1),
            )
        )
    return [
        {
            "layer_id": "bg_plate",
            "kind": "background",
            "name": "Stage Background Plate",
            "parent_id": None,
            "stack_order": 0,
            "visible_from": 0,
            "visible_to": 18,
            "opacity": 1.0,
            "alpha": 1.0,
            "srgb8": colors[0],
            "bounds": (0, 0, 40, 24),
        },
        {
            "layer_id": "prop_crate",
            "kind": "prop",
            "name": "Foreground Crate",
            "parent_id": None,
            "stack_order": 1,
            "visible_from": 0,
            "visible_to": 9,
            "opacity": 0.92,
            "alpha": 0.84,
            "srgb8": colors[1],
            "bounds": (3, 6, 13, 11),
        },
        {
            "layer_id": "prop_lamp",
            "kind": "prop",
            "name": "Practical Lamp",
            "parent_id": None,
            "stack_order": 2,
            "visible_from": 5,
            "visible_to": 15,
            "opacity": 1.0,
            "alpha": 0.72,
            "srgb8": colors[2],
            "bounds": (17, 3, 8, 15),
        },
        {
            "layer_id": "character",
            "kind": "character",
            "name": "Hero Character",
            "parent_id": None,
            "stack_order": 3,
            "visible_from": 2,
            "visible_to": 18,
            "opacity": 1.0,
            "alpha": 0.94,
            "srgb8": colors[3],
            "bounds": (24, 4, 12, 17),
        },
        {
            "layer_id": "character_glow",
            "kind": "character",
            "name": "Character Edge Glow",
            "parent_id": "character",
            "stack_order": 4,
            "visible_from": 2,
            "visible_to": 12,
            "opacity": 0.85,
            "alpha": 0.46,
            "srgb8": colors[4],
            "bounds": (22, 2, 17, 21),
        },
    ]


def _linear_color(spec: dict[str, Any]) -> tuple[float, float, float]:
    return tuple(_srgb_to_linear(channel / 255.0) for channel in spec["srgb8"])


def _bounds_object(spec: dict[str, Any]) -> dict[str, int]:
    x, y, width, height = spec["bounds"]
    return {"x": x, "y": y, "w": width, "h": height}


def _v1_document(seed: int, specs: list[dict[str, Any]]) -> dict[str, Any]:
    layers = []
    for spec in specs:
        linear = _linear_color(spec)
        encoded = _encode_rgb(linear, spec["alpha"], "srgb", "premultiplied")
        layers.append(
            {
                "id": spec["layer_id"],
                "type": spec["kind"],
                "name": spec["name"],
                "order": spec["stack_order"],
                "visible": True,
                "visible_from": spec["visible_from"],
                "visible_until": spec["visible_to"],
                "bounds": _bounds_object(spec),
                "rgba_srgb_premultiplied": [*encoded, spec["alpha"]],
                "opacity": spec["opacity"],
            }
        )
    return {
        "format": {
            "name": "virtual-production-composite",
            "version": "vpfc/1",
        },
        "sceneId": f"vp_stage_{seed}",
        "canvas": {"width": WIDTH, "height": HEIGHT, "channels": 4},
        "timeline": {
            "frame_count": FRAME_COUNT,
            "window_size": WINDOW_SIZE,
            "time_base": {
                "unit": "frames",
                "fps": {"num": FPS_NUM, "den": FPS_DEN},
            },
        },
        "color_space": "srgb",
        "alpha_mode": "premultiplied",
        "layers": layers,
    }


def _v2_document(seed: int, specs: list[dict[str, Any]]) -> dict[str, Any]:
    modes = {
        "bg_plate": ("srgb", "straight"),
        "prop_crate": ("srgb", "straight"),
        "prop_lamp": ("linear", "premultiplied"),
        "character": ("srgb", "premultiplied"),
        "character_glow": ("linear", "straight"),
    }
    layers = []
    for spec in specs:
        transfer, alpha_mode = modes[spec["layer_id"]]
        linear = _linear_color(spec)
        encoded = _encode_rgb(linear, spec["alpha"], transfer, alpha_mode)
        x, y, width, height = spec["bounds"]
        layers.append(
            {
                "layerId": spec["layer_id"],
                "layerType": spec["kind"],
                "displayName": spec["name"],
                "zIndex": spec["stack_order"],
                "parentId": spec["parent_id"],
                "visibility": {
                    "startTick": spec["visible_from"] * TICKS_PER_FRAME,
                    "endTickExclusive": spec["visible_to"] * TICKS_PER_FRAME,
                },
                "bounds": {
                    "left": x,
                    "top": y,
                    "width": width,
                    "height": height,
                },
                "rgba": {
                    "transfer": transfer,
                    "alphaMode": alpha_mode,
                    "values": [*encoded, spec["alpha"]],
                },
                "opacity": spec["opacity"],
            }
        )
    return {
        "schema": {"vendor": "VP Stage Tools", "revision": "vpfc/2"},
        "sceneId": f"vp_stage_{seed}",
        "canvasWidth": WIDTH,
        "canvasHeight": HEIGHT,
        "channelCount": 4,
        "frameCount": FRAME_COUNT,
        "windowSize": WINDOW_SIZE,
        "frameRate": {"num": FPS_NUM, "den": FPS_DEN},
        "timeBase": {
            "unit": "ticks",
            "ticksPerSecond": TICKS_PER_SECOND,
            "ticksPerFrame": TICKS_PER_FRAME,
        },
        "layers": layers,
    }


def _v3_document(seed: int, specs: list[dict[str, Any]]) -> dict[str, Any]:
    modes = {
        "bg_plate": ("linear", "straight"),
        "prop_crate": ("linear", "premultiplied"),
        "prop_lamp": ("linear", "straight"),
        "character": ("linear", "premultiplied"),
        "character_glow": ("linear", "straight"),
    }
    by_parent: dict[str | None, list[dict[str, Any]]] = {}
    for spec in specs:
        by_parent.setdefault(spec["parent_id"], []).append(spec)

    def build(spec: dict[str, Any]) -> dict[str, Any]:
        transfer, alpha_mode = modes[spec["layer_id"]]
        linear = _linear_color(spec)
        encoded = _encode_rgb(linear, spec["alpha"], transfer, alpha_mode)
        node: dict[str, Any] = {
            "id": spec["layer_id"],
            "kind": spec["kind"],
            "name": spec["name"],
            "stackIndex": spec["stack_order"],
            "visibility": {
                "fromFrame": spec["visible_from"],
                "toFrameExclusive": spec["visible_to"],
            },
            "bounds": list(spec["bounds"]),
            "alpha": {"mode": alpha_mode},
            "color": {"space": transfer, "rgba": [*encoded, spec["alpha"]]},
            "opacity": spec["opacity"],
        }
        children = by_parent.get(spec["layer_id"], [])
        if children:
            node["children"] = [build(child) for child in children]
        return node

    roots = [build(spec) for spec in by_parent.get(None, [])]
    return {
        "document": {"kind": "vp-foreground-composite", "revision": "vpfc/3"},
        "sceneId": f"vp_stage_{seed}",
        "canvas": {"width": WIDTH, "height": HEIGHT, "channels": 4},
        "temporal": {
            "frameCount": FRAME_COUNT,
            "windowSize": WINDOW_SIZE,
            "framePeriodSeconds": {"num": 1, "den": 24},
        },
        "layers": roots,
    }


def _v1_mapping() -> list[dict[str, str]]:
    trace = [
        _entry("format.version", "source_version", "version_token_to_canonical"),
        _entry("canvas.width", "canvas.width", "identity"),
        _entry("canvas.height", "canvas.height", "identity"),
        _entry("canvas.channels", "canvas.channels", "identity"),
        _entry("timeline.frame_count", "timeline.frames", "identity"),
        _entry("timeline.window_size", "timeline.window_size", "identity"),
        _entry("timeline.time_base.fps.num", "timeline.fps_num", "identity"),
        _entry("timeline.time_base.fps.den", "timeline.fps_den", "identity"),
        _entry(
            "timeline.time_base.unit", "timeline.source_time_unit", "identity"
        ),
        _entry("sceneId", "scene_id", "identity"),
        _entry("alpha_mode", "layers[*].alpha_mode", "broadcast"),
        _entry(
            "color_space", "layers[*].source_color_space", "broadcast"
        ),
    ]
    names = [
        ("id", "layer_id", "identity"),
        ("type", "kind", "identity"),
        ("name", "name", "identity"),
        ("order", "stack_order", "identity"),
        ("visible", "visible", "identity"),
        ("visible_from", "visible_from", "identity"),
        ("visible_until", "visible_to", "exclusive_until_to_to"),
        ("bounds", "bounds", "object_to_rect"),
        (
            "rgba_srgb_premultiplied",
            "color_linear",
            "srgb_premultiplied_to_linear_straight",
        ),
        (
            "rgba_srgb_premultiplied[3]",
            "alpha",
            "channel_index_3_to_alpha",
        ),
        ("opacity", "opacity", "identity"),
    ]
    for index in range(5):
        for source, internal, transform in names:
            trace.append(
                _entry(
                    f"layers[{index}].{source}",
                    f"layers[{index}].{internal}",
                    transform,
                )
            )
    return trace


def _v2_mapping() -> list[dict[str, str]]:
    trace = [
        _entry("schema.revision", "source_version", "version_token_to_canonical"),
        _entry("canvasWidth", "canvas.width", "identity"),
        _entry("canvasHeight", "canvas.height", "identity"),
        _entry("channelCount", "canvas.channels", "identity"),
        _entry("frameCount", "timeline.frames", "identity"),
        _entry("windowSize", "timeline.window_size", "identity"),
        _entry("frameRate.num", "timeline.fps_num", "identity"),
        _entry("frameRate.den", "timeline.fps_den", "identity"),
        _entry("timeBase.unit", "timeline.source_time_unit", "identity"),
        _entry(
            "timeBase.ticksPerFrame",
            "timeline.source_time_basis.ticksPerFrame",
            "identity",
        ),
        _entry(
            "timeBase.ticksPerSecond",
            "timeline.source_time_basis.ticksPerSecond",
            "identity",
        ),
        _entry("sceneId", "scene_id", "identity"),
    ]
    names = [
        ("layerId", "layer_id", "identity"),
        ("layerType", "kind", "identity"),
        ("displayName", "name", "identity"),
        ("zIndex", "stack_order", "identity"),
        ("parentId", "parent_id", "identity"),
        (
            "visibility.startTick",
            "visible_from",
            "tick_to_frame",
        ),
        (
            "visibility.endTickExclusive",
            "visible_to",
            "tick_to_frame_exclusive",
        ),
        ("bounds", "bounds", "left_top_width_height_to_rect"),
        ("rgba.transfer", "source_color_space", "identity"),
        ("rgba.alphaMode", "alpha_mode", "identity"),
        (
            "rgba.values",
            "color_linear",
            "declared_transfer_and_alpha_to_linear_straight",
        ),
        ("rgba.values[3]", "alpha", "channel_index_3_to_alpha"),
        ("opacity", "opacity", "identity"),
    ]
    for index in range(5):
        for source, internal, transform in names:
            trace.append(
                _entry(
                    f"layers[{index}].{source}",
                    f"layers[{index}].{internal}",
                    transform,
                )
            )
    return trace


def _v3_mapping() -> list[dict[str, str]]:
    trace = [
        _entry("document.revision", "source_version", "version_token_to_canonical"),
        _entry("canvas.width", "canvas.width", "identity"),
        _entry("canvas.height", "canvas.height", "identity"),
        _entry("canvas.channels", "canvas.channels", "identity"),
        _entry("temporal.frameCount", "timeline.frames", "identity"),
        _entry("temporal.windowSize", "timeline.window_size", "identity"),
        _entry(
            "temporal.framePeriodSeconds.num",
            "timeline.fps_den",
            "period_numerator_to_denominator",
        ),
        _entry(
            "temporal.framePeriodSeconds.den",
            "timeline.fps_num",
            "period_denominator_to_numerator",
        ),
        _entry(
            "temporal.framePeriodSeconds",
            "timeline.source_time_unit",
            "seconds_unit",
        ),
        _entry("sceneId", "scene_id", "identity"),
    ]
    paths = [
        ("layers[0]", 0, None),
        ("layers[1]", 1, None),
        ("layers[2]", 2, None),
        ("layers[3]", 3, None),
        ("layers[3].children[0]", 4, "layers[3].children[0]"),
    ]
    names = [
        ("id", "layer_id", "identity"),
        ("kind", "kind", "identity"),
        ("name", "name", "identity"),
        ("stackIndex", "stack_order", "identity"),
        ("visibility.fromFrame", "visible_from", "identity"),
        ("visibility.toFrameExclusive", "visible_to", "identity"),
        ("bounds", "bounds", "array_to_rect"),
        ("alpha.mode", "alpha_mode", "identity"),
        ("color.space", "source_color_space", "identity"),
        (
            "color.rgba",
            "color_linear",
            "declared_transfer_and_alpha_to_linear_straight",
        ),
        ("color.rgba[3]", "alpha", "channel_index_3_to_alpha"),
        ("opacity", "opacity", "identity"),
    ]
    for path, index, nesting_path in paths:
        for source, internal, transform in names:
            trace.append(
                _entry(
                    f"{path}.{source}",
                    f"layers[{index}].{internal}",
                    transform,
                )
            )
        if nesting_path:
            trace.append(
                _entry(
                    f"{nesting_path}.__nesting__",
                    f"layers[{index}].parent_id",
                    "nesting_to_parent_id",
                )
            )
    return trace


def _visible(
    spec: dict[str, Any],
    frame: int,
    by_id: dict[str, dict[str, Any]],
) -> bool:
    if frame < spec["visible_from"] or frame >= spec["visible_to"]:
        return False
    if spec["parent_id"] is None:
        return True
    return _visible(by_id[spec["parent_id"]], frame, by_id)


def _reference_pixel(
    specs: list[dict[str, Any]],
    frame: int,
    x: int,
    y: int,
) -> list[int]:
    by_id = {spec["layer_id"]: spec for spec in specs}
    destination = [0.0, 0.0, 0.0, 0.0]
    for spec in sorted(specs, key=lambda item: item["stack_order"]):
        if not _visible(spec, frame, by_id):
            continue
        left, top, width, height = spec["bounds"]
        if not (left <= x < left + width and top <= y < top + height):
            continue
        source_alpha = spec["alpha"] * spec["opacity"]
        inverse = 1.0 - source_alpha
        linear = _linear_color(spec)
        for channel in range(3):
            destination[channel] = (
                linear[channel] * source_alpha + destination[channel] * inverse
            )
        destination[3] = source_alpha + destination[3] * inverse
    alpha = destination[3]
    result = []
    for channel in range(3):
        straight = destination[channel] / alpha if alpha > 1e-15 else 0.0
        result.append(_quantize(_linear_to_srgb(straight)))
    result.append(_quantize(alpha))
    return result


SAMPLE_PIXELS = ((2, 2), (8, 10), (20, 10), (28, 12), (31, 8), (39, 23))


def _truth(
    seed: int,
    specs: list[dict[str, Any]],
) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
    by_id = {spec["layer_id"]: spec for spec in specs}
    frames = []
    instructions = []
    for frame in range(FRAME_COUNT):
        visible = [
            spec["layer_id"]
            for spec in sorted(specs, key=lambda item: item["stack_order"])
            if _visible(spec, frame, by_id)
        ]
        samples = [
            {"x": x, "y": y, "rgba8": _reference_pixel(specs, frame, x, y)}
            for x, y in SAMPLE_PIXELS
        ]
        frames.append(
            {
                "frame": frame,
                "time_seconds": frame * FPS_DEN / FPS_NUM,
                "visible_layers": visible,
                "samples": samples,
            }
        )
        instructions.append(
            {
                "frame": frame,
                "window": frame // WINDOW_SIZE,
                "sample_pixels": [[x, y] for x, y in SAMPLE_PIXELS],
            }
        )
    expected = {
        "generator": "reference-rgba8/1",
        "seed": seed,
        "scene_id": f"vp_stage_{seed}",
        "canvas": {"width": WIDTH, "height": HEIGHT, "channels": 4},
        "frame_count": FRAME_COUNT,
        "window_size": WINDOW_SIZE,
        "fps": {"num": FPS_NUM, "den": FPS_DEN},
        "frames": frames,
    }
    instruction_doc = {
        "scene_id": f"vp_stage_{seed}",
        "window_size": WINDOW_SIZE,
        "window_count": (FRAME_COUNT + WINDOW_SIZE - 1) // WINDOW_SIZE,
        "instructions": instructions,
    }
    visibility = {
        "scene_id": f"vp_stage_{seed}",
        "layer_endpoints": {
            spec["layer_id"]: {
                "visible_from": spec["visible_from"],
                "visible_to_exclusive": spec["visible_to"],
            }
            for spec in specs
        },
        "parent_links": {
            spec["layer_id"]: spec["parent_id"]
            for spec in specs
            if spec["parent_id"] is not None
        },
    }
    return instruction_doc, expected, visibility


def _dump(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(
            value,
            sort_keys=True,
            indent=2,
            ensure_ascii=False,
            allow_nan=False,
        )
        + "\n",
        encoding="utf-8",
    )


def generate(output: Path, seed: int = DEFAULT_SEED) -> dict[str, Any]:
    output.mkdir(parents=True, exist_ok=True)
    specs = _make_layer_specs(seed)
    v1 = _v1_document(seed, specs)
    v2 = _v2_document(seed, specs)
    v3 = _v3_document(seed, specs)
    instructions, expected, visibility = _truth(seed, specs)
    mappings = {
        "scene_id": f"vp_stage_{seed}",
        "vpfc/1": _v1_mapping(),
        "vpfc/2": _v2_mapping(),
        "vpfc/3": _v3_mapping(),
    }
    files = {
        "scene_vpfc1.json": v1,
        "scene_vpfc2.json": v2,
        "scene_vpfc3.json": v3,
        "frame_instructions.json": instructions,
        "truth/expected_frames.json": expected,
        "truth/visibility_endpoints.json": visibility,
        "truth/field_mappings.json": mappings,
    }
    for relative, value in files.items():
        _dump(output / relative, value)
    hashes = {
        relative: hashlib.sha256((output / relative).read_bytes()).hexdigest()
        for relative in sorted(files)
    }
    manifest = {
        "generator": "generate_samples.py",
        "seed": seed,
        "format": "vp-samples/1",
        "files": hashes,
    }
    _dump(output / "generator_manifest.json", manifest)
    return manifest


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--output",
        type=Path,
        default=Path(__file__).resolve().parents[1] / "samples",
    )
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED)
    arguments = parser.parse_args()
    manifest = generate(arguments.output, arguments.seed)
    print(
        f"Generated {len(manifest['files'])} deterministic sample files "
        f"in {arguments.output} with seed {arguments.seed}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
