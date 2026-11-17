"""Deterministic heightfield generation from an integer seed.

Heights come from value noise: lattice corner values are produced by a
self-implemented 64-bit integer hash, then smoothly interpolated with the
classic smoothstep curve. All randomness derives from integer arithmetic, so
identical parameters always produce byte-identical results.
"""

import hashlib

import numpy as np

from .hashing import hash_to_unit, lattice_hash


def generate_heights(seed, verts_x, verts_y, feature=8, octaves=3):
    """Generate a (verts_y, verts_x) float64 heightfield in [0, 1).

    `feature` is the lattice spacing (in vertices) of the first octave; each
    subsequent octave doubles the frequency and halves the amplitude.
    """
    if verts_x < 2 or verts_y < 2:
        raise ValueError("heightfield needs at least 2x2 vertices")
    heights = np.zeros((verts_y, verts_x), dtype=np.float64)
    amplitude = 1.0
    total_amplitude = 0.0
    spacing = float(feature)
    for octave in range(octaves):
        heights += amplitude * _value_noise_layer(
            seed + 0x9E3779B9 * (octave + 1), verts_x, verts_y, spacing
        )
        total_amplitude += amplitude
        amplitude *= 0.5
        spacing = max(spacing / 2.0, 1.0)
    return heights / total_amplitude


def _value_noise_layer(seed, verts_x, verts_y, spacing):
    # Lattice dimensions needed to cover the vertex grid.
    lat_x = int(np.ceil((verts_x - 1) / spacing)) + 2
    lat_y = int(np.ceil((verts_y - 1) / spacing)) + 2
    lattice = np.empty((lat_y, lat_x), dtype=np.float64)
    for ly in range(lat_y):
        for lx in range(lat_x):
            lattice[ly, lx] = hash_to_unit(lattice_hash(seed, lx, ly))

    xs = np.arange(verts_x, dtype=np.float64) / spacing
    ys = np.arange(verts_y, dtype=np.float64) / spacing
    x0 = np.minimum(xs.astype(np.int64), lat_x - 2)
    y0 = np.minimum(ys.astype(np.int64), lat_y - 2)
    fx = xs - x0
    fy = ys - y0
    # Smoothstep interpolation weights.
    sx = fx * fx * (3.0 - 2.0 * fx)
    sy = fy * fy * (3.0 - 2.0 * fy)

    v00 = lattice[np.ix_(y0, x0)]
    v10 = lattice[np.ix_(y0, x0 + 1)]
    v01 = lattice[np.ix_(y0 + 1, x0)]
    v11 = lattice[np.ix_(y0 + 1, x0 + 1)]
    top = v00 + (v10 - v00) * sx[None, :]
    bottom = v01 + (v11 - v01) * sx[None, :]
    return top + (bottom - top) * sy[:, None]


def heights_digest(heights):
    """Stable digest of a heightfield's raw float64 bytes."""
    return hashlib.sha256(np.ascontiguousarray(heights).tobytes()).hexdigest()
