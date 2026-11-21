"""Color management chain.

Stages, in order:

1. spectral normalization (reflectance on the unified grid, illuminant
   normalized to Y = 1),
2. observer CMF integration to tristimulus values,
3. white point chromatic adaptation (Bradford / von Kries / none),
4. transform to each target color space (sRGB, Display-P3),
5. transfer-function encoding.

Every stage computes in float64 (declared precision 1e-12) and attaches an
error summary.  Non-finite tristimulus values or an unknown target color
space abort with E_COLOR_INVALID before any version is written.
"""

from __future__ import annotations

import numpy as np

from . import colorimetry as clr
from .canon import content_hash
from .errors import ErrorCode, SpectralStudioError
from .spectrum import unified_grid

DECLARED_PRECISION = 1e-12


class PipelineEnvironment:
    """Shared, hash-identified inputs to the chain."""

    def __init__(self, illuminant="D65", white_point="D65",
                 observer=clr.OBSERVER_1931_2, adaptation=clr.ADAPT_BRADFORD,
                 targets=(clr.SPACE_SRGB, clr.SPACE_DISPLAY_P3)):
        self.illuminant = illuminant
        self.white_point = white_point
        self.observer = observer
        self.adaptation = adaptation
        self.targets = tuple(targets)
        for space in self.targets:
            if space not in clr.KNOWN_SPACES:
                raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                          "unknown target color space",
                                          {"space": space})
        if observer not in clr.KNOWN_OBSERVERS:
            raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                      "unknown observer", {"observer": observer})
        if adaptation not in clr.KNOWN_ADAPTATIONS:
            raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                      "unknown adaptation strategy",
                                      {"adaptation": adaptation})

    def to_config(self):
        return {
            "illuminant": self.illuminant,
            "white_point": self.white_point,
            "observer": self.observer,
            "adaptation": self.adaptation,
            "targets": list(self.targets),
        }

    @classmethod
    def from_config(cls, config):
        return cls(
            illuminant=config.get("illuminant", "D65"),
            white_point=config.get("white_point", "D65"),
            observer=config.get("observer", clr.OBSERVER_1931_2),
            adaptation=config.get("adaptation", clr.ADAPT_BRADFORD),
            targets=tuple(config.get("targets",
                                     [clr.SPACE_SRGB, clr.SPACE_DISPLAY_P3])),
        )

    def dependency_factors(self):
        """Content-hash identity of every shared input."""
        src_white = clr.illuminant_white_xyz(self.illuminant, self.observer)
        dst_white = clr.white_point_xyz(self.white_point, self.observer)
        return {
            "illuminant": clr.illuminant_hash(self.illuminant),
            "white_point": clr.white_point_hash(self.white_point),
            "observer": self.observer,
            "adaptation": self.adaptation,
            "cmf": clr.cmf_hash(self.observer),
            "adapt_matrix": clr.adaptation_matrix_hash(self.adaptation,
                                                       src_white, dst_white),
            "targets": content_hash({"targets": list(self.targets)}),
        }


class SharedCaches:
    """Content-hash keyed caches for CMFs and adaptation matrices."""

    def __init__(self):
        self._cmf = {}
        self._adapt = {}
        self.stats = {"cmf_hit": 0, "cmf_miss": 0,
                      "adapt_hit": 0, "adapt_miss": 0}

    def cmf_for(self, observer):
        key = clr.cmf_hash(observer)
        if key not in self._cmf:
            self._cmf[key] = clr.color_matching_functions(observer)
            self.stats["cmf_miss"] += 1
        else:
            self.stats["cmf_hit"] += 1
        return self._cmf[key]

    def adaptation_for(self, strategy, src_white_xyz, dst_white_xyz):
        key = clr.adaptation_matrix_hash(strategy, src_white_xyz, dst_white_xyz)
        if key not in self._adapt:
            self._adapt[key] = clr.adaptation_matrix(strategy, src_white_xyz,
                                                     dst_white_xyz)
            self.stats["adapt_miss"] += 1
        else:
            self.stats["adapt_hit"] += 1
        return self._adapt[key]


def _finite3(name, vec):
    vec = np.asarray(vec, dtype=np.float64)
    if vec.shape != (3,) or not np.all(np.isfinite(vec)):
        raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                  f"non-finite tristimulus at stage {name}",
                                  {"stage": name})
    return vec


def run_chain(spec, env: PipelineEnvironment, caches: SharedCaches):
    """Run the full chain for one material; returns intermediates + encodings.

    Raises E_COLOR_INVALID on non-finite tristimulus values or unknown
    target color spaces; nothing is written by this function on failure.
    """
    grid = unified_grid()
    reflectance = np.asarray(spec.reflectance, dtype=np.float64)
    if reflectance.shape != grid.shape or not np.all(np.isfinite(reflectance)):
        raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                  "material reflectance not on unified grid",
                                  {"material_id": spec.material_id})

    # Stage 1: spectral normalization.
    illuminant = clr.illuminant_spd(env.illuminant)
    cmf = caches.cmf_for(env.observer)
    norm = np.trapezoid(illuminant * cmf[:, 1], grid)
    if norm <= 0.0 or not np.isfinite(norm):
        raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                  "illuminant normalization failed")
    norm_illuminant = illuminant / norm
    refl_max = float(np.max(reflectance))
    norm_summary = {
        "illuminant_y_norm": float(norm),
        "reflectance_max": refl_max,
        "error": abs(refl_max - float(np.max(spec.reflectance))),
    }

    # Stage 2: CMF integration to XYZ (perfect diffuser -> Y = 1).
    xyz = clr.integrate_xyz(reflectance, cmf, norm_illuminant)
    fine_grid = np.linspace(grid[0], grid[-1], 2 * grid.size - 1)
    fine = np.interp(fine_grid, grid, reflectance)
    fine_ill = np.interp(fine_grid, grid, norm_illuminant)
    fine_cmf = np.stack([np.interp(fine_grid, grid, cmf[:, i]) for i in range(3)],
                        axis=1)
    xyz_fine = np.trapezoid(fine[:, None] * fine_ill[:, None] * fine_cmf,
                            fine_grid, axis=0)
    integration_summary = {
        "xyz": [float(v) for v in xyz],
        "abs_error": [float(v) for v in np.abs(xyz - xyz_fine)],
    }
    _finite3("integration", xyz)

    # Stage 3: white point adaptation.
    src_white = clr.illuminant_white_xyz(env.illuminant, env.observer)
    dst_white = clr.white_point_xyz(env.white_point, env.observer)
    matrix = caches.adaptation_for(env.adaptation, src_white, dst_white)
    xyz_adapted = matrix @ xyz
    adapt_residual = float(np.max(np.abs(
        np.linalg.inv(matrix) @ matrix - np.eye(3))))
    adapt_summary = {
        "src_white": [float(v) for v in src_white],
        "dst_white": [float(v) for v in dst_white],
        "matrix_inverse_residual": adapt_residual,
    }
    _finite3("adaptation", xyz_adapted)

    # Stages 4+5: target space transform and transfer encoding.
    encoded = {}
    transform_summary = {}
    for space in env.targets:
        if space not in clr.KNOWN_SPACES:
            raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                      "unknown target color space",
                                      {"space": space})
        linear = clr.xyz_to_rgb_matrix(space) @ xyz_adapted
        _finite3(f"transform:{space}", linear)
        gamut_clip = float(max(0.0, -np.min(linear)))
        clipped = np.clip(linear, 0.0, 1.0)
        enc = np.clip(clr.encode_transfer(clipped, space), 0.0, 1.0)
        roundtrip = clr.decode_transfer(enc, space)
        transform_summary[space] = {
            "linear_rgb": [float(v) for v in linear],
            "gamut_clip_amount": gamut_clip,
            "encode_roundtrip_error": float(np.max(np.abs(roundtrip - clipped))),
        }
        encoded[space] = [float(v) for v in enc]

    intermediates = {
        "normalization": norm_summary,
        "integration": integration_summary,
        "adaptation": adapt_summary,
        "transform": transform_summary,
        "precision": DECLARED_PRECISION,
    }
    return {
        "material_id": spec.material_id,
        "xyz": [float(v) for v in xyz],
        "xyz_adapted": [float(v) for v in xyz_adapted],
        "encoded": encoded,
        "intermediates": intermediates,
    }
