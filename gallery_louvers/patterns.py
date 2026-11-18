"""Deterministic perforation patterns and brushed-microsurface lookup tables."""

from __future__ import annotations

import hashlib
import json
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .scene import Leaf, Scene
from .util import (
    canonical_json_bytes,
    hash_unit,
    hash_unit_array,
    sha256_bytes,
    smoothstep,
    stable_u64,
)

ATLAS_MAGIC = b"GLVPAT1\n"
ATLAS_VERSION = 1


@dataclass(frozen=True)
class LeafPattern:
    layer: int
    leaf: int
    seed: int
    aperture: np.ndarray
    microsurface: np.ndarray
    hole_centers: np.ndarray

    @property
    def key(self) -> tuple[int, int]:
        return (self.layer, self.leaf)


@dataclass(frozen=True)
class PatternSet:
    patterns: dict[tuple[int, int], LeafPattern]
    digest: str
    atlas_bytes: bytes

    def __getitem__(self, key: tuple[int, int]) -> LeafPattern:
        return self.patterns[key]

    @property
    def ordered(self) -> tuple[LeafPattern, ...]:
        return tuple(self.patterns[key] for key in sorted(self.patterns))

    def sample_aperture(self, layer: int, leaf: int, u: np.ndarray, v: np.ndarray) -> np.ndarray:
        pattern = self.patterns[(layer, leaf)]
        return _bilinear_clamped(pattern.aperture, u, v)

    def sample_microsurface(
        self, layer: int, leaf: int, u: np.ndarray, v: np.ndarray
    ) -> np.ndarray:
        pattern = self.patterns[(layer, leaf)]
        return _bilinear_clamped(pattern.microsurface, u, v)


def _bilinear_clamped(image: np.ndarray, u: np.ndarray, v: np.ndarray) -> np.ndarray:
    image = np.asarray(image)
    height, width = image.shape[:2]
    u_arr = np.clip(np.asarray(u, dtype=np.float64), 0.0, 1.0)
    v_arr = np.clip(np.asarray(v, dtype=np.float64), 0.0, 1.0)
    x = u_arr * (width - 1)
    y = v_arr * (height - 1)
    x0 = np.floor(x).astype(np.int64)
    y0 = np.floor(y).astype(np.int64)
    x1 = np.minimum(x0 + 1, width - 1)
    y1 = np.minimum(y0 + 1, height - 1)
    fx = x - x0
    fy = y - y0
    if image.ndim == 3:
        fx = fx[..., None]
        fy = fy[..., None]
    top = image[y0, x0] * (1.0 - fx) + image[y0, x1] * fx
    bottom = image[y1, x0] * (1.0 - fx) + image[y1, x1] * fx
    return top * (1.0 - fy) + bottom * fy


def _pattern_parameters(seed: int, layer: int, leaf: int, domain: str) -> dict[str, float]:
    domain_hash = int.from_bytes(hashlib.sha256(domain.encode("utf-8")).digest()[:8], "little")
    h0 = stable_u64(seed, layer, leaf, domain_hash, 1)
    h1 = stable_u64(seed, layer, leaf, domain_hash, 2)
    h2 = stable_u64(seed, layer, leaf, domain_hash, 3)
    h3 = stable_u64(seed, layer, leaf, domain_hash, 4)
    h4 = stable_u64(seed, layer, leaf, domain_hash, 5)
    h5 = stable_u64(seed, layer, leaf, domain_hash, 6)
    h6 = stable_u64(seed, layer, leaf, domain_hash, 7)
    cols = 3 + int(h0 % 2)
    rows = 2 + int(h1 % 2)
    petals = 5 + int(h2 % 3)
    phase_x = (h3 >> 11) / float(1 << 53)
    phase_y = (h4 >> 11) / float(1 << 53)
    rotation = ((h5 >> 11) / float(1 << 53)) * (np.pi / 2.0)
    petal_phase = ((h6 >> 11) / float(1 << 53)) * (2.0 * np.pi)
    cell_rx = 0.39 + ((h1 >> 17) % 7) * 0.005
    cell_ry = 0.37 + ((h2 >> 19) % 7) * 0.005
    aperture_floor = 0.045 + ((h4 >> 21) % 5) * 0.004
    return {
        "cols": float(cols),
        "rows": float(rows),
        "petals": float(petals),
        "phase_x": float(phase_x),
        "phase_y": float(phase_y),
        "rotation": float(rotation),
        "petal_phase": float(petal_phase),
        "cell_rx": float(cell_rx),
        "cell_ry": float(cell_ry),
        "aperture_floor": float(aperture_floor),
        "seed": float(seed & 0x7FFFFFFF),
    }


def generate_leaf_pattern(
    seed: int,
    layer: int,
    leaf: int,
    pattern_shape: tuple[int, int],
    micro_shape: tuple[int, int],
    domain: str,
) -> LeafPattern:
    """Generate one leaf solely from its seed/id pair and declared resolutions."""

    height, width = (int(pattern_shape[0]), int(pattern_shape[1]))
    rows_micro, cols_micro = (int(micro_shape[0]), int(micro_shape[1]))
    params = _pattern_parameters(seed, layer, leaf, domain)
    yy, xx = np.mgrid[0:height, 0:width]
    u = (xx.astype(np.float64) + 0.5) / width
    v = (yy.astype(np.float64) + 0.5) / height
    fx = (u * params["cols"] + params["phase_x"]) % 1.0 - 0.5
    fy = (v * params["rows"] + params["phase_y"]) % 1.0 - 0.5
    ca = np.cos(params["rotation"])
    sa = np.sin(params["rotation"])
    xr = ca * fx - sa * fy
    yr = sa * fx + ca * fy
    radius = np.sqrt(
        (xr / params["cell_rx"]) ** 2 + (yr / params["cell_ry"]) ** 2
    )
    theta = np.arctan2(yr, xr)
    rosette = (
        0.620
        + 0.068 * np.cos(params["petals"] * theta + params["petal_phase"])
        + 0.018 * np.cos((params["petals"] - 2.0) * theta - 0.6 * params["petal_phase"])
    )
    hole = 1.0 - smoothstep(rosette - 0.016, rosette + 0.016, radius)
    ring_radius = rosette + 0.085
    ring = (
        1.0
        - smoothstep(ring_radius - 0.009, ring_radius + 0.009, radius)
    ) * smoothstep(ring_radius - 0.018, ring_radius - 0.009, radius)
    fine_hash = hash_unit_array(
        xx.astype(np.int64) // 3,
        yy.astype(np.int64) // 3,
        stable_u64(seed, layer, leaf, 71),
    )
    etched = (fine_hash - 0.5) * 0.018
    border_u = np.minimum(u, 1.0 - u)
    border_v = np.minimum(v, 1.0 - v)
    border = smoothstep(0.012, 0.050, np.minimum(border_u, border_v))
    aperture = params["aperture_floor"] + 0.905 * hole + 0.065 * ring + etched
    aperture = np.clip((aperture - (1.0 - border) * 0.028), 0.0, 1.0)
    aperture32 = np.ascontiguousarray(aperture, dtype=np.float32)

    my, mx = np.mgrid[0:rows_micro, 0:cols_micro]
    mu = (mx.astype(np.float64) + 0.5) / cols_micro
    mv = (my.astype(np.float64) + 0.5) / rows_micro
    base_seed = stable_u64(seed, layer, leaf, 1231)
    grid_u = np.floor(mu * max(cols_micro - 1, 1)).astype(np.int64)
    grid_v = np.floor(mv * max(rows_micro - 1, 1)).astype(np.int64)
    noise_x = hash_unit_array(grid_u, grid_v, base_seed) - 0.5
    noise_y = hash_unit_array(grid_v, grid_u, base_seed + 17) - 0.5
    along = np.sin(
        2.0 * np.pi * (mv * (5.0 + int(stable_u64(base_seed, 5) % 4)) + 0.13)
    )
    across = np.sin(
        2.0 * np.pi * (mu * (2.0 + int(stable_u64(base_seed, 9) % 3)) + 0.37)
    )
    micro_x = 0.34 * noise_x + 0.11 * along
    micro_y = 0.27 * noise_y + 0.07 * across
    microsurface = np.stack([micro_x, micro_y], axis=-1)
    microsurface32 = np.ascontiguousarray(microsurface, dtype=np.float32)

    centers: list[list[float]] = []
    for row in range(int(params["rows"])):
        for column in range(int(params["cols"])):
            cx = (column + 0.5 - params["phase_x"]) / params["cols"]
            cy = (row + 0.5 - params["phase_y"]) / params["rows"]
            if 0.0 <= cx <= 1.0 and 0.0 <= cy <= 1.0:
                centers.append([float(cx), float(cy)])
    centers_arr = np.asarray(centers, dtype=np.float64)
    return LeafPattern(
        layer=layer,
        leaf=leaf,
        seed=seed,
        aperture=aperture32,
        microsurface=microsurface32,
        hole_centers=centers_arr,
    )


def build_patterns(scene: Scene) -> PatternSet:
    domain = scene.render.seed_domain
    pshape = scene.render.pattern_resolution
    mshape = scene.render.microsurface_resolution
    patterns: dict[tuple[int, int], LeafPattern] = {}
    for leaf in sorted(scene.leaves, key=lambda item: item.key):
        patterns[(leaf.layer, leaf.leaf)] = generate_leaf_pattern(
            leaf.seed,
            leaf.layer,
            leaf.leaf,
            pshape,
            mshape,
            domain,
        )
    atlas = serialize_atlas(patterns)
    return PatternSet(
        patterns=patterns,
        digest=sha256_bytes(atlas),
        atlas_bytes=atlas,
    )


def pattern_manifest(scene: Scene, patterns: PatternSet) -> dict[str, Any]:
    entries = []
    for pattern in patterns.ordered:
        entries.append(
            {
                "layer": pattern.layer,
                "leaf": pattern.leaf,
                "seed": pattern.seed,
                "aperture_shape": list(pattern.aperture.shape),
                "microsurface_shape": list(pattern.microsurface.shape),
                "hole_center_count": int(pattern.hole_centers.shape[0]),
                "aperture_sha256": sha256_bytes(pattern.aperture.tobytes(order="C")),
                "microsurface_sha256": sha256_bytes(
                    pattern.microsurface.tobytes(order="C")
                ),
            }
        )
    return {
        "seed_domain": scene.render.seed_domain,
        "pattern_resolution": list(scene.render.pattern_resolution),
        "microsurface_resolution": list(scene.render.microsurface_resolution),
        "entries": entries,
    }


def serialize_atlas(patterns: dict[tuple[int, int], LeafPattern]) -> bytes:
    payloads: list[bytes] = []
    entry_headers: list[dict[str, Any]] = []
    offset = 0
    for key in sorted(patterns):
        pattern = patterns[key]
        aperture = np.ascontiguousarray(pattern.aperture.astype("<f4", copy=False))
        micro = np.ascontiguousarray(pattern.microsurface.astype("<f4", copy=False))
        centers = np.ascontiguousarray(pattern.hole_centers.astype("<f8", copy=False))
        a_bytes = aperture.tobytes(order="C")
        m_bytes = micro.tobytes(order="C")
        c_bytes = centers.tobytes(order="C")
        payload = a_bytes + m_bytes + c_bytes
        payloads.append(payload)
        entry_headers.append(
            {
                "layer": pattern.layer,
                "leaf": pattern.leaf,
                "seed": pattern.seed,
                "aperture_shape": list(aperture.shape),
                "microsurface_shape": list(micro.shape),
                "hole_centers_shape": list(centers.shape),
                "offset": offset,
                "nbytes": len(payload),
                "aperture_nbytes": len(a_bytes),
                "microsurface_nbytes": len(m_bytes),
                "hole_centers_nbytes": len(c_bytes),
                "sha256": sha256_bytes(payload),
            }
        )
        offset += len(payload)
    header = {
        "version": ATLAS_VERSION,
        "dtype": "<f4/<f8",
        "entries": entry_headers,
        "payload_nbytes": offset,
    }
    header_bytes = canonical_json_bytes(header)
    return (
        ATLAS_MAGIC
        + struct.pack("<Q", len(header_bytes))
        + header_bytes
        + b"".join(payloads)
    )


def read_atlas_header(atlas_bytes: bytes) -> dict[str, Any]:
    if not atlas_bytes.startswith(ATLAS_MAGIC) or len(atlas_bytes) < len(ATLAS_MAGIC) + 8:
        raise ValueError("not a GLV pattern atlas")
    cursor = len(ATLAS_MAGIC)
    (header_size,) = struct.unpack("<Q", atlas_bytes[cursor : cursor + 8])
    cursor += 8
    header_end = cursor + int(header_size)
    header = json.loads(atlas_bytes[cursor:header_end].decode("utf-8"))
    payload = atlas_bytes[header_end:]
    if len(payload) != header["payload_nbytes"]:
        raise ValueError("atlas payload size mismatch")
    for entry in header["entries"]:
        chunk = payload[entry["offset"] : entry["offset"] + entry["nbytes"]]
        if sha256_bytes(chunk) != entry["sha256"]:
            raise ValueError("atlas entry checksum mismatch")
    return header


def write_pattern_atlas(path: str | Path, patterns: PatternSet) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(patterns.atlas_bytes)


def extract_pattern_arrays(
    atlas_bytes: bytes, layer: int, leaf: int
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Reference decoder used by tests and external technical review."""

    cursor = len(ATLAS_MAGIC)
    (header_size,) = struct.unpack("<Q", atlas_bytes[cursor : cursor + 8])
    cursor += 8
    header = json.loads(
        atlas_bytes[cursor : cursor + int(header_size)].decode("utf-8")
    )
    payload = atlas_bytes[cursor + int(header_size) :]
    for entry in header["entries"]:
        if entry["layer"] == layer and entry["leaf"] == leaf:
            chunk = payload[entry["offset"] : entry["offset"] + entry["nbytes"]]
            offset = 0
            aperture = np.frombuffer(
                chunk[offset : offset + entry["aperture_nbytes"]],
                dtype="<f4",
            ).reshape(entry["aperture_shape"])
            offset += entry["aperture_nbytes"]
            micro = np.frombuffer(
                chunk[offset : offset + entry["microsurface_nbytes"]],
                dtype="<f4",
            ).reshape(entry["microsurface_shape"])
            offset += entry["microsurface_nbytes"]
            centers = np.frombuffer(
                chunk[offset : offset + entry["hole_centers_nbytes"]],
                dtype="<f8",
            ).reshape(entry["hole_centers_shape"])
            return aperture.copy(), micro.copy(), centers.copy()
    raise KeyError(f"pattern ({layer}, {leaf}) not found")
