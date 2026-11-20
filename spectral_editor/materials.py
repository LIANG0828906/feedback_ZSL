from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping

import numpy as np

from .canonical import sha256_obj
from .errors import material_invalid
from .models import GridSpec, SUPPORTED_TARGETS, freeze_value


@dataclass(frozen=True)
class MaterialDefinition:
    material_id: str
    params: Mapping[str, Any]

    @property
    def kind(self) -> str:
        return str(self.params["kind"])


def _required(value: Mapping[str, Any], key: str, path: str) -> Any:
    if key not in value:
        raise material_invalid(f"{path}.{key}", "required field is missing")
    return value[key]


def _string(value: Any, path: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise material_invalid(path, "must be a non-empty string")
    return value.strip()


def _number(value: Any, path: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise material_invalid(path, "must be a number")
    result = float(value)
    if not math.isfinite(result):
        raise material_invalid(path, "must be finite")
    return result


def _number_list(value: Any, path: str) -> list[float]:
    if not isinstance(value, (list, tuple)):
        raise material_invalid(path, "must be an array")
    return [_number(item, f"{path}[{index}]") for index, item in enumerate(value)]


def _parse_targets(value: Any, path: str) -> tuple[str, ...]:
    if value is None:
        return SUPPORTED_TARGETS
    if not isinstance(value, (list, tuple)) or not value:
        raise material_invalid(path, "must be a non-empty array")
    requested = {_string(item, f"{path}[{index}]") for index, item in enumerate(value)}
    unknown = sorted(requested.difference(SUPPORTED_TARGETS))
    if unknown:
        raise material_invalid(path, f"unsupported target color space {unknown[0]!r}")
    return tuple(target for target in SUPPORTED_TARGETS if target in requested)


def _resample_and_validate_spectrum(
    raw: Any, path: str, grid: GridSpec
) -> dict[str, Any]:
    if not isinstance(raw, Mapping):
        raise material_invalid(path, "must be an object")
    wavelength_key = "wavelengths" if "wavelengths" in raw else "wavelengths_nm"
    if wavelength_key not in raw:
        raise material_invalid(f"{path}.wavelengths", "required field is missing")
    wavelengths = _number_list(raw[wavelength_key], f"{path}.{wavelength_key}")
    values = _number_list(_required(raw, "values", path), f"{path}.values")
    interval = _number(_required(raw, "interval_nm", path), f"{path}.interval_nm")
    if len(wavelengths) < 2:
        raise material_invalid(f"{path}.{wavelength_key}", "must contain at least two samples")
    if len(values) != len(wavelengths):
        raise material_invalid(
            f"{path}.values", "length must match wavelength sample count"
        )
    if interval <= 0:
        raise material_invalid(f"{path}.interval_nm", "must be greater than zero")
    for index, wavelength in enumerate(wavelengths):
        if wavelength <= 0:
            raise material_invalid(
                f"{path}.{wavelength_key}[{index}]", "must be greater than zero"
            )
        if index and wavelength <= wavelengths[index - 1]:
            raise material_invalid(
                f"{path}.{wavelength_key}[{index}]",
                "wavelengths must be strictly increasing",
            )
    for index in range(1, len(wavelengths)):
        actual = wavelengths[index] - wavelengths[index - 1]
        tolerance = max(1e-7, abs(interval) * 1e-6)
        if abs(actual - interval) > tolerance:
            raise material_invalid(
                f"{path}.{wavelength_key}[{index}]",
                "wavelength spacing does not match interval_nm",
            )
    for index, reflectance in enumerate(values):
        if reflectance < 0.0 or reflectance > 1.0:
            raise material_invalid(
                f"{path}.values[{index}]", "reflectance must be within [0, 1]"
            )
    resampled = np.interp(
        np.asarray(grid.wavelengths(), dtype=np.float64),
        np.asarray(wavelengths, dtype=np.float64),
        np.asarray(values, dtype=np.float64),
    )
    resampled = np.clip(resampled, 0.0, 1.0)
    truth_source = str(raw.get("truth_source", "local-json-spectrum"))
    return {
        "original_wavelengths_nm": [round(item, 10) for item in wavelengths],
        "original_values": [round(item, 12) for item in values],
        "original_sample_count": len(wavelengths),
        "original_interval_nm": interval,
        "resampled_values": [round(float(item), 12) for item in resampled],
        "resampled_interval_nm": float(grid.step_nm),
        "truth_source": truth_source,
    }


def parse_material(
    raw: Any, index: int, grid: GridSpec, *, path: str | None = None
) -> MaterialDefinition:
    base_path = path or f"materials[{index}]"
    if not isinstance(raw, Mapping):
        raise material_invalid(base_path, "must be an object")
    material_id = _string(_required(raw, "id", base_path), f"{base_path}.id")
    kind = _string(_required(raw, "kind", base_path), f"{base_path}.kind")
    name = _string(_required(raw, "name", base_path), f"{base_path}.name")
    targets = _parse_targets(raw.get("targets"), f"{base_path}.targets")
    truth_source = str(raw.get("truth_source", "local-json-material"))
    if kind == "spectral":
        spectrum = _resample_and_validate_spectrum(
            _required(raw, "spectrum", base_path), f"{base_path}.spectrum", grid
        )
        spectrum["truth_source"] = truth_source
        source_hash = sha256_obj(
            {
                "id": material_id,
                "kind": kind,
                "name": name,
                "targets": targets,
                "spectrum": {
                    "wavelengths_nm": spectrum["original_wavelengths_nm"],
                    "values": spectrum["original_values"],
                    "interval_nm": spectrum["original_interval_nm"],
                },
                "truth_source": truth_source,
            }
        )
        params = {
            "id": material_id,
            "kind": "spectral",
            "name": name,
            "targets": list(targets),
            "truth_source": truth_source,
            "source_hash": source_hash,
            "spectrum": spectrum,
        }
        return MaterialDefinition(material_id, freeze_value(params))
    if kind == "display":
        color_space = _string(
            _required(raw, "color_space", base_path), f"{base_path}.color_space"
        )
        if color_space not in ("srgb", "display-p3"):
            raise material_invalid(
                f"{base_path}.color_space",
                "must be 'srgb' or 'display-p3'",
            )
        encoded = _number_list(
            _required(raw, "encoded", base_path), f"{base_path}.encoded"
        )
        if len(encoded) != 3:
            raise material_invalid(
                f"{base_path}.encoded", "must contain exactly three channels"
            )
        for channel, value in enumerate(encoded):
            if value < 0.0 or value > 1.0:
                raise material_invalid(
                    f"{base_path}.encoded[{channel}]", "must be within [0, 1]"
                )
        source_hash = sha256_obj(
            {
                "id": material_id,
                "kind": kind,
                "name": name,
                "targets": targets,
                "color_space": color_space,
                "encoded": encoded,
                "truth_source": truth_source,
            }
        )
        params = {
            "id": material_id,
            "kind": "display",
            "name": name,
            "targets": list(targets),
            "truth_source": truth_source,
            "source_hash": source_hash,
            "color_space": color_space,
            "encoded": [round(item, 12) for item in encoded],
        }
        return MaterialDefinition(material_id, freeze_value(params))
    raise material_invalid(
        f"{base_path}.kind", "must be either 'spectral' or 'display'"
    )


def parse_material_document(document: Any, grid: GridSpec) -> list[MaterialDefinition]:
    if isinstance(document, Mapping) and "materials" in document:
        raw_materials = document["materials"]
    elif isinstance(document, list):
        raw_materials = document
    elif isinstance(document, Mapping):
        raw_materials = [document]
    else:
        raise material_invalid("$", "material document must be an object or array")
    if not isinstance(raw_materials, (list, tuple)) or not raw_materials:
        raise material_invalid("materials", "must be a non-empty array")
    definitions = [
        parse_material(raw, index, grid)
        for index, raw in enumerate(raw_materials)
    ]
    seen: set[str] = set()
    for index, definition in enumerate(definitions):
        if definition.material_id in seen:
            raise material_invalid(
                f"materials[{index}].id",
                f"duplicate material id {definition.material_id!r}",
            )
        seen.add(definition.material_id)
    return definitions


def load_material_document(path: str | Path, grid: GridSpec) -> list[MaterialDefinition]:
    source = Path(path)
    try:
        with source.open("rb") as handle:
            document = json.loads(handle.read().decode("utf-8"))
    except FileNotFoundError as exc:
        raise material_invalid("$", f"material file not found: {source}") from exc
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise material_invalid("$", f"invalid JSON material document: {exc}") from exc
    return parse_material_document(document, grid)


def params_from_single_descriptor(
    descriptor: Mapping[str, Any], grid: GridSpec, *, path: str = "material"
) -> MaterialDefinition:
    return parse_material(descriptor, 0, grid, path=path)
