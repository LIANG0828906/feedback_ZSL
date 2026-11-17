"""Per-chunk RGBA texture baking from normals (Lambert + gamma)."""

import hashlib
import io

import numpy as np
from PIL import Image

GAMMA = 2.2
AMBIENT = 0.15


def light_direction():
    """Fixed, module-level lighting direction (unit vector, y-up)."""
    v = np.array([0.5, 0.8, 0.3], dtype=np.float64)
    return v / np.linalg.norm(v)


def bake_texture(normals):
    """Bake an (n, n, 3) normal field to an (n, n, 4) uint8 RGBA texture."""
    lambert = np.clip(normals @ light_direction(), 0.0, 1.0)
    shade = AMBIENT + (1.0 - AMBIENT) * lambert
    corrected = np.power(shade, 1.0 / GAMMA)
    rgb = np.round(corrected * 255.0).astype(np.uint8)
    rgba = np.empty(normals.shape[:2] + (4,), dtype=np.uint8)
    rgba[..., 0] = rgba[..., 1] = rgba[..., 2] = rgb
    rgba[..., 3] = 255
    return rgba


def encode_png(rgba):
    """Deterministically encode an RGBA array to PNG bytes."""
    buf = io.BytesIO()
    Image.fromarray(rgba, mode="RGBA").save(buf, format="PNG")
    return buf.getvalue()


def texture_digest(png_bytes):
    return hashlib.sha256(png_bytes).hexdigest()
