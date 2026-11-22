"""Deterministic block content hashing and granular fingerprints.

All numeric operations here are pure NumPy and depend only on pixel bytes and
the configured parameters -- never on thread identity, block processing order,
or global mutable RNG state -- so the same block always yields identical hash
and fingerprint regardless of how the scheduler runs it.
"""

import hashlib

import numpy as np

from .errors import ForensicsError, E_PARA


def content_hash(block, algorithm="sha256"):
    """Stable content hash over a block's raw byte representation."""
    if algorithm not in ("sha256", "sha1", "md5"):
        raise ForensicsError(
            E_PARA,
            "unsupported hash_algorithm %r" % algorithm,
            field="hash_algorithm",
        )
    hasher = hashlib.new(algorithm)
    hasher.update(np.ascontiguousarray(block, dtype=np.uint8).tobytes())
    return hasher.hexdigest()


def _mean_pool(block, granularity):
    """Average-pool a block to a ``granularity x granularity`` cell grid.

    Cells are split deterministically by index ranges (edge cells may be
    smaller), then each cell is mean-pooled per channel.  This is order and
    thread independent.
    """
    height, width, channels = block.shape
    if granularity <= 0 or granularity > min(height, width):
        raise ForensicsError(
            E_PARA,
            "fingerprint_granularity %d does not fit block %dx%d"
            % (granularity, width, height),
            field="fingerprint_granularity",
        )
    y_edges = np.linspace(0, height, granularity + 1, dtype=int)
    x_edges = np.linspace(0, width, granularity + 1, dtype=int)
    pooled = np.zeros((granularity, granularity, channels), dtype=np.float64)
    block_f = block.astype(np.float64)
    for iy in range(granularity):
        for ix in range(granularity):
            cell = block_f[y_edges[iy] : y_edges[iy + 1], x_edges[ix] : x_edges[ix + 1]]
            pooled[iy, ix] = cell.reshape(-1, channels).mean(axis=0)
    return pooled


def fingerprint(block, granularity=8):
    """Granular content fingerprint.

    Returns a deterministic dict with the pooled grid (rounded to a fixed
    precision so it serialises cleanly) plus a short digest.  The digest
    couples the pooled values with their sign pattern for a compact key.
    """
    pooled = _mean_pool(block, granularity)
    quantized = np.round(pooled, 4)
    # Sign-of-difference structure captures texture independent of brightness.
    diff_x = np.sign(np.diff(quantized, axis=1))
    digest_payload = quantized.tobytes() + diff_x.tobytes()
    digest = hashlib.sha256(digest_payload).hexdigest()
    return {
        "granularity": granularity,
        "pooled": quantized.tolist(),
        "digest": digest,
    }


def median_filter2d(plane, window):
    """Edge-padded median filter for a 2-D float plane, pure NumPy.

    Uses a single strided view instead of a Python double loop, so the window
    stack is built in C while remaining fully deterministic.
    """
    if window <= 0 or window % 2 == 0:
        raise ForensicsError(
            E_PARA,
            "median window must be a positive odd integer, got %r" % window,
            field="window",
        )
    radius = window // 2
    padded = np.pad(plane, radius, mode="edge")
    height, width = plane.shape
    strides = padded.strides * 2
    windows = np.lib.stride_tricks.as_strided(
        padded,
        shape=(height, width, window, window),
        strides=strides,
        writeable=False,
    )
    flat = windows.reshape(height, width, window * window)
    # Odd window -> median is the central order statistic; partition avoids a
    # full sort and is roughly 2x faster while remaining element-wise identical.
    middle = (window * window) // 2
    return np.partition(flat, middle, axis=2)[:, :, middle]


def uniform_filter2d(plane, window):
    """Edge-padded box mean filter built from one strided view."""
    if window <= 0 or window % 2 == 0:
        raise ForensicsError(
            E_PARA,
            "box window must be a positive odd integer, got %r" % window,
            field="window",
        )
    radius = window // 2
    padded = np.pad(plane.astype(np.float64), radius, mode="edge")
    height, width = plane.shape
    windows = np.lib.stride_tricks.as_strided(
        padded,
        shape=(height, width, window, window),
        strides=padded.strides * 2,
        writeable=False,
    )
    return windows.reshape(height, width, window * window).mean(axis=2)
