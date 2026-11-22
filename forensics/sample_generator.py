"""Deterministic local sample generator.

A fixed master seed drives every random draw, so running this module repeatedly
produces byte-identical images and ground-truth manifests.  Each image contains
smooth gradients plus known geometric primitives (rectangle / circle / line),
and one or more block-aligned tamper regions whose pixels are replaced by a
high-variance noise patch.  The manifest explicitly records:

* image resolution and channel count,
* geometric primitive bounding boxes,
* the tamper mask as rectangles and as a per-block boolean list,
* the original (pre-tamper) reference pixels for every tampered block.

Tamper rectangles are aligned to the block grid so that preview and precise
paths reach exactly the same tampered-block decision set on these samples.
"""

import json
import os

import numpy as np
from PIL import Image

MASTER_SEED = 20261123
BLOCK_SIZE = 32

# name -> (height, width, channels, tamper count)
SAMPLE_SPECS = [
    ("sample_gray_256", 256, 256, 1, 1),
    ("sample_rgb_256", 256, 256, 3, 2),
    ("sample_gray_320x240", 240, 320, 1, 1),
    ("sample_rgb_384x320", 320, 384, 3, 2),
    ("sample_gray_384", 384, 384, 1, 2),
    ("sample_rgb_512x384", 384, 512, 3, 3),
]


def _base_image(height, width, channels, rng):
    """Smooth, low-noise background so tampered high-variance patches stand out."""
    yy, xx = np.mgrid[0:height, 0:width].astype(np.float64)
    grad_x = xx / max(width - 1, 1)
    grad_y = yy / max(height - 1, 1)
    base = np.zeros((height, width, channels), dtype=np.float64)
    for c in range(channels):
        phase = rng.uniform(0.0, np.pi)
        base[:, :, c] = (
            60.0
            + 90.0 * grad_x
            + 40.0 * grad_y
            + 18.0 * np.sin(grad_x * 6.0 + phase)
            + 12.0 * np.cos(grad_y * 5.0 + phase)
        )
    # Very mild deterministic sensor noise (well below tamper variance).
    sensor = rng.normal(0.0, 2.0, size=(height, width, channels))
    return np.clip(base + sensor, 0, 255)


def _draw_primitives(image, rng):
    """Stamp known primitives and return their bounding boxes (x0,y0,x1,y1)."""
    height, width, channels = image.shape
    primitives = []

    # Filled rectangle.
    rx0 = int(rng.randint(16, max(17, width // 3)))
    ry0 = int(rng.randint(16, max(17, height // 3)))
    rx1 = min(width - 1, rx0 + int(rng.randint(24, 48)))
    ry1 = min(height - 1, ry0 + int(rng.randint(24, 48)))
    color = rng.randint(120, 240, size=channels)
    image[ry0:ry1, rx0:rx1, :] = color
    primitives.append(
        {"type": "rectangle", "bbox": [rx0, ry0, rx1, ry1], "color": color.tolist()}
    )

    # Filled circle.
    cx = int(rng.randint(width // 2, max(width // 2 + 1, width - 24)))
    cy = int(rng.randint(height // 4, max(height // 4 + 1, height - 24)))
    radius = int(rng.randint(14, 26))
    yy, xx = np.ogrid[:height, :width]
    disk = (xx - cx) ** 2 + (yy - cy) ** 2 <= radius ** 2
    color = rng.randint(40, 200, size=channels)
    image[disk] = color
    primitives.append(
        {
            "type": "circle",
            "bbox": [cx - radius, cy - radius, cx + radius, cy + radius],
            "center": [cx, cy],
            "radius": radius,
            "color": color.tolist(),
        }
    )

    # Line (thick, drawn as a thin rectangle for deterministic bounds).
    lx0 = int(rng.randint(8, max(9, width // 2)))
    ly0 = int(rng.randint(height // 2, max(height // 2 + 1, height - 16)))
    length = int(rng.randint(40, 90))
    thickness = 3
    lx1 = min(width - 1, lx0 + length)
    ly1 = min(height - 1, ly0 + thickness)
    color = rng.randint(0, 120, size=channels)
    image[ly0:ly1, lx0:lx1, :] = color
    primitives.append(
        {"type": "line", "bbox": [lx0, ly0, lx1, ly1], "color": color.tolist()}
    )
    return primitives


def _grid_blocks(height, width, block_size):
    blocks = []
    for by in range(0, height, block_size):
        for bx in range(0, width, block_size):
            blocks.append(
                (
                    bx,
                    by,
                    min(block_size, width - bx),
                    min(block_size, height - by),
                )
            )
    return blocks


def _choose_tamper_blocks(height, width, count, rng):
    """Pick non-adjacent block-aligned tamper regions, deterministic per seed."""
    cols = width // BLOCK_SIZE
    rows = height // BLOCK_SIZE
    chosen = set()
    attempts = 0
    while len(chosen) < count and attempts < 200:
        attempts += 1
        bcx = int(rng.randint(0, cols))
        bcy = int(rng.randint(0, rows))
        # Keep a one-block moat so regions do not touch.
        if all(abs(bcx - x) + abs(bcy - y) > 1 for (x, y) in chosen):
            chosen.add((bcx, bcy))
    return sorted(chosen)


def generate_sample(name, height, width, channels, tamper_count, out_dir):
    rng = np.random.RandomState(MASTER_SEED + hash(name) % 100000)
    image = _base_image(height, width, channels, rng)
    primitives = _draw_primitives(image, rng)
    original = image.copy()

    tampers = []
    tampered_block_ids = set()
    blocks = _grid_blocks(height, width, BLOCK_SIZE)
    for (bcx, bcy) in _choose_tamper_blocks(height, width, tamper_count, rng):
        x0 = bcx * BLOCK_SIZE
        y0 = bcy * BLOCK_SIZE
        x1 = min(width, x0 + BLOCK_SIZE)
        y1 = min(height, y0 + BLOCK_SIZE)
        # High-variance replacement patch: clearly separable from background.
        patch = rng.normal(
            rng.randint(80, 160),
            rng.randint(28, 40),
            size=(y1 - y0, x1 - x0, channels),
        )
        image[y0:y1, x0:x1, :] = np.clip(patch, 0, 255)
        ref = original[y0:y1, x0:x1, :]
        tampers.append(
            {
                "rect": [int(x0), int(y0), int(x1), int(y1)],
                "block_coord": [bcx, bcy],
                "original_mean": float(ref.mean()),
                "original_std": float(ref.std()),
                # Explicit reference pixels (rounded) for the tampered region.
                "original_pixels": np.round(ref).astype(np.uint8).tolist(),
            }
        )

    block_ids = []
    for idx, (bx, by, bw, bh) in enumerate(blocks):
        for t in tampers:
            tx0, ty0, tx1, ty1 = t["rect"]
            if bx >= tx0 and by >= ty0 and bx + bw <= tx1 and by + bh <= ty1:
                tampered_block_ids.add(idx)
        block_ids.append(idx)

    tamper_mask = [idx in tampered_block_ids for idx in block_ids]

    image_u8 = np.round(image).astype(np.uint8)
    if channels == 1:
        pil = Image.fromarray(image_u8[:, :, 0], mode="L")
    else:
        pil = Image.fromarray(image_u8, mode="RGB")
    img_path = os.path.join(out_dir, name + ".png")
    pil.save(img_path, format="PNG")

    manifest = {
        "image_file": name + ".png",
        "width": width,
        "height": height,
        "channels": channels,
        "block_size": BLOCK_SIZE,
        "master_seed": MASTER_SEED,
        "primitives": primitives,
        "tamper_regions": [
            {k: v for k, v in t.items() if k != "original_pixels"} for t in tampers
        ],
        "tamper_mask": tamper_mask,
        "tampered_block_ids": sorted(tampered_block_ids),
        "reference_pixels": {
            str(i): t["original_pixels"] for i, t in enumerate(tampers)
        },
    }
    manifest_path = os.path.join(out_dir, name + ".manifest.json")
    with open(manifest_path, "w", encoding="utf-8") as fh:
        json.dump(manifest, fh)
    return manifest


def generate_all(out_dir):
    os.makedirs(out_dir, exist_ok=True)
    manifests = []
    for spec in SAMPLE_SPECS:
        manifests.append(generate_sample(*spec, out_dir=out_dir))
    index = {
        "master_seed": MASTER_SEED,
        "block_size": BLOCK_SIZE,
        "samples": [m["image_file"] for m in manifests],
    }
    with open(os.path.join(out_dir, "index.json"), "w", encoding="utf-8") as fh:
        json.dump(index, fh, indent=2)
    return manifests


if __name__ == "__main__":
    import sys

    target = sys.argv[1] if len(sys.argv) > 1 else os.path.join(
        os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "samples"
    )
    result = generate_all(target)
    print("Generated %d deterministic samples in %s" % (len(result), target))
