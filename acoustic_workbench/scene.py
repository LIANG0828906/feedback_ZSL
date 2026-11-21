"""Venue scene loading, validation, material lookup and factor hashing."""

from __future__ import annotations

import copy
import math
from pathlib import Path
from typing import Any, Dict, Iterable, List, Mapping, Optional, Sequence, Tuple

from .codes import (
    AcousticError,
    E_CHANNEL_LAYOUT_MISMATCH,
    E_CONFIG_INVALID,
    E_DECAY_NEGATIVE,
    E_POSITION_OUT_OF_BOUNDS,
)
from .util import hash_obj, read_json


CHANNEL_ORDER: Tuple[str, ...] = ("FL", "FR", "FC", "LFE", "BL", "BR")
STAGE_ORDER: Tuple[str, ...] = (
    "ir_load",
    "direct_alignment",
    "early_reflections",
    "decay_model",
    "late_reverb",
    "spatial_synthesis",
    "field_coefficients",
)

FACTOR_ORDER: Tuple[str, ...] = (
    "ir_file",
    "channel_layout",
    "alignment",
    "window_params",
    "frequency_config",
    "source_position",
    "listener_position",
    "material",
    "decay_config",
    "reverb_config",
)

FACTOR_EARLIEST_STAGE: Dict[str, str] = {
    "ir_file": "ir_load",
    "channel_layout": "ir_load",
    "alignment": "direct_alignment",
    "window_params": "early_reflections",
    "frequency_config": "early_reflections",
    "source_position": "early_reflections",
    "listener_position": "early_reflections",
    "material": "decay_model",
    "decay_config": "decay_model",
    "reverb_config": "late_reverb",
}


def load_scene(path: Path) -> Dict[str, Any]:
    if not Path(path).is_file():
        raise AcousticError(
            E_CONFIG_INVALID,
            "Scene description does not exist.",
            injection_point=f"scene:{path}",
        )
    try:
        scene = read_json(path)
    except (OSError, ValueError) as exc:
        raise AcousticError(
            E_CONFIG_INVALID,
            f"Scene description cannot be read: {exc}",
            injection_point=f"scene:{path}",
        ) from exc
    if not isinstance(scene, dict):
        raise AcousticError(
            E_CONFIG_INVALID,
            "Scene root must be a JSON object.",
            injection_point="scene:root",
        )
    return scene


def clone_scene(scene: Mapping[str, Any]) -> Dict[str, Any]:
    return copy.deepcopy(dict(scene))


def listener_by_id(scene: Mapping[str, Any], listener_id: str) -> Dict[str, Any]:
    for listener in scene.get("listeners", []):
        if listener.get("id") == listener_id:
            return listener
    raise AcousticError(
        E_CONFIG_INVALID,
        f"Unknown listener: {listener_id}",
        injection_point="scene:listeners",
    )


def _finite_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _position(value: Any, field: str) -> Tuple[float, float, float]:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise AcousticError(
            E_CONFIG_INVALID,
            f"{field} must contain exactly three coordinates.",
            injection_point=f"scene:{field}",
        )
    if not all(_finite_number(item) for item in value):
        raise AcousticError(
            E_CONFIG_INVALID,
            f"{field} contains a non-finite coordinate.",
            injection_point=f"scene:{field}",
        )
    return float(value[0]), float(value[1]), float(value[2])


def _bounds(scene: Mapping[str, Any]) -> Tuple[Tuple[float, float, float], Tuple[float, float, float]]:
    venue = scene.get("venue")
    if not isinstance(venue, dict) or not isinstance(venue.get("bounds"), dict):
        raise AcousticError(
            E_CONFIG_INVALID,
            "venue.bounds is required.",
            injection_point="scene:venue.bounds",
        )
    bounds = venue["bounds"]
    low = _position(bounds.get("min"), "venue.bounds.min")
    high = _position(bounds.get("max"), "venue.bounds.max")
    if any(high[index] <= low[index] for index in range(3)):
        raise AcousticError(
            E_CONFIG_INVALID,
            "Each venue bound maximum must exceed its minimum.",
            injection_point="scene:venue.bounds",
        )
    return low, high


def _validate_position_in_bounds(
    position: Sequence[float],
    low: Sequence[float],
    high: Sequence[float],
    *,
    location: str,
) -> None:
    for index, coordinate in enumerate(position):
        if coordinate < low[index] or coordinate > high[index]:
            raise AcousticError(
                E_POSITION_OUT_OF_BOUNDS,
                "Position lies outside the venue bounds.",
                injection_point=location,
                details={
                    "detected_at": "scene:venue.bounds",
                    "position": list(position),
                    "bounds_min": list(low),
                    "bounds_max": list(high),
                },
            )


def validate_scene(scene: Mapping[str, Any], listener_id: str) -> Dict[str, Any]:
    if not isinstance(scene.get("scene_id"), str) or not scene.get("scene_id"):
        raise AcousticError(
            E_CONFIG_INVALID,
            "scene_id is required.",
            injection_point="scene:scene_id",
        )
    venue = scene.get("venue")
    if not isinstance(venue, dict):
        raise AcousticError(
            E_CONFIG_INVALID,
            "venue is required.",
            injection_point="scene:venue",
        )
    low, high = _bounds(scene)

    layout = scene.get("channel_layout")
    if not isinstance(layout, list) or len(layout) != len(CHANNEL_ORDER):
        raise AcousticError(
            E_CHANNEL_LAYOUT_MISMATCH,
            "A complete six-channel 5.1 layout is required.",
            injection_point="scene:channel_layout",
            details={"detected_at": "scene:channel_layout", "channel_layout": layout},
        )
    if set(layout) != set(CHANNEL_ORDER) or len(set(layout)) != len(CHANNEL_ORDER):
        raise AcousticError(
            E_CHANNEL_LAYOUT_MISMATCH,
            "Channel layout must contain FL, FR, FC, LFE, BL and BR exactly once.",
            injection_point="scene:channel_layout",
            details={"detected_at": "scene:channel_layout", "channel_layout": layout},
        )

    source = scene.get("source")
    if not isinstance(source, dict):
        raise AcousticError(
            E_CONFIG_INVALID,
            "source is required.",
            injection_point="scene:source",
        )
    source_position = _position(source.get("position"), "source.position")
    _validate_position_in_bounds(
        source_position,
        low,
        high,
        location="scene:source.position",
    )

    listener = listener_by_id(scene, listener_id)
    listener_position = _position(listener.get("position"), f"listeners[{listener_id}].position")
    _validate_position_in_bounds(
        listener_position,
        low,
        high,
        location=f"scene:listeners[{listener_id}].position",
    )
    if not isinstance(listener.get("ir"), str) or not listener["ir"]:
        raise AcousticError(
            E_CONFIG_INVALID,
            "Listener IR path is required.",
            injection_point=f"scene:listeners[{listener_id}].ir",
        )

    alignment = scene.get("alignment", {})
    threshold = alignment.get("threshold", 0.05)
    if not _finite_number(threshold) or not 0.0 < float(threshold) <= 1.0:
        raise AcousticError(
            E_CONFIG_INVALID,
            "alignment.threshold must be finite and in (0, 1].",
            injection_point="scene:alignment.threshold",
        )
    reference = listener.get("reference_channel", alignment.get("reference_channel", "FC"))
    if reference not in layout:
        raise AcousticError(
            E_CHANNEL_LAYOUT_MISMATCH,
            "Reference channel is not present in the channel layout.",
            injection_point=f"scene:listeners[{listener_id}].reference_channel",
            details={"detected_at": "scene:alignment.reference_channel", "reference_channel": reference},
        )

    window = scene.get("window_params")
    if not isinstance(window, dict):
        raise AcousticError(
            E_CONFIG_INVALID,
            "window_params is required.",
            injection_point="scene:window_params",
        )
    direct_ms = window.get("direct_window_ms")
    early_ms = window.get("early_window_ms")
    tail_ms = window.get("tail_ms")
    if not all(_finite_number(item) and float(item) > 0 for item in (direct_ms, early_ms, tail_ms)):
        raise AcousticError(
            E_CONFIG_INVALID,
            "Window durations must be finite and positive.",
            injection_point="scene:window_params",
        )
    if float(direct_ms) >= float(early_ms):
        raise AcousticError(
            E_CONFIG_INVALID,
            "direct_window_ms must be smaller than early_window_ms.",
            injection_point="scene:window_params.direct_window_ms",
        )

    frequency = scene.get("frequency_config")
    if not isinstance(frequency, dict):
        raise AcousticError(
            E_CONFIG_INVALID,
            "frequency_config is required.",
            injection_point="scene:frequency_config",
        )
    bands = frequency.get("bands_hz")
    if (
        not isinstance(bands, list)
        or not bands
        or not all(_finite_number(item) and float(item) > 0 for item in bands)
        or list(map(float, bands)) != sorted(map(float, bands))
    ):
        raise AcousticError(
            E_CONFIG_INVALID,
            "frequency_config.bands_hz must be a sorted positive list.",
            injection_point="scene:frequency_config.bands_hz",
        )
    fft_size = frequency.get("fft_size")
    if not isinstance(fft_size, int) or fft_size < 64 or fft_size & (fft_size - 1):
        raise AcousticError(
            E_CONFIG_INVALID,
            "frequency_config.fft_size must be a power-of-two integer >= 64.",
            injection_point="scene:frequency_config.fft_size",
        )

    decay = scene.get("decay_config", {})
    override = decay.get("decay_time_override")
    if override is not None and (not _finite_number(override) or float(override) <= 0.0):
        raise AcousticError(
            E_DECAY_NEGATIVE,
            "decay_time_override must be finite and positive.",
            injection_point=f"scene:decay_config.decay_time_override",
            details={"detected_at": "scene:decay_config.decay_time_override", "value": override},
        )
    min_decay = decay.get("min_decay_time_sec", 0.05)
    if not _finite_number(min_decay) or float(min_decay) <= 0:
        raise AcousticError(
            E_DECAY_NEGATIVE,
            "min_decay_time_sec must be finite and positive.",
            injection_point="scene:decay_config.min_decay_time_sec",
        )

    material_table = venue.get("material_table")
    surfaces = venue.get("surfaces")
    if not isinstance(material_table, dict) or not material_table:
        raise AcousticError(
            E_CONFIG_INVALID,
            "venue.material_table is required.",
            injection_point="scene:venue.material_table",
        )
    if not isinstance(surfaces, dict) or not surfaces:
        raise AcousticError(
            E_CONFIG_INVALID,
            "venue.surfaces is required.",
            injection_point="scene:venue.surfaces",
        )
    band_count = len(bands)
    for surface, material_name in surfaces.items():
        material = material_table.get(material_name)
        if not isinstance(material, dict):
            raise AcousticError(
                E_CONFIG_INVALID,
                f"Unknown material for surface {surface}: {material_name}",
                injection_point=f"scene:venue.surfaces.{surface}",
            )
        absorption = material.get("absorption")
        if (
            not isinstance(absorption, list)
            or len(absorption) != band_count
            or not all(_finite_number(item) and 0.0 <= float(item) < 1.0 for item in absorption)
        ):
            raise AcousticError(
                E_CONFIG_INVALID,
                f"Material {material_name} absorption must have one value per octave band.",
                injection_point=f"scene:venue.material_table.{material_name}.absorption",
            )

    sample_rate = scene.get("sample_rate", 48000)
    if sample_rate != 48000:
        raise AcousticError(
            E_CONFIG_INVALID,
            "This workbench requires 48000 Hz IRs.",
            injection_point="scene:sample_rate",
        )
    return {
        "listener": listener,
        "listener_position": listener_position,
        "source_position": source_position,
        "bounds_min": low,
        "bounds_max": high,
        "reference_channel": reference,
        "layout": list(layout),
    }


def material_profile(scene: Mapping[str, Any]) -> Dict[str, Any]:
    venue = scene["venue"]
    low, high = _bounds(scene)
    extents = [high[index] - low[index] for index in range(3)]
    volume = extents[0] * extents[1] * extents[2]
    surface_area = 2.0 * (
        extents[0] * extents[1]
        + extents[0] * extents[2]
        + extents[1] * extents[2]
    )
    bands = [float(item) for item in scene["frequency_config"]["bands_hz"]]
    selected = []
    alpha = [0.0 for _ in bands]
    surfaces = venue["surfaces"]
    for surface_name in sorted(surfaces):
        material_name = surfaces[surface_name]
        material = venue["material_table"][material_name]
        absorption = [float(item) for item in material["absorption"]]
        selected.append(
            {
                "surface": surface_name,
                "material": material_name,
                "absorption": absorption,
            }
        )
        for index, value in enumerate(absorption):
            alpha[index] += value / len(surfaces)
    mean_alpha = sum(alpha) / len(alpha)
    return {
        "venue_id": venue.get("id", ""),
        "bounds_min": list(low),
        "bounds_max": list(high),
        "volume_m3": round(volume, 12),
        "surface_area_m2": round(surface_area, 12),
        "bands_hz": bands,
        "absorption": [round(item, 12) for item in alpha],
        "mean_absorption": round(mean_alpha, 12),
        "reflection_gain": round(math.sqrt(max(0.0, 1.0 - mean_alpha)), 12),
        "selected_materials": selected,
    }


def factor_hashes(scene: Mapping[str, Any], listener_id: str) -> Dict[str, str]:
    listener = listener_by_id(scene, listener_id)
    alignment = scene.get("alignment", {})
    reference = listener.get("reference_channel", alignment.get("reference_channel", "FC"))
    material = material_profile(scene)
    return {
        "ir_file": hash_obj(listener["ir"]),
        "channel_layout": hash_obj(scene["channel_layout"]),
        "alignment": hash_obj(
            {
                "threshold": alignment.get("threshold", 0.05),
                "reference_channel": reference,
                "normalize_gain": bool(alignment.get("normalize_gain", True)),
            }
        ),
        "window_params": hash_obj(scene["window_params"]),
        "frequency_config": hash_obj(scene["frequency_config"]),
        "source_position": hash_obj(scene["source"]["position"]),
        "listener_position": hash_obj(listener["position"]),
        "material": hash_obj(material),
        "decay_config": hash_obj(scene.get("decay_config", {})),
        "reverb_config": hash_obj(scene.get("reverb", {})),
    }


def stage_fingerprints(
    scene: Mapping[str, Any],
    listener_id: str,
    *,
    raw_wav_sha256: str,
) -> Dict[str, str]:
    factors = factor_hashes(scene, listener_id)
    load = hash_obj(
        {
            "stage": "ir_load",
            "schema": 1,
            "raw_wav": raw_wav_sha256,
            "layout": factors["channel_layout"],
            "sample_rate": scene.get("sample_rate", 48000),
        }
    )
    align = hash_obj(
        {
            "stage": "direct_alignment",
            "schema": 1,
            "load": load,
            "alignment": factors["alignment"],
        }
    )
    early = hash_obj(
        {
            "stage": "early_reflections",
            "schema": 1,
            "align": align,
            "window": factors["window_params"],
            "frequency": factors["frequency_config"],
            "source_position": factors["source_position"],
            "listener_position": factors["listener_position"],
        }
    )
    decay = hash_obj(
        {
            "stage": "decay_model",
            "schema": 1,
            "early": early,
            "frequency": factors["frequency_config"],
            "material": factors["material"],
            "decay_config": factors["decay_config"],
            "source_position": factors["source_position"],
            "listener_position": factors["listener_position"],
        }
    )
    reverb = hash_obj(
        {
            "stage": "late_reverb",
            "schema": 1,
            "decay": decay,
            "early": early,
            "reverb_config": factors["reverb_config"],
            "layout": factors["channel_layout"],
            "source_position": factors["source_position"],
            "listener_position": factors["listener_position"],
            "material": factors["material"],
        }
    )
    spatial = hash_obj(
        {
            "stage": "spatial_synthesis",
            "schema": 1,
            "reverb": reverb,
            "early": early,
            "align": align,
            "layout": factors["channel_layout"],
            "sample_rate": scene.get("sample_rate", 48000),
        }
    )
    field = hash_obj(
        {
            "stage": "field_coefficients",
            "schema": 1,
            "spatial": spatial,
            "decay": decay,
            "listener_position": factors["listener_position"],
            "source_position": factors["source_position"],
            "layout": factors["channel_layout"],
        }
    )
    return {
        "ir_load": load,
        "direct_alignment": align,
        "early_reflections": early,
        "decay_model": decay,
        "late_reverb": reverb,
        "spatial_synthesis": spatial,
        "field_coefficients": field,
    }


def earliest_affected_stage(changed_factors: Iterable[str]) -> Optional[str]:
    indices = [
        STAGE_ORDER.index(FACTOR_EARLIEST_STAGE[factor])
        for factor in changed_factors
        if factor in FACTOR_EARLIEST_STAGE
    ]
    if not indices:
        return None
    return STAGE_ORDER[min(indices)]


def stages_from(stage: Optional[str]) -> List[str]:
    if stage is None:
        return []
    return list(STAGE_ORDER[STAGE_ORDER.index(stage) :])


def ensure_listener_directory(scene: Mapping[str, Any], listener: Mapping[str, Any]) -> Path:
    ir_path = Path(str(listener["ir"]))
    if ir_path.is_absolute():
        return ir_path
    return Path(str(scene.get("_scene_dir", "."))) / ir_path

