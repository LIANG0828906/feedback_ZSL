from __future__ import annotations

import math
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .codec import encode_atlas_bytes, read_atlas
from .constants import BIT_DEPTHS, COLOR_SPACE_TO_CODE, TRANSFER_TO_CODE
from .errors import AtlasError, ErrorCode
from .hashing import hash_value
from .model import AtlasImage
from .transfer import encode_transfer

SUITE_PRESETS: tuple[dict[str, Any], ...] = (
    {
        "name": "rec709_srgb_64x48_10bit",
        "width": 64,
        "height": 48,
        "bit_depth": 10,
        "color_space": "rec709",
        "transfer": "srgb",
        "grid_cols": 2,
        "grid_rows": 2,
        "source_peak_nits": 1000.0,
    },
    {
        "name": "p3_linear_96x64_12bit",
        "width": 96,
        "height": 64,
        "bit_depth": 12,
        "color_space": "display-p3",
        "transfer": "linear",
        "grid_cols": 3,
        "grid_rows": 2,
        "source_peak_nits": 1000.0,
    },
    {
        "name": "rec2020_pq_80x80_10bit",
        "width": 80,
        "height": 80,
        "bit_depth": 10,
        "color_space": "rec2020",
        "transfer": "pq",
        "grid_cols": 4,
        "grid_rows": 4,
        "source_peak_nits": 1000.0,
    },
)


def _scene_linear(width: int, height: int, seed: int) -> np.ndarray:
    y, x = np.mgrid[0:height, 0:width]
    u = (x.astype(np.float64) + 0.5) / width
    v = (y.astype(np.float64) + 0.5) / height
    phase = (int(seed) % 10000) / 10000.0
    red = 0.03 + 0.70 * u
    green = 0.04 + 0.64 * (1.0 - v)
    blue = 0.06 + 0.45 * (0.5 + 0.5 * np.sin(2.0 * math.pi * (u + phase)))
    highlight = np.exp(
        -(
            np.square(u - (0.68 + 0.08 * math.sin(phase * 17.0)))
            + np.square(v - 0.42)
        )
        / 0.016
    )
    red = red + 0.80 * highlight
    green = green + 0.55 * highlight
    blue = blue + 0.22 * highlight
    checker = (((x // max(1, width // 8)) + (y // max(1, height // 8))) & 1).astype(
        np.float64
    )
    gain = 0.93 + 0.07 * checker
    rgb = np.stack([red * gain, green * gain, blue * gain], axis=-1)
    return np.clip(rgb, 0.0, 1.0)


def generate_atlas(
    path: str | Path,
    *,
    width: int = 64,
    height: int = 48,
    bit_depth: int = 10,
    color_space: str = "rec709",
    transfer: str = "srgb",
    grid_cols: int = 2,
    grid_rows: int = 2,
    source_peak_nits: float = 1000.0,
    seed: int = 7,
) -> AtlasImage:
    if width <= 0 or height <= 0:
        raise AtlasError(
            ErrorCode.E_USAGE,
            "sample width and height must be positive",
            field="width" if width <= 0 else "height",
        )
    if bit_depth not in BIT_DEPTHS:
        raise AtlasError(
            ErrorCode.E_USAGE,
            f"bit_depth must be one of {list(BIT_DEPTHS)}",
            field="bit_depth",
        )
    if color_space not in COLOR_SPACE_TO_CODE:
        raise AtlasError(
            ErrorCode.E_USAGE,
            f"unknown color space {color_space!r}",
            field="color_space",
        )
    if transfer not in TRANSFER_TO_CODE:
        raise AtlasError(
            ErrorCode.E_USAGE,
            f"unknown transfer function {transfer!r}",
            field="transfer",
        )
    if grid_cols <= 0 or grid_rows <= 0:
        raise AtlasError(
            ErrorCode.E_USAGE,
            "tile grid dimensions must be positive",
            field="grid_cols" if grid_cols <= 0 else "grid_rows",
        )
    if width % grid_cols or height % grid_rows:
        raise AtlasError(
            ErrorCode.E_USAGE,
            "sample tile grid must evenly divide the resolution",
            field="grid_cols" if width % grid_cols else "grid_rows",
        )
    linear = _scene_linear(width, height, seed)
    encoded_rgb = encode_transfer(linear, transfer, source_peak_nits)
    max_code = (1 << bit_depth) - 1
    codes = np.floor(np.clip(encoded_rgb, 0.0, 1.0) * max_code + 0.5)
    stored_rgb = (codes.astype(np.float32) / max_code).astype(np.float16)
    rgba = np.ones((height, width, 4), dtype=np.float16)
    rgba[..., :3] = stored_rgb
    payload = encode_atlas_bytes(
        rgba,
        bit_depth=bit_depth,
        color_space=color_space,
        transfer=transfer,
        grid_cols=grid_cols,
        grid_rows=grid_rows,
        source_peak_nits=source_peak_nits,
    )
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_bytes(payload)
    return read_atlas(destination)


def generate_suite(
    directory: str | Path,
    *,
    seed: int = 7,
    include_baseline: bool = True,
) -> list[dict[str, Any]]:
    from .pipeline import AtlasPipeline

    root = Path(directory)
    root.mkdir(parents=True, exist_ok=True)
    results: list[dict[str, Any]] = []
    for index, preset in enumerate(SUITE_PRESETS):
        data = dict(preset)
        name = str(data.pop("name"))
        target = root / f"{name}.atlas"
        atlas = generate_atlas(target, seed=seed + index * 101, **data)
        record: dict[str, Any] = {
            "name": name,
            "path": str(target),
            "source_hash": atlas.source_hash,
            "width": atlas.header.width,
            "height": atlas.header.height,
            "bit_depth": atlas.header.bit_depth,
            "color_space": atlas.header.color_space,
            "transfer_function": atlas.header.transfer_function,
            "grid_cols": atlas.header.grid_cols,
            "grid_rows": atlas.header.grid_rows,
            "tiles": atlas.tile_count,
        }
        if include_baseline:
            pipeline = AtlasPipeline(atlas)
            version = pipeline.current_version_record
            record["baseline_version"] = version.number
            record["baseline_chain_hash"] = version.chain_hash
            record["baseline_content_hash"] = version.content_hash
        results.append(record)
    manifest = {
        "generator": "atlas_pipeline.sample",
        "seed": int(seed),
        "presets": results,
    }
    manifest_path = root / "suite-manifest.json"
    import json

    manifest_path.write_text(
        json.dumps(manifest, indent=2, sort_keys=True, ensure_ascii=True) + "\n",
        encoding="ascii",
    )
    return results
