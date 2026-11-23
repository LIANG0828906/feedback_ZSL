"""Image loading/saving; decode failures are mapped to E_DECODE."""

from __future__ import annotations

import os

import numpy as np
from PIL import Image, UnidentifiedImageError

from .errors import GeoError, ErrorCode


def load_image(path: str) -> np.ndarray:
    """Load an image as HxW (L) or HxWx3 (RGB) uint8 array."""
    if not os.path.exists(path):
        raise GeoError(ErrorCode.E_DECODE, f"file not found: {path}",
                       field="path")
    try:
        with open(path, "rb") as fh:
            header = fh.read(2)
            if len(header) < 2:
                raise GeoError(ErrorCode.E_DECODE,
                               f"truncated image file: {path}", field="path")
        with Image.open(path) as im:
            im.load()
            if im.mode == "L":
                return np.asarray(im, dtype=np.uint8)
            if im.mode == "RGB":
                return np.asarray(im, dtype=np.uint8)
            return np.asarray(im.convert("RGB"), dtype=np.uint8)
    except (UnidentifiedImageError, OSError, ValueError) as exc:
        raise GeoError(ErrorCode.E_DECODE,
                       f"failed to decode {path}: {exc}",
                       field="path") from exc


def save_image(path: str, image: np.ndarray) -> None:
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    arr = np.asarray(image)
    if arr.ndim == 2:
        im = Image.fromarray(arr.astype(np.uint8), mode="L")
    else:
        im = Image.fromarray(arr.astype(np.uint8), mode="RGB")
    im.save(path)
