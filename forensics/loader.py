"""Sample loading, structural validation and block indexing.

Loads a PNG plus its ground-truth manifest, verifies that the manifest's
resolution / channel description matches the decoded image, and builds a block
grid with explicit per-block pixel boundaries.
"""

import json
import os

import numpy as np
from PIL import Image

from .errors import ForensicsError, E_DECODE, E_META, E_PARA


class BlockIndex:
    """Maps block ids to pixel boundaries over the image grid."""

    def __init__(self, height, width, block_size):
        if not isinstance(block_size, int) or block_size <= 0:
            raise ForensicsError(
                E_PARA,
                "block_size must be a positive integer, got %r" % block_size,
                field="block_size",
            )
        if block_size > max(height, width):
            raise ForensicsError(
                E_PARA,
                "block_size %d exceeds image dimensions %dx%d"
                % (block_size, width, height),
                field="block_size",
            )
        self.height = height
        self.width = width
        self.block_size = block_size
        self._blocks = []
        for by in range(0, height, block_size):
            for bx in range(0, width, block_size):
                self._blocks.append(
                    (
                        bx,
                        by,
                        min(block_size, width - bx),
                        min(block_size, height - by),
                    )
                )

    def __len__(self):
        return len(self._blocks)

    def bounds(self, block_id):
        if not isinstance(block_id, (int, np.integer)) or block_id < 0 or block_id >= len(
            self._blocks
        ):
            raise ForensicsError(
                E_PARA,
                "block_id %r out of range [0,%d)" % (block_id, len(self._blocks)),
                field="block_id",
            )
        return self._blocks[int(block_id)]

    def all_bounds(self):
        return list(self._blocks)

    def grid_shape(self):
        cols = (self.width + self.block_size - 1) // self.block_size
        rows = (self.height + self.block_size - 1) // self.block_size
        return rows, cols


class ImageSample:
    """A decoded image paired with its validated manifest and block index."""

    def __init__(self, name, pixels, manifest, block_size):
        self.name = name
        # pixels shape: (H, W, C), uint8, C in {1, 3}; channel axis always kept.
        self.pixels = pixels
        self.manifest = manifest
        self.height, self.width, self.channels = pixels.shape
        self.block_index = BlockIndex(self.height, self.width, block_size)

    def block_pixels(self, block_id):
        bx, by, bw, bh = self.block_index.bounds(block_id)
        return self.pixels[by : by + bh, bx : bx + bw, :]

    def ground_truth_mask(self):
        """Boolean per-block tamper ground truth, aligned to block order."""
        mask = self.manifest.get("tamper_mask")
        if mask is None or len(mask) != len(self.block_index):
            raise ForensicsError(
                E_META,
                "manifest tamper_mask length does not match block count",
                field="tamper_mask",
            )
        return np.asarray(mask, dtype=bool)


def _load_manifest(manifest_path):
    if not os.path.exists(manifest_path):
        raise ForensicsError(
            E_DECODE, "manifest file is missing: %s" % manifest_path, field="manifest"
        )
    try:
        with open(manifest_path, "r", encoding="utf-8") as fh:
            return json.load(fh)
    except (json.JSONDecodeError, OSError) as exc:
        raise ForensicsError(
            E_DECODE, "cannot parse manifest: %s" % exc, field="manifest"
        )


def load_sample(image_path, block_size=32, manifest_path=None):
    """Decode and validate one sample.

    Missing/corrupt image or manifest -> E_DECODE.
    Resolution / channel mismatch with manifest -> E_META naming the field.
    Illegal block size -> E_PARA.
    """
    if not os.path.exists(image_path):
        raise ForensicsError(
            E_DECODE, "image file is missing: %s" % image_path, field="image"
        )
    if manifest_path is None:
        manifest_path = image_path.rsplit(".", 1)[0] + ".manifest.json"
    manifest = _load_manifest(manifest_path)

    try:
        with Image.open(image_path) as pil:
            pil.load()
            mode = pil.mode
            decoded = np.asarray(pil)
    except Exception as exc:  # PIL raises a variety of decode errors
        raise ForensicsError(
            E_DECODE, "image decode failed: %s" % exc, field="image"
        )

    if mode == "L":
        pixels = decoded[:, :, np.newaxis]
    elif mode == "RGB":
        pixels = decoded
    else:
        raise ForensicsError(
            E_META,
            "unsupported image mode %r (only L and RGB are supported)" % mode,
            field="channels",
        )

    height, width = pixels.shape[0], pixels.shape[1]
    channels = pixels.shape[2]

    if int(manifest.get("width", -1)) != width:
        raise ForensicsError(
            E_META,
            "manifest width=%s but decoded width=%d"
            % (manifest.get("width"), width),
            field="width",
        )
    if int(manifest.get("height", -1)) != height:
        raise ForensicsError(
            E_META,
            "manifest height=%s but decoded height=%d"
            % (manifest.get("height"), height),
            field="height",
        )
    if int(manifest.get("channels", -1)) != channels:
        raise ForensicsError(
            E_META,
            "manifest channels=%s but decoded channels=%d"
            % (manifest.get("channels"), channels),
            field="channels",
        )

    if "tamper_mask" not in manifest or "primitives" not in manifest:
        missing = "tamper_mask" if "tamper_mask" not in manifest else "primitives"
        raise ForensicsError(
            E_META, "manifest missing required field %r" % missing, field=missing
        )

    return ImageSample(
        name=os.path.splitext(os.path.basename(image_path))[0],
        pixels=pixels,
        manifest=manifest,
        block_size=block_size,
    )


def load_dataset(sample_dir, block_size=32):
    """Load every sample referenced by ``index.json`` in ``sample_dir``."""
    index_path = os.path.join(sample_dir, "index.json")
    if not os.path.exists(index_path):
        raise ForensicsError(
            E_DECODE, "dataset index.json missing: %s" % index_path, field="index"
        )
    with open(index_path, "r", encoding="utf-8") as fh:
        index = json.load(fh)
    samples = []
    for image_file in index["samples"]:
        samples.append(
            load_sample(os.path.join(sample_dir, image_file), block_size=block_size)
        )
    return samples
