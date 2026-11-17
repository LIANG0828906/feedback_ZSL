from __future__ import annotations

import io
from typing import Any

import numpy as np
from PIL import Image, PngImagePlugin

from .io_utils import canonical_json_bytes, sha256_bytes


def encode_rgba_png(
    rgba: np.ndarray, metadata: dict[str, Any]
) -> tuple[bytes, str]:
    image = Image.fromarray(np.ascontiguousarray(rgba, dtype=np.uint8), "RGBA")
    png_info = PngImagePlugin.PngInfo()
    metadata_json = canonical_json_bytes(metadata).decode("utf-8")
    png_info.add_text("volverify", metadata_json)
    buffer = io.BytesIO()
    image.save(
        buffer,
        format="PNG",
        pnginfo=png_info,
        compress_level=9,
        optimize=False,
    )
    data = buffer.getvalue()
    return data, sha256_bytes(data)


def encode_depth_f32(depth: np.ndarray) -> tuple[bytes, str]:
    data = np.ascontiguousarray(depth, dtype="<f4").tobytes(order="C")
    return data, sha256_bytes(data)


def image_metadata(
    frame_id: str,
    scene_fingerprint: str,
    requested_step: float,
    actual_step_min: float,
    actual_step_max: float,
    convergence: dict[str, Any],
    ray_start_rule: str,
    ray_direction_rule: str,
    width: int,
    height: int,
) -> dict[str, Any]:
    return {
        "tool": "volverify",
        "frame_id": frame_id,
        "scene_fingerprint": scene_fingerprint,
        "width": width,
        "height": height,
        "rgba_encoding": "straight_alpha_srgb8",
        "tone_mapping": "rgb=(1-exp(-linear_radiance*exposure))^(1/gamma), alpha=-expm1(-optical_depth)",
        "sampling": {
            "requested_step": requested_step,
            "actual_step_min": actual_step_min,
            "actual_step_max": actual_step_max,
            "ray_start_rule": ray_start_rule,
            "ray_direction_rule": ray_direction_rule,
        },
        "depth": {
            "file": "frame.depth.f32",
            "dtype": "<f4",
            "shape": [height, width],
            "order": "C",
        },
        "convergence": {
            "tolerance": convergence["tolerance"],
            "mean_relative_deviations": convergence[
                "mean_relative_deviations"
            ],
            "monotonic": convergence["monotonic"],
            "passed": convergence["passed"],
        },
    }

