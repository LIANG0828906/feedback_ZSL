"""Deterministic synthetic sample generator.

Fixed-seed generation of synthetic text images (varied resolutions, single
and multi-line layouts, skewed and low-contrast text, sparse and dense
regions) plus ground-truth manifests: four-point quad, text, char count and
average stroke width per region. Also emits OCR result description samples
in all supported schema versions. Fully offline.
"""

from __future__ import annotations

import hashlib
import json
import math
import random
from pathlib import Path
from typing import Dict, List

from PIL import Image, ImageDraw, ImageFont

from . import engine
from .model import ImageResult
from .schemas.exporter import export_bytes

DEFAULT_SEED = 42
DEFAULT_NUM_IMAGES = 8

_WORDS = (
    "QUEST START HP MP LVL GOLD SAVE LOAD EXIT PLAY "
    "SCORE BONUS ITEM SKILL MAP MENU YES NO OK "
    "PLAYER ENEMY BOSS STAGE CLEAR PAUSE"
).split()

_SIZES = (
    (320, 240),
    (640, 480),
    (800, 600),
    (512, 512),
    (1024, 768),
    (480, 800),
    (640, 360),
    (960, 540),
)

_LAYOUTS = ("single", "multi", "skewed", "low_contrast", "sparse", "dense")


def _text_for(rng: random.Random, dense: bool) -> str:
    count = rng.randint(3, 6) if dense else rng.randint(1, 3)
    return " ".join(rng.choice(_WORDS) for _ in range(count))


def _rotate(point, center, degrees):
    rad = math.radians(degrees)
    cos_v, sin_v = math.cos(rad), math.sin(rad)
    px, py = point[0] - center[0], point[1] - center[1]
    return (
        center[0] + px * cos_v - py * sin_v,
        center[1] + px * sin_v + py * cos_v,
    )


def _draw_region(draw, pos, text, font, fill, stroke_width, angle):
    x, y = pos
    bbox = draw.textbbox((x, y), text, font=font, stroke_width=stroke_width)
    draw.text(
        (x, y), text, font=font, fill=fill,
        stroke_width=stroke_width, stroke_fill=fill,
    )
    corners = [
        (bbox[0], bbox[1]),
        (bbox[2], bbox[1]),
        (bbox[2], bbox[3]),
        (bbox[0], bbox[3]),
    ]
    if angle:
        center = ((bbox[0] + bbox[2]) / 2.0, (bbox[1] + bbox[3]) / 2.0)
        corners = [_rotate(pt, center, angle) for pt in corners]
    return tuple((int(round(px)), int(round(py))) for px, py in corners)


def _clamp_quad(quad, width, height):
    return tuple(
        (min(max(px, 0), width), min(max(py, 0), height)) for px, py in quad
    )


def gt_fingerprint(gt: Dict) -> str:
    canonical = json.dumps(gt["regions"], sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def generate_image(index: int, rng: random.Random, out_dir: Path) -> Dict:
    width, height = _SIZES[index % len(_SIZES)]
    layout = _LAYOUTS[index % len(_LAYOUTS)]
    image_id = f"sample_{index:03d}"

    bg_level = rng.randint(180, 230)
    image = Image.new("RGB", (width, height), (bg_level, bg_level, bg_level - 10))
    draw = ImageDraw.Draw(image)

    if layout == "single":
        n_lines, dense = 1, False
    elif layout == "multi":
        n_lines, dense = rng.randint(3, 5), False
    elif layout == "skewed":
        n_lines, dense = rng.randint(2, 4), False
    elif layout == "low_contrast":
        n_lines, dense = rng.randint(2, 4), False
    elif layout == "sparse":
        n_lines, dense = 2, False
    else:  # dense
        n_lines, dense = rng.randint(4, 6), True

    regions: List[Dict] = []
    y_cursor = rng.randint(10, 30)
    for _ in range(n_lines):
        font_size = rng.randint(14, 28)
        font = ImageFont.load_default(size=font_size)
        stroke = rng.randint(0, 2)
        if layout == "low_contrast":
            delta = rng.randint(15, 40)
            fill = (bg_level - delta, bg_level - delta, bg_level - delta - 5)
        else:
            fill = (rng.randint(0, 90), rng.randint(0, 90), rng.randint(0, 90))
        angle = rng.uniform(-8.0, 8.0) if layout == "skewed" else 0.0

        segments = rng.randint(2, 3) if dense else 1
        x_cursor = rng.randint(8, 40)
        max_bottom = y_cursor
        for _ in range(segments):
            text = _text_for(rng, dense)
            quad = _draw_region(
                draw, (x_cursor, y_cursor), text, font, fill, stroke, angle
            )
            quad = _clamp_quad(quad, width, height)
            regions.append(
                {
                    "quad": [[px, py] for px, py in quad],
                    "text": text,
                    "char_count": len(text),
                    "avg_stroke_width": float(max(stroke, 1)),
                }
            )
            seg_right = max(p[0] for p in quad)
            max_bottom = max(max_bottom, max(p[1] for p in quad))
            gap = rng.randint(4, 10) if dense else rng.randint(30, 60)
            x_cursor = seg_right + gap
            if x_cursor > width - 60:
                break
        y_cursor = max_bottom + rng.randint(14, 34)
        if y_cursor > height - 40:
            break

    image_path = out_dir / "images" / f"{image_id}.png"
    image_path.parent.mkdir(parents=True, exist_ok=True)
    image.save(image_path)
    content_hash = hashlib.sha256(image_path.read_bytes()).hexdigest()

    return {
        "image_id": image_id,
        "image_file": f"images/{image_id}.png",
        "width": width,
        "height": height,
        "layout": layout,
        "content_hash": content_hash,
        "regions": regions,
    }


def gt_regions_of(gt: Dict) -> List[engine.GtRegion]:
    return [
        engine.GtRegion(
            quad=tuple(tuple(pt) for pt in region["quad"]),
            text=region["text"],
            char_count=region["char_count"],
            avg_stroke_width=region["avg_stroke_width"],
        )
        for region in gt["regions"]
    ]


def generate_dataset(
    out_dir, seed: int = DEFAULT_SEED, num_images: int = DEFAULT_NUM_IMAGES
) -> List[Dict]:
    """Generate images, ground truth, fingerprints and v1/v2/v3 samples."""
    out_dir = Path(out_dir)
    rng = random.Random(seed)
    gt_dir = out_dir / "ground_truth"
    sample_dir = out_dir / "sample_results"
    gt_dir.mkdir(parents=True, exist_ok=True)
    sample_dir.mkdir(parents=True, exist_ok=True)

    manifests: List[Dict] = []
    for index in range(num_images):
        gt = generate_image(index, rng, out_dir)
        gt["fingerprint"] = gt_fingerprint(gt)
        gt_path = gt_dir / f"{gt['image_id']}.json"
        gt_path.write_text(
            json.dumps(gt, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )

        lines = engine.detect_and_recognize(gt["image_id"], gt_regions_of(gt))
        result = ImageResult(
            image_id=gt["image_id"],
            width=gt["width"],
            height=gt["height"],
            lines=lines,
            source_version="generated",
        )
        for version in ("1.0", "2.0", "3.0"):
            payload, _warnings = export_bytes(result, version)
            target = sample_dir / version / f"{gt['image_id']}.json"
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(payload)
        manifests.append(gt)

    manifest_path = out_dir / "manifest.json"
    manifest_path.write_text(
        json.dumps(
            {
                "seed": seed,
                "num_images": num_images,
                "images": [
                    {
                        "image_id": gt["image_id"],
                        "content_hash": gt["content_hash"],
                        "fingerprint": gt["fingerprint"],
                    }
                    for gt in manifests
                ],
            },
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )
    return manifests
