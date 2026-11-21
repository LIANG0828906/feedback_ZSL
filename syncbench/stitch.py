"""Synchronized multichannel stitching with incremental recompute cache."""

from __future__ import annotations

import hashlib

import numpy as np

from .errors import E_STITCH_MISMATCH, SyncError
from .resample import output_length, resample


def content_hash(array):
    arr = np.ascontiguousarray(np.asarray(array, dtype=np.float64))
    return hashlib.sha256(arr.tobytes()).hexdigest()


def params_hash(offset, slope, mode, seg_len):
    key = f"{offset!r}|{slope!r}|{mode}|{seg_len}"
    return hashlib.sha256(key.encode()).hexdigest()


class SegmentCache:
    """Caches compensated segments keyed by content + parameter hashes."""

    def __init__(self):
        self._store = {}
        self.hits = 0
        self.misses = 0

    def key(self, channel, seg_idx, input_hash, p_hash):
        return (channel, seg_idx, input_hash, p_hash)

    def get(self, key):
        value = self._store.get(key)
        if value is None:
            self.misses += 1
        else:
            self.hits += 1
        return value

    def put(self, key, value):
        self._store[key] = value

    def stats(self):
        return {"hits": self.hits, "misses": self.misses, "entries": len(self._store)}


def compensate_segment(x, seg_out_start, seg_out_len, offset, slope, mode):
    """Compensate the output-index range of one segment."""
    n = np.arange(seg_out_start, seg_out_start + seg_out_len, dtype=np.float64)
    pos = offset + (1.0 + slope) * n
    lo = int(np.floor(pos[0])) - 32
    hi = int(np.ceil(pos[-1])) + 33
    lo = max(lo, 0)
    hi = min(hi, len(x))
    sub = x[lo:hi]
    return resample(sub, offset=pos[0] - lo, slope=slope, mode=mode, n_out=seg_out_len)


def compensate_channel(x, offset, slope, mode, seg_len, channel, input_hash, cache):
    """Compensate one channel segment-by-segment using the cache.

    Returns (array, segment_hashes, n_reused, n_recomputed).
    """
    n_out = output_length(len(x), offset, slope)
    p_hash = params_hash(offset, slope, mode, seg_len)
    pieces = []
    hashes = []
    reused = 0
    recomputed = 0
    seg_idx = 0
    for start in range(0, n_out, seg_len):
        seg_out_len = min(seg_len, n_out - start)
        key = cache.key(channel, seg_idx, input_hash, p_hash)
        cached = cache.get(key)
        if cached is not None:
            piece, c_hash = cached
            reused += 1
        else:
            piece = compensate_segment(x, start, seg_out_len, offset, slope, mode)
            c_hash = content_hash(piece)
            cache.put(key, (piece, c_hash))
            recomputed += 1
        pieces.append(piece)
        hashes.append(c_hash)
        seg_idx += 1
    joined = np.concatenate(pieces) if pieces else np.zeros(0)
    return joined, hashes, reused, recomputed


def stitch_channels(compensated):
    """Stack compensated channels into an aligned (channels, samples) matrix.

    All channels are truncated to the common minimum length so sample
    alignment and equal totals are guaranteed. Any inconsistency (empty
    channel, non-finite data) aborts the whole stitch: the caller then
    writes nothing for any channel.
    """
    if not compensated:
        raise SyncError(E_STITCH_MISMATCH, "no compensated channels to stitch")
    lengths = [len(c) for c in compensated]
    if min(lengths) <= 0:
        raise SyncError(
            E_STITCH_MISMATCH, "empty compensated channel", {"lengths": lengths}
        )
    target = min(lengths)
    matrix = np.zeros((len(compensated), target), dtype=np.float64)
    for i, ch in enumerate(compensated):
        if not np.all(np.isfinite(ch)):
            raise SyncError(E_STITCH_MISMATCH, f"non-finite samples in channel {i}")
        matrix[i] = ch[:target]
    return matrix
