from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Callable, Mapping, Sequence

import numpy as np

from .canonical import normalize_value, sha256_obj
from .errors import material_invalid, pipeline_uninit
from .models import (
    GridSpec,
    IlluminantSpec,
    PipelineConfig,
    SUPPORTED_ADAPTATIONS,
    SUPPORTED_OBSERVERS,
    SUPPORTED_TARGETS,
    thaw_value,
)


WHITE_POINTS: dict[str, tuple[float, float, float]] = {
    "D50": (0.96422, 1.0, 0.82521),
    "D55": (0.95682, 1.0, 0.92149),
    "D60": (0.95265, 1.0, 1.00883),
    "D65": (0.95047, 1.0, 1.08883),
    "D75": (0.94972, 1.0, 1.22638),
    "E": (1.0, 1.0, 1.0),
}

RGB_TO_XYZ = {
    "srgb": np.array(
        [
            [0.4124564, 0.3575761, 0.1804375],
            [0.2126729, 0.7151522, 0.0721750],
            [0.0193339, 0.1191920, 0.9503041],
        ],
        dtype=np.float64,
    ),
    "display-p3": np.array(
        [
            [0.4865709486482162, 0.26566769316909306, 0.1982172852343625],
            [0.2289745640697488, 0.6917385218365064, 0.079286914093745],
            [0.0, 0.04511338185890264, 1.043944368900976],
        ],
        dtype=np.float64,
    ),
}
XYZ_TO_RGB = {name: np.linalg.inv(matrix) for name, matrix in RGB_TO_XYZ.items()}

ADAPTATION_MATRICES = {
    "bradford": np.array(
        [
            [0.8951, 0.2664, -0.1614],
            [-0.7502, 1.7135, 0.0367],
            [0.0389, -0.0685, 1.0296],
        ],
        dtype=np.float64,
    ),
    "von-kries": np.array(
        [
            [0.40024, 0.70760, -0.08081],
            [-0.22630, 1.16532, 0.04570],
            [0.0, 0.0, 0.91822],
        ],
        dtype=np.float64,
    ),
}


def _as_array(values: Sequence[float], path: str) -> np.ndarray:
    array = np.asarray(values, dtype=np.float64)
    if array.ndim != 1:
        raise material_invalid(path, "must be a one-dimensional array")
    if not np.all(np.isfinite(array)):
        raise material_invalid(path, "must contain finite values")
    return array


def _piecewise_gaussian(
    wavelength: np.ndarray,
    center: float,
    left_width: float,
    right_width: float,
) -> np.ndarray:
    width = np.where(wavelength < center, left_width, right_width)
    return np.exp(-0.5 * np.square((wavelength - center) / width))


def analytic_observer(name: str, wavelengths: Sequence[float]) -> np.ndarray:
    """Deterministic multi-lobe approximation of CIE observer CMFs."""
    if name not in SUPPORTED_OBSERVERS:
        raise pipeline_uninit("pipeline.observer", f"unsupported observer {name!r}")
    wave = np.asarray(wavelengths, dtype=np.float64)
    if name == "2deg":
        x = (
            1.056 * _piecewise_gaussian(wave, 599.8, 37.9, 31.0)
            + 0.362 * _piecewise_gaussian(wave, 442.0, 16.0, 26.7)
            - 0.065 * _piecewise_gaussian(wave, 501.1, 20.4, 26.2)
        )
        y = (
            0.821 * _piecewise_gaussian(wave, 568.8, 46.9, 40.5)
            + 0.286 * _piecewise_gaussian(wave, 530.9, 16.3, 31.1)
        )
        z = (
            1.217 * _piecewise_gaussian(wave, 437.0, 11.8, 36.0)
            + 0.681 * _piecewise_gaussian(wave, 459.0, 26.0, 13.8)
        )
    else:
        x = (
            1.020 * _piecewise_gaussian(wave, 600.0, 38.5, 32.0)
            + 0.380 * _piecewise_gaussian(wave, 445.0, 17.0, 28.0)
            - 0.060 * _piecewise_gaussian(wave, 500.0, 20.5, 26.5)
        )
        y = (
            0.850 * _piecewise_gaussian(wave, 570.0, 48.0, 42.0)
            + 0.300 * _piecewise_gaussian(wave, 532.0, 17.0, 33.0)
        )
        z = (
            1.200 * _piecewise_gaussian(wave, 438.0, 12.0, 37.0)
            + 0.700 * _piecewise_gaussian(wave, 460.0, 27.0, 15.0)
        )
    return np.stack([np.clip(x, 0.0, None), np.clip(y, 0.0, None), np.clip(z, 0.0, None)], axis=1)


def planckian_spectrum(wavelengths: Sequence[float], temperature_k: float) -> np.ndarray:
    if not math.isfinite(temperature_k) or temperature_k <= 0:
        raise material_invalid(
            "pipeline.illuminant.temperature_k", "must be positive and finite"
        )
    wave = np.asarray(wavelengths, dtype=np.float64)
    if np.any(wave <= 0):
        raise material_invalid(
            "pipeline.illuminant.wavelengths_nm", "wavelengths must be positive"
        )
    meters = wave * 1e-9
    c2 = 1.438776877e-2
    with np.errstate(over="ignore"):
        radiance = 1.0 / (np.power(meters, 5.0) * np.expm1(c2 / (meters * temperature_k)))
    peak = float(np.max(radiance))
    if not math.isfinite(peak) or peak <= 0:
        raise material_invalid(
            "pipeline.illuminant.temperature_k", "cannot generate illuminant spectrum"
        )
    return radiance / peak


def generate_observer_document(name: str, grid: GridSpec) -> dict[str, Any]:
    wavelengths = np.asarray(grid.wavelengths(), dtype=np.float64)
    cmf = analytic_observer(name, wavelengths)
    return {
        "schema": "spectral-observer/v1",
        "observer": name,
        "wavelengths_nm": [round(float(item), 10) for item in wavelengths],
        "x": [round(float(item), 12) for item in cmf[:, 0]],
        "y": [round(float(item), 12) for item in cmf[:, 1]],
        "z": [round(float(item), 12) for item in cmf[:, 2]],
    }


def generate_illuminant_document(
    name: str, temperature_k: float, grid: GridSpec
) -> dict[str, Any]:
    wavelengths = np.asarray(grid.wavelengths(), dtype=np.float64)
    values = planckian_spectrum(wavelengths, temperature_k)
    return {
        "schema": "spectral-illuminant/v1",
        "name": name,
        "temperature_k": temperature_k,
        "wavelengths_nm": [round(float(item), 10) for item in wavelengths],
        "values": [round(float(item), 12) for item in values],
    }


def srgb_encode(linear: np.ndarray) -> np.ndarray:
    value = np.asarray(linear, dtype=np.float64)
    encoded = np.where(
        value <= 0.0031308,
        12.92 * value,
        1.055 * np.power(np.clip(value, 0.0, None), 1.0 / 2.4) - 0.055,
    )
    return np.clip(encoded, 0.0, 1.0)


def srgb_decode(encoded: np.ndarray) -> np.ndarray:
    value = np.asarray(encoded, dtype=np.float64)
    return np.where(
        value <= 0.04045,
        value / 12.92,
        np.power((value + 0.055) / 1.055, 2.4),
    )


def adaptation_matrix(
    strategy: str,
    source_white: Sequence[float],
    target_white: Sequence[float],
) -> np.ndarray:
    if strategy == "none":
        return np.identity(3, dtype=np.float64)
    if strategy not in ADAPTATION_MATRICES:
        raise pipeline_uninit(
            "pipeline.adaptation", f"unsupported adaptation {strategy!r}"
        )
    source = np.asarray(source_white, dtype=np.float64)
    target = np.asarray(target_white, dtype=np.float64)
    if source.shape != (3,) or target.shape != (3,):
        raise pipeline_uninit("pipeline.white_point", "white point must be XYZ")
    cone = ADAPTATION_MATRICES[strategy]
    source_cone = cone @ source
    target_cone = cone @ target
    if np.any(np.abs(source_cone) < 1e-12):
        raise pipeline_uninit(
            "pipeline.white_point", "source white cannot be adapted"
        )
    diagonal = target_cone / source_cone
    return np.linalg.inv(cone) @ np.diag(diagonal) @ cone


def _max_channel_delta(old: Mapping[str, Any], new: Mapping[str, Any]) -> dict[str, float]:
    result: dict[str, float] = {}
    for target in SUPPORTED_TARGETS:
        if target in old and target in new:
            old_values = np.asarray(old[target], dtype=np.float64)
            new_values = np.asarray(new[target], dtype=np.float64)
            result[target] = round(float(np.max(np.abs(old_values - new_values))), 12)
    return result


@dataclass(frozen=True)
class ChainResult:
    colors: Mapping[str, tuple[float, float, float]]
    intermediates: Mapping[str, Any]
    dependencies: Mapping[str, str]

    def to_dict(self) -> dict[str, Any]:
        return {
            "colors": {key: list(value) for key, value in self.colors.items()},
            "intermediates": thaw_value(self.intermediates),
            "dependencies": dict(self.dependencies),
        }


class ColorPipeline:
    """Color management chain with an explicit initialized/uninitialized state."""

    def __init__(self) -> None:
        self._initialized = False
        self._config: PipelineConfig | None = None
        self._observer_cache: dict[str, dict[str, np.ndarray]] = {}
        self._illuminant_cache: dict[str, tuple[np.ndarray, np.ndarray]] = {}

    @classmethod
    def from_config(cls, config: PipelineConfig) -> "ColorPipeline":
        pipeline = cls()
        pipeline.initialize(config)
        return pipeline

    @property
    def config(self) -> PipelineConfig:
        self.require_initialized()
        assert self._config is not None
        return self._config

    def require_initialized(self) -> None:
        if not self._initialized or self._config is None:
            raise pipeline_uninit("pipeline", "color pipeline is not initialized")
        if self._config.observer not in self._observer_cache:
            raise pipeline_uninit(
                "pipeline.observer", "observer color matching functions are unavailable"
            )

    def initialize(self, config: PipelineConfig) -> None:
        if not isinstance(config, PipelineConfig):
            raise pipeline_uninit("pipeline", "a valid PipelineConfig is required")
        self._config = config
        wavelengths = np.asarray(config.grid.wavelengths(), dtype=np.float64)
        if config.observer not in self._observer_cache:
            self._observer_cache[config.observer] = self._build_observer(
                config.observer, wavelengths
            )
        self._resolve_illuminant(config.illuminant, wavelengths)
        self._initialized = True

    def register_observer(
        self,
        name: str,
        wavelengths_nm: Sequence[float],
        x: Sequence[float],
        y: Sequence[float],
        z: Sequence[float],
    ) -> None:
        wavelengths = _as_array(wavelengths_nm, "observer.wavelengths_nm")
        xyz = np.stack(
            [
                _as_array(x, "observer.x"),
                _as_array(y, "observer.y"),
                _as_array(z, "observer.z"),
            ],
            axis=1,
        )
        if xyz.shape != (wavelengths.shape[0], 3):
            raise pipeline_uninit("observer", "observer arrays must have equal lengths")
        if np.any(np.diff(wavelengths) <= 0):
            raise pipeline_uninit(
                "observer.wavelengths_nm", "wavelengths must be strictly increasing"
            )
        self._observer_cache[name] = {"wavelengths": wavelengths, "xyz": xyz}

    def register_illuminant(
        self,
        spec: IlluminantSpec,
        wavelengths_nm: Sequence[float],
        values: Sequence[float],
    ) -> None:
        wavelengths = _as_array(wavelengths_nm, "illuminant.wavelengths_nm")
        radiance = _as_array(values, "illuminant.values")
        if wavelengths.shape != radiance.shape:
            raise pipeline_uninit(
                "illuminant.values", "length must match wavelengths_nm"
            )
        if np.any(np.diff(wavelengths) <= 0):
            raise pipeline_uninit(
                "illuminant.wavelengths_nm", "wavelengths must be strictly increasing"
            )
        if np.any(radiance < 0):
            raise pipeline_uninit("illuminant.values", "must be non-negative")
        self._illuminant_cache[_illuminant_key(spec)] = (wavelengths, radiance)

    def _build_observer(
        self, name: str, wavelengths: np.ndarray
    ) -> dict[str, np.ndarray]:
        if name not in SUPPORTED_OBSERVERS:
            raise pipeline_uninit("pipeline.observer", f"unsupported observer {name!r}")
        xyz = analytic_observer(name, wavelengths)
        if np.sum(xyz[:, 1]) <= 0:
            raise pipeline_uninit("pipeline.observer", "observer y is degenerate")
        return {"wavelengths": wavelengths, "xyz": xyz}

    def _resolve_illuminant(
        self, spec: IlluminantSpec, wavelengths: np.ndarray
    ) -> tuple[np.ndarray, np.ndarray]:
        key = _illuminant_key(spec)
        if key in self._illuminant_cache:
            return self._illuminant_cache[key]
        if spec.values is not None and spec.wavelengths_nm is not None:
            source_wave = np.asarray(spec.wavelengths_nm, dtype=np.float64)
            source_values = np.asarray(spec.values, dtype=np.float64)
            resampled = np.interp(wavelengths, source_wave, source_values)
        elif spec.temperature_k is not None:
            resampled = planckian_spectrum(wavelengths, float(spec.temperature_k))
        else:
            raise pipeline_uninit(
                "pipeline.illuminant",
                "illuminant needs either a temperature or wavelength/value samples",
            )
        if np.sum(resampled) <= 0:
            raise pipeline_uninit("pipeline.illuminant", "illuminant is degenerate")
        self._illuminant_cache[key] = (wavelengths.copy(), resampled)
        return self._illuminant_cache[key]

    def _observer(self, config: PipelineConfig) -> np.ndarray:
        try:
            cached = self._observer_cache[config.observer]
        except KeyError as exc:
            raise pipeline_uninit(
                "pipeline.observer", "observer is not initialized"
            ) from exc
        if not np.array_equal(
            cached["wavelengths"], np.asarray(config.grid.wavelengths(), dtype=np.float64)
        ):
            cached = self._build_observer(
                config.observer,
                np.asarray(config.grid.wavelengths(), dtype=np.float64),
            )
            self._observer_cache[config.observer] = cached
        return cached["xyz"]

    def _illuminant(self, config: PipelineConfig) -> np.ndarray:
        wavelengths = np.asarray(config.grid.wavelengths(), dtype=np.float64)
        _, values = self._resolve_illuminant(config.illuminant, wavelengths)
        return values

    def dependencies(
        self, params: Mapping[str, Any], config: PipelineConfig
    ) -> dict[str, str]:
        self.require_initialized()
        kind = str(params.get("kind"))
        dependencies = {
            "material": sha256_obj(params),
            "targets": sha256_obj(list(config.target_tuple)),
            "white_point": sha256_obj(
                {"name": config.white_point_name, "xyz": config.white_point_xyz}
            ),
            "adaptation": sha256_obj(config.adaptation),
        }
        if kind == "spectral":
            dependencies["observer"] = sha256_obj(config.observer)
            dependencies["illuminant"] = sha256_obj(config.illuminant.to_dict())
        elif kind != "display":
            raise material_invalid("materials.kind", f"unsupported material kind {kind!r}")
        return dependencies

    def compute(
        self,
        params: Mapping[str, Any],
        config: PipelineConfig | None = None,
        *,
        cancel_check: Callable[[], bool] | None = None,
    ) -> ChainResult:
        self.require_initialized()
        active_config = config or self.config
        if active_config is not self._config:
            self.initialize(active_config)
        dependencies = self.dependencies(params, active_config)
        kind = str(params.get("kind"))
        if cancel_check and cancel_check():
            from .errors import cancelled

            raise cancelled()
        if kind == "spectral":
            return self._compute_spectral(params, active_config, dependencies)
        if kind == "display":
            return self._compute_display(params, active_config, dependencies)
        raise material_invalid("materials.kind", f"unsupported material kind {kind!r}")

    def _compute_spectral(
        self,
        params: Mapping[str, Any],
        config: PipelineConfig,
        dependencies: Mapping[str, str],
    ) -> ChainResult:
        spectrum = params.get("spectrum")
        if not isinstance(spectrum, Mapping):
            raise material_invalid("materials.spectrum", "spectrum metadata is missing")
        values = spectrum.get("resampled_values")
        if not isinstance(values, (list, tuple)):
            raise material_invalid(
                "materials.spectrum.resampled_values", "normalized spectrum is missing"
            )
        reflectance = _as_array(values, "materials.spectrum.resampled_values")
        if reflectance.shape[0] != config.grid.sample_count:
            raise pipeline_uninit(
                "pipeline.grid",
                "normalized spectrum grid no longer matches the active pipeline grid",
            )
        if np.any(reflectance < 0.0) or np.any(reflectance > 1.0):
            raise material_invalid(
                "materials.spectrum.resampled_values",
                "reflectance must be within [0, 1]",
            )
        illuminant = self._illuminant(config)
        observer = self._observer(config)
        delta_nm = float(config.grid.step_nm)
        normalizer = float(np.sum(illuminant * observer[:, 1] * delta_nm))
        if normalizer <= 0:
            raise pipeline_uninit("pipeline.observer", "integration normalizer is zero")
        source_white = (
            np.sum(illuminant[:, None] * observer * delta_nm, axis=0) / normalizer
        )
        source_xyz = (
            np.sum(
                (illuminant * reflectance)[:, None] * observer * delta_nm,
                axis=0,
            )
            / normalizer
        )
        target_white = np.asarray(config.white_point_xyz, dtype=np.float64)
        matrix = adaptation_matrix(config.adaptation, source_white, target_white)
        adapted_xyz = matrix @ source_xyz
        linear_rgb = {
            target: XYZ_TO_RGB[target] @ adapted_xyz for target in SUPPORTED_TARGETS
        }
        encoded_rgb = {
            target: np.clip(srgb_encode(linear_rgb[target]), 0.0, 1.0)
            for target in SUPPORTED_TARGETS
        }
        colors = {
            target: tuple(float(item) for item in encoded_rgb[target])
            for target in config.target_tuple
        }
        normalization = {
            "method": "illuminant-weighted observer integration",
            "original_sample_count": int(spectrum.get("original_sample_count", 0)),
            "normalized_sample_count": int(config.grid.sample_count),
            "integration_normalizer": normalizer,
            "reflectance_min": float(np.min(reflectance)),
            "reflectance_max": float(np.max(reflectance)),
            "truth_source": str(spectrum.get("truth_source", params.get("truth_source", ""))),
            "source_hash": str(params.get("source_hash", "")),
        }
        intermediates = {
            "material_id": str(params.get("id", "")),
            "observer": config.observer,
            "illuminant_hash": sha256_obj(config.illuminant.to_dict()),
            "spectral_normalization": normalization,
            "source_white_xyz": [float(item) for item in source_white],
            "source_xyz": [float(item) for item in source_xyz],
            "target_white_xyz": [float(item) for item in target_white],
            "adaptation": {
                "strategy": config.adaptation,
                "matrix": [float(item) for item in matrix.reshape(-1)],
            },
            "adapted_xyz": [float(item) for item in adapted_xyz],
            "linear_rgb": {
                target: [float(item) for item in linear_rgb[target]]
                for target in SUPPORTED_TARGETS
            },
            "encoded_rgb": {
                target: [float(item) for item in encoded_rgb[target]]
                for target in SUPPORTED_TARGETS
            },
            "transfer_function": {
                "srgb": "IEC 61966-2-1 sRGB piecewise",
                "display-p3": "Display-P3 piecewise (sRGB transfer)",
            },
        }
        return ChainResult(
            colors=colors,
            intermediates=intermediates,
            dependencies=dict(dependencies),
        )

    def _compute_display(
        self,
        params: Mapping[str, Any],
        config: PipelineConfig,
        dependencies: Mapping[str, str],
    ) -> ChainResult:
        color_space = str(params.get("color_space"))
        if color_space not in RGB_TO_XYZ:
            raise material_invalid(
                "materials.color_space", f"unsupported source color space {color_space!r}"
            )
        encoded = _as_array(params.get("encoded", ()), "materials.encoded")
        if encoded.shape != (3,):
            raise material_invalid("materials.encoded", "must contain exactly three values")
        if np.any(encoded < 0.0) or np.any(encoded > 1.0):
            raise material_invalid("materials.encoded", "must be within [0, 1]")
        linear_source = srgb_decode(encoded)
        source_xyz = RGB_TO_XYZ[color_space] @ linear_source
        source_white = np.asarray(WHITE_POINTS["D65"], dtype=np.float64)
        target_white = np.asarray(config.white_point_xyz, dtype=np.float64)
        matrix = adaptation_matrix(config.adaptation, source_white, target_white)
        adapted_xyz = matrix @ source_xyz
        linear_rgb = {
            target: XYZ_TO_RGB[target] @ adapted_xyz for target in SUPPORTED_TARGETS
        }
        encoded_rgb = {
            target: np.clip(srgb_encode(linear_rgb[target]), 0.0, 1.0)
            for target in SUPPORTED_TARGETS
        }
        colors = {
            target: tuple(float(item) for item in encoded_rgb[target])
            for target in config.target_tuple
        }
        intermediates = {
            "material_id": str(params.get("id", "")),
            "observer": config.observer,
            "illuminant_hash": sha256_obj(config.illuminant.to_dict()),
            "spectral_normalization": {
                "method": "profile encoded transfer decode + matrix to XYZ",
                "original_sample_count": 3,
                "normalized_sample_count": 3,
                "integration_normalizer": 1.0,
                "truth_source": str(
                    params.get("truth_source", "existing-display-material-descriptor")
                ),
                "source_hash": str(params.get("source_hash", "")),
                "color_space": color_space,
                "encoded_min": float(np.min(encoded)),
                "encoded_max": float(np.max(encoded)),
                "encoded_mean": float(np.mean(encoded)),
            },
            "source_white_xyz": [float(item) for item in source_white],
            "source_xyz": [float(item) for item in source_xyz],
            "target_white_xyz": [float(item) for item in target_white],
            "adaptation": {
                "strategy": config.adaptation,
                "matrix": [float(item) for item in matrix.reshape(-1)],
            },
            "adapted_xyz": [float(item) for item in adapted_xyz],
            "linear_rgb": {
                target: [float(item) for item in linear_rgb[target]]
                for target in SUPPORTED_TARGETS
            },
            "encoded_rgb": {
                target: [float(item) for item in encoded_rgb[target]]
                for target in SUPPORTED_TARGETS
            },
            "transfer_function": {
                "srgb": "IEC 61966-2-1 sRGB piecewise",
                "display-p3": "Display-P3 piecewise (sRGB transfer)",
            },
        }
        return ChainResult(
            colors=colors,
            intermediates=intermediates,
            dependencies=dict(dependencies),
        )


def _illuminant_key(spec: IlluminantSpec) -> str:
    return sha256_obj(spec.to_dict())


def default_pipeline_config(
    *,
    seed: int = 0,
    illuminant_name: str = "D65",
    temperature_k: float = 6504.0,
    white_point_name: str = "D65",
) -> PipelineConfig:
    del seed
    grid = GridSpec(380.0, 780.0, 5.0)
    return PipelineConfig(
        grid=grid,
        observer="2deg",
        adaptation="bradford",
        white_point_name=white_point_name,
        white_point_xyz=WHITE_POINTS[white_point_name],
        illuminant=IlluminantSpec(
            name=illuminant_name,
            temperature_k=float(temperature_k),
            wavelengths_nm=None,
            values=None,
        ),
    )


def max_channel_delta(
    old_colors: Mapping[str, Any], new_colors: Mapping[str, Any]
) -> dict[str, float]:
    return _max_channel_delta(old_colors, new_colors)


def normalized_intermediates(value: Mapping[str, Any]) -> dict[str, Any]:
    return normalize_value(value)
