"""Colour management chain (requirement 2).

Chain order: spectral normalisation -> observer CMF integration ->
white-point adaptation -> target RGB transform -> transfer encoding.
Every intermediate is tagged with a content hash.
All reductions run in fixed array order so results are byte-identical
regardless of thread count or completion order.
"""
from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass

import numpy as np

from .config import BakeConfig
from .spectrum import Material

# --- analytic CIE 1931 2-deg CMF fit (Wyman, Sloan, Shirley 2013) ---
_CMF_LOBES = {
    "x": ((442.0, 16.0, 26.7, 0.362), (599.8, 37.9, 31.0, 1.056), (501.1, 20.4, 26.2, -0.065)),
    "y": ((568.8, 46.9, 40.5, 0.821), (530.9, 16.3, 31.1, 0.286)),
    "z": ((437.0, 11.8, 36.0, 1.217), (459.0, 26.0, 13.8, 0.681)),
}

# XYZ -> linear RGB matrices (D65)
_XYZ_TO_RGB = {
    "srgb": np.array(
        [
            [3.2406, -1.5372, -0.4986],
            [-0.9689, 1.8758, 0.0415],
            [0.0557, -0.2040, 1.0570],
        ],
        dtype=np.float64,
    ),
    "display-p3": np.array(
        [
            [2.4934969, -0.9313836, -0.4027107],
            [-0.8294890, 1.7626640, 0.0236247],
            [0.0358458, -0.0761724, 0.9568845],
        ],
        dtype=np.float64,
    ),
}

_RGB_TO_XYZ = {
    "srgb": np.linalg.inv(_XYZ_TO_RGB["srgb"]),
    "display-p3": np.linalg.inv(_XYZ_TO_RGB["display-p3"]),
}

_ADAPTATION_MATRICES = {
    "bradford": np.array(
        [[0.8951, 0.2664, -0.1614], [-0.7502, 1.7135, 0.0367], [0.0389, -0.0685, 1.0296]],
        dtype=np.float64,
    ),
    "cat02": np.array(
        [[0.7328, 0.4296, -0.1624], [-0.7036, 1.6975, 0.0061], [0.0030, 0.0136, 0.9834]],
        dtype=np.float64,
    ),
}

# nominal CCT for synthetic blackbody illuminants
_ILLUMINANT_CCT = {"D65": 6504.0, "D50": 5003.0, "A": 2856.0, "E": None}


def content_hash(*parts: bytes) -> str:
    h = hashlib.sha256()
    for part in parts:
        h.update(part)
    return h.hexdigest()


def array_hash(array: np.ndarray, tag: str) -> str:
    arr = np.ascontiguousarray(np.asarray(array, dtype=np.float64))
    return content_hash(tag.encode("utf-8"), repr(arr.shape).encode(), arr.tobytes())


def _gauss(wl: np.ndarray, mu: float, s1: float, s2: float) -> np.ndarray:
    sigma = np.where(wl < mu, s1, s2)
    t = (wl - mu) / sigma
    return np.exp(-0.5 * t * t)


def observer_cmf(observer: str, grid_wl: np.ndarray) -> np.ndarray:
    """Return (N, 3) colour matching functions, columns X, Y, Z."""
    if observer != "cie1931_2deg":
        raise ValueError(f"unsupported observer: {observer}")
    cols = []
    for key in ("x", "y", "z"):
        total = np.zeros_like(grid_wl)
        for mu, s1, s2, amp in _CMF_LOBES[key]:
            total = total + amp * _gauss(grid_wl, mu, s1, s2)
        cols.append(total)
    return np.stack(cols, axis=1)


def _planck(grid_wl: np.ndarray, cct: float) -> np.ndarray:
    lam = grid_wl * 1e-9
    c1 = 3.741771852e-16
    c2 = 1.438776877e-2
    s = c1 / (lam**5 * (np.exp(c2 / (lam * cct)) - 1.0))
    return s / np.max(s)


def illuminant_spectrum(name: str, grid_wl: np.ndarray, custom: dict | None = None) -> np.ndarray:
    if name == "E":
        return np.ones_like(grid_wl)
    if name in _ILLUMINANT_CCT and _ILLUMINANT_CCT[name]:
        return _planck(grid_wl, _ILLUMINANT_CCT[name])
    if name == "custom":
        wl = np.asarray(custom["wavelengths_nm"], dtype=np.float64)
        power = np.asarray(custom["power"], dtype=np.float64)
        return np.interp(grid_wl, wl, power)
    raise ValueError(f"unsupported illuminant: {name}")


def adaptation_transform(method: str, src_wp: np.ndarray, dst_wp: np.ndarray) -> np.ndarray:
    if method == "none" or np.array_equal(src_wp, dst_wp):
        return np.eye(3)
    m = _ADAPTATION_MATRICES[method]
    m_inv = np.linalg.inv(m)
    src_lms = m @ src_wp
    dst_lms = m @ dst_wp
    return m_inv @ np.diag(dst_lms / src_lms) @ m


def srgb_transfer_encode(linear: np.ndarray) -> np.ndarray:
    linear = np.clip(linear, 0.0, 1.0)
    return np.where(linear <= 0.0031308, 12.92 * linear, 1.055 * linear ** (1.0 / 2.4) - 0.055)


def encode_codes(linear_rgb: np.ndarray) -> list[int]:
    """Deterministic 8-bit quantisation (round half up)."""
    encoded = srgb_transfer_encode(np.asarray(linear_rgb, dtype=np.float64))
    return [int(v) for v in np.floor(encoded * 255.0 + 0.5).clip(0, 255)]


def entry_content_hash(material_id: str, param_hash: str, codes: dict) -> str:
    """Content hash of one baked sphere; recomputed during manifest validation."""
    return content_hash(
        material_id.encode(),
        param_hash.encode(),
        *[content_hash(t.encode(), bytes(codes[t])).encode() for t in sorted(codes)],
    )


@dataclass
class ChainContext:
    """Precomputed, hash-tagged chain state shared by all materials in a batch."""

    grid_wl: np.ndarray
    cmf: np.ndarray
    illuminant: np.ndarray
    norm_k: float
    src_wp: np.ndarray
    dst_wp: np.ndarray
    adapt_matrix: np.ndarray
    step_hashes: dict


def build_chain_context(config: BakeConfig, illuminant_override: dict | None = None) -> ChainContext:
    grid = config.grid
    grid_wl = grid.start_nm + grid.step_nm * np.arange(grid.sample_count, dtype=np.float64)
    cmf = observer_cmf(config.observer, grid_wl)
    illum = illuminant_spectrum(config.white_point, grid_wl, illuminant_override)
    dlam = grid.step_nm
    # spectral normalisation reference: k scales perfect white to Y=100
    norm_k = 100.0 / float(np.sum(illum * cmf[:, 1]) * dlam)
    src_wp = norm_k * (cmf.T @ illum) * dlam  # illuminant XYZ = source white point
    target_wp = norm_k * (cmf.T @ illuminant_spectrum("D65", grid_wl)) * dlam
    adapt = adaptation_transform(config.adaptation, src_wp, target_wp)
    step_hashes = {
        "grid": array_hash(grid_wl, "grid"),
        "cmf": array_hash(cmf, f"cmf:{config.observer}"),
        "illuminant": array_hash(illum, f"illuminant:{config.white_point}"),
        "adaptation": array_hash(adapt, f"adapt:{config.adaptation}"),
    }
    return ChainContext(
        grid_wl=grid_wl,
        cmf=cmf,
        illuminant=illum,
        norm_k=norm_k,
        src_wp=src_wp,
        dst_wp=target_wp,
        adapt_matrix=adapt,
        step_hashes=step_hashes,
    )


def material_param_hash(material: Material) -> str:
    canonical = json.dumps(material.raw, sort_keys=True, separators=(",", ":"))
    return content_hash(b"material:", canonical.encode("utf-8"))


def global_param_hash(config: BakeConfig) -> str:
    payload = {
        "observer": config.observer,
        "white_point": config.white_point,
        "adaptation": config.adaptation,
        "targets": list(config.targets),
        "grid": [config.grid.start_nm, config.grid.end_nm, config.grid.step_nm],
    }
    canonical = json.dumps(payload, sort_keys=True, separators=(",", ":"))
    return content_hash(b"global:", canonical.encode("utf-8"))


def material_xyz(material: Material, ctx: ChainContext) -> tuple[np.ndarray, dict]:
    """Run normalisation + integration for one material; return XYZ and step hashes."""
    dlam = float(ctx.grid_wl[1] - ctx.grid_wl[0]) if ctx.grid_wl.size > 1 else 1.0
    if material.type == "spectral":
        refl = material.reflectance
        norm_hash = array_hash(refl, f"norm:{material.norm_reference!r}")
        radiance = refl * ctx.illuminant
        xyz = ctx.norm_k * (ctx.cmf.T @ radiance) * dlam
    else:
        xyz = _RGB_TO_XYZ[material.type] @ np.asarray(material.rgb, dtype=np.float64) * 100.0
        norm_hash = array_hash(np.asarray(material.rgb), f"norm-rgb:{material.type}")
    integ_hash = array_hash(xyz, "xyz-integrated")
    return xyz, {"normalisation": norm_hash, "integration": integ_hash}


def bake_material(material: Material, ctx: ChainContext, config: BakeConfig) -> dict:
    """Full chain for one material sphere. Pure function of its inputs."""
    xyz, step_hashes = material_xyz(material, ctx)
    adapted = ctx.adapt_matrix @ xyz
    step_hashes["adaptation"] = array_hash(adapted, "xyz-adapted")
    codes = {}
    for target in config.targets:
        linear = _XYZ_TO_RGB[target] @ (adapted / 100.0)
        step_hashes[f"transform:{target}"] = array_hash(linear, f"rgb-linear:{target}")
        codes[target] = encode_codes(linear)
        step_hashes[f"encoding:{target}"] = content_hash(
            f"codes:{target}".encode(), bytes(codes[target])
        )
    param_hash = material_param_hash(material)
    entry_hash = entry_content_hash(material.material_id, param_hash, codes)
    return {
        "material_id": material.material_id,
        "material_index": material.material_index,
        "material_param_hash": param_hash,
        "codes": codes,
        "step_hashes": step_hashes,
        "content_hash": entry_hash,
    }


def max_code_delta(old_entries: list[dict], new_entries: list[dict]) -> int:
    """Max absolute code-value delta between two published mappings."""
    old_by_id = {e["material_id"]: e["codes"] for e in old_entries}
    delta = 0
    for entry in new_entries:
        old = old_by_id.get(entry["material_id"])
        if not old:
            continue
        for target, codes in entry["codes"].items():
            if target in old:
                diff = max(abs(a - b) for a, b in zip(codes, old[target]))
                delta = max(delta, diff)
    return delta
