"""Deterministic local sample generation and truth manifest parsing."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any, Dict, Iterable, List, Mapping, Optional, Tuple

import cv2
import numpy as np

from .errors import EdgePipeError, ErrorCode

SAMPLE_SCHEMA = "edgepipe.samples/v1"
TRUTH_SCHEMA = "edgepipe.truth/v1"


def _segment(identifier: str, x1: int, y1: int, x2: int, y2: int) -> Dict[str, Any]:
    return {"id": identifier, "x1": x1, "y1": y1, "x2": x2, "y2": y2}


def _circle(identifier: str, cx: int, cy: int, diameter: int) -> Dict[str, Any]:
    return {"id": identifier, "cx": cx, "cy": cy, "diameter": diameter}


SPECS: Tuple[Dict[str, Any], ...] = (
    {
        "name": "gray_lines_lowcontrast_96x128",
        "filename": "gray_lines_lowcontrast_96x128.png",
        "width": 128,
        "height": 96,
        "channels": 1,
        "seed": 20260101,
        "base": 132,
        "ink": 112,
        "noise_sigma": 2.2,
        "thickness": 5,
        "segments": [
            _segment("s0", 13, 19, 112, 24),
            _segment("s1", 20, 47, 17, 82),
            _segment("s2", 34, 40, 79, 78),
        ],
        "circles": [_circle("c0", 96, 58, 26)],
        "recommended_config": {
            "gradient_operator": "sobel",
            "low_threshold": 20.0,
            "high_threshold": 45.0,
            "open_kernel_size": 1,
            "close_kernel_size": 3,
            "thinning_iterations": 4,
            "prune_length": 8,
            "block_size": 48,
        },
    },
    {
        "name": "gray_noisy_131x97",
        "filename": "gray_noisy_131x97.png",
        "width": 97,
        "height": 131,
        "channels": 1,
        "seed": 20260102,
        "base": 108,
        "ink": 34,
        "noise_sigma": 8.0,
        "thickness": 5,
        "segments": [
            _segment("s0", 10, 18, 84, 30),
            _segment("s1", 14, 75, 54, 119),
            _segment("s2", 60, 106, 88, 112),
        ],
        "circles": [_circle("c0", 63, 58, 30)],
        "recommended_config": {
            "gradient_operator": "sobel",
            "low_threshold": 60.0,
            "high_threshold": 140.0,
            "open_kernel_size": 1,
            "close_kernel_size": 3,
            "thinning_iterations": 4,
            "prune_length": 8,
            "block_size": 48,
        },
    },
    {
        "name": "color_shapes_120x160",
        "filename": "color_shapes_120x160.png",
        "width": 160,
        "height": 120,
        "channels": 3,
        "seed": 20260103,
        "base_bgr": [54, 78, 94],
        "ink_bgr": [18, 214, 36],
        "noise_sigma": 3.0,
        "thickness": 6,
        "segments": [
            _segment("s0", 14, 22, 139, 28),
            _segment("s1", 25, 50, 22, 101),
            _segment("s2", 51, 91, 126, 98),
            _segment("s3", 78, 31, 117, 81),
        ],
        "circles": [
            _circle("c0", 56, 63, 28),
            _circle("c1", 137, 63, 20),
        ],
        "recommended_config": {
            "gradient_operator": "sobel",
            "low_threshold": 40.0,
            "high_threshold": 90.0,
            "open_kernel_size": 1,
            "close_kernel_size": 3,
            "thinning_iterations": 4,
            "prune_length": 8,
            "block_size": 48,
        },
    },
    {
        "name": "color_lowcontrast_150x110",
        "filename": "color_lowcontrast_150x110.png",
        "width": 110,
        "height": 150,
        "channels": 3,
        "seed": 20260104,
        "base_bgr": [122, 132, 140],
        "ink_bgr": [101, 112, 119],
        "noise_sigma": 1.4,
        "thickness": 5,
        "segments": [
            _segment("s0", 12, 18, 92, 21),
            _segment("s1", 18, 46, 94, 53),
            _segment("s2", 26, 75, 55, 125),
            _segment("s3", 62, 87, 93, 136),
        ],
        "circles": [_circle("c0", 69, 62, 28)],
        "recommended_config": {
            "gradient_operator": "scharr",
            "low_threshold": 30.0,
            "high_threshold": 60.0,
            "open_kernel_size": 1,
            "close_kernel_size": 3,
            "thinning_iterations": 4,
            "prune_length": 8,
            "block_size": 48,
        },
    },
)


def _pixel_hash(image: np.ndarray) -> str:
    header = f"{image.shape}:{image.dtype}".encode("ascii")
    digest = hashlib.sha256()
    digest.update(header)
    digest.update(np.ascontiguousarray(image).tobytes())
    return digest.hexdigest()


def _draw_scene(spec: Mapping[str, Any]) -> np.ndarray:
    rng = np.random.default_rng(int(spec["seed"]))
    height = int(spec["height"])
    width = int(spec["width"])
    channels = int(spec["channels"])
    sigma = float(spec["noise_sigma"])
    thickness = int(spec["thickness"])

    if channels == 1:
        base = np.full((height, width), int(spec["base"]), dtype=np.float32)
        noise = rng.normal(0.0, sigma, size=(height, width))
        image = np.clip(np.rint(base + noise), 0, 255).astype(np.uint8)
        ink = int(spec["ink"])
    else:
        base = np.array(spec["base_bgr"], dtype=np.float32)
        noise = rng.normal(
            0.0, sigma, size=(height, width, 3)
        )
        image = np.clip(
            np.rint(base.reshape(1, 1, 3) + noise), 0, 255
        ).astype(np.uint8)
        ink = tuple(int(value) for value in spec["ink_bgr"])

    for segment in spec["segments"]:
        cv2.line(
            image,
            (int(segment["x1"]), int(segment["y1"])),
            (int(segment["x2"]), int(segment["y2"])),
            ink,
            thickness,
            cv2.LINE_8,
        )
    for circle in spec["circles"]:
        radius = int(circle["diameter"]) // 2
        cv2.circle(
            image,
            (int(circle["cx"]), int(circle["cy"])),
            radius,
            ink,
            thickness=-1,
            lineType=cv2.LINE_8,
        )
    return np.ascontiguousarray(image)


def generate_samples(
    output_dir: str | Path,
    *,
    clean: bool = False,
) -> Dict[str, Any]:
    """Create PNG samples and a deterministic, parseable truth manifest."""

    root = Path(output_dir)
    root.mkdir(parents=True, exist_ok=True)
    if clean:
        for pattern in ("*.png", "*.json"):
            for path in root.glob(pattern):
                path.unlink()

    manifest_samples: List[Dict[str, Any]] = []
    for spec in SPECS:
        image = _draw_scene(spec)
        path = root / str(spec["filename"])
        if not cv2.imwrite(str(path), image):
            raise EdgePipeError(
                ErrorCode.IO,
                f"无法写入样本: {path}",
                field="path",
            )
        item = {
            "name": spec["name"],
            "path": str(spec["filename"]),
            "width": int(spec["width"]),
            "height": int(spec["height"]),
            "channels": int(spec["channels"]),
            "seed": int(spec["seed"]),
            "scene": "noisy" if float(spec["noise_sigma"]) >= 6 else "low_contrast",
            "content_sha256": _pixel_hash(image),
            "segments": list(spec["segments"]),
            "circles": list(spec["circles"]),
            "recommended_config": dict(spec["recommended_config"]),
        }
        manifest_samples.append(item)

    manifest = {
        "schema": SAMPLE_SCHEMA,
        "truth_schema": TRUTH_SCHEMA,
        "sample_count": len(manifest_samples),
        "samples": manifest_samples,
    }
    (root / "index.json").write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True),
        encoding="utf-8",
    )
    return manifest


def load_manifest(path: str | Path) -> Dict[str, Any]:
    manifest_path = Path(path)
    if manifest_path.is_dir():
        manifest_path = manifest_path / "index.json"
    try:
        data = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise EdgePipeError(
            ErrorCode.DECODE,
            f"无法解析样本清单: {manifest_path}",
            field="manifest",
            details={"error": str(exc)},
        ) from exc
    if data.get("schema") != SAMPLE_SCHEMA:
        raise EdgePipeError(
            ErrorCode.DECODE,
            "样本清单 schema 不匹配",
            field="manifest.schema",
            details={"actual": data.get("schema")},
        )
    return data


def find_sample(manifest: Mapping[str, Any], name: str) -> Dict[str, Any]:
    for item in manifest.get("samples", []):
        if item.get("name") == name or item.get("path") == name:
            return dict(item)
    direct = Path(name)
    if direct.exists():
        for item in manifest.get("samples", []):
            if item.get("path") == direct.name:
                return dict(item)
    raise EdgePipeError(
        ErrorCode.PARA,
        f"未找到样本: {name}",
        field="sample",
        details={"available": [item.get("name") for item in manifest.get("samples", [])]},
    )


def decode_image(path: str | Path) -> np.ndarray:
    image_path = Path(path)
    if not image_path.exists():
        raise EdgePipeError(
            ErrorCode.DECODE,
            f"图像不存在: {image_path}",
            field="path",
        )
    try:
        data = np.fromfile(str(image_path), dtype=np.uint8)
    except OSError as exc:
        raise EdgePipeError(
            ErrorCode.DECODE,
            f"图像读取失败: {image_path}",
            field="path",
            details={"error": str(exc)},
        ) from exc
    image = cv2.imdecode(data, cv2.IMREAD_UNCHANGED)
    if image is None or image.size == 0:
        raise EdgePipeError(
            ErrorCode.DECODE,
            f"图像解码失败或内容损坏: {image_path}",
            field="path",
        )
    return image


def sample_image_path(sample: Mapping[str, Any], manifest_dir: str | Path) -> Path:
    return Path(manifest_dir) / str(sample["path"])


def truth_from_sample(sample: Mapping[str, Any]) -> Dict[str, Any]:
    return {
        "schema": TRUTH_SCHEMA,
        "sample": sample.get("name"),
        "segments": list(sample.get("segments", [])),
        "circles": list(sample.get("circles", [])),
    }
