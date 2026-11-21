from __future__ import annotations

from pathlib import Path
from typing import Any, Dict, Iterable, List, Tuple

from .errors import E_CONFIG_INVALID, BusinessError
from .models import BandSpec, CalibrationPolicy, PhasePolicy, ScenarioConfig
from .util import finite_number, load_json, require_keys


VALID_LAYOUTS = {"mono", "5.1"}
TOP_LEVEL_KEYS = (
    "bands",
    "max_cancellation_gain_db",
    "target_suppression_db",
    "measurement_layout",
)


def parse_config(payload: Dict[str, Any]) -> ScenarioConfig:
    require_keys(payload, TOP_LEVEL_KEYS, "config")
    raw_bands = payload["bands"]
    if not isinstance(raw_bands, list) or not raw_bands:
        raise BusinessError(
            E_CONFIG_INVALID,
            "config.bands must be a non-empty list",
            details={"field": "bands"},
        )

    bands: List[BandSpec] = []
    seen_ids = set()
    previous_high = -1.0
    for index, raw in enumerate(raw_bands):
        if not isinstance(raw, dict):
            raise BusinessError(
                E_CONFIG_INVALID,
                f"config.bands[{index}] must be an object",
                details={"index": index},
            )
        require_keys(
            raw, ("id", "low_hz", "high_hz", "transition_hz"), f"config.bands[{index}]"
        )
        band_id = str(raw["id"]).strip()
        if not band_id or band_id in seen_ids:
            raise BusinessError(
                E_CONFIG_INVALID,
                "band ids must be non-empty and unique",
                band=band_id or f"index:{index}",
                details={"index": index},
            )
        seen_ids.add(band_id)
        low = finite_number(raw["low_hz"], f"bands[{index}].low_hz")
        high = finite_number(raw["high_hz"], f"bands[{index}].high_hz")
        transition = finite_number(
            raw["transition_hz"], f"bands[{index}].transition_hz"
        )
        if low < 0.0:
            raise BusinessError(
                E_CONFIG_INVALID, "band low_hz must be >= 0", band=band_id
            )
        if high <= low:
            raise BusinessError(
                E_CONFIG_INVALID,
                "band high_hz must be greater than low_hz",
                band=band_id,
                details={"low_hz": low, "high_hz": high},
            )
        width = high - low
        if transition <= 0.0 or transition > width:
            raise BusinessError(
                E_CONFIG_INVALID,
                "band transition_hz must be > 0 and <= band width",
                band=band_id,
                details={"transition_hz": transition, "width_hz": width},
            )
        if low < previous_high:
            raise BusinessError(
                E_CONFIG_INVALID,
                "bands must be ordered and non-overlapping",
                band=band_id,
                details={"previous_high_hz": previous_high, "low_hz": low},
            )
        previous_high = high
        bands.append(
            BandSpec(
                id=band_id,
                low_hz=low,
                high_hz=high,
                transition_hz=transition,
            )
        )

    max_gain = finite_number(
        payload["max_cancellation_gain_db"], "max_cancellation_gain_db"
    )
    target = finite_number(payload["target_suppression_db"], "target_suppression_db")
    if not 0.0 <= max_gain <= 60.0:
        raise BusinessError(
            E_CONFIG_INVALID,
            "max_cancellation_gain_db must be within [0, 60]",
            details={"max_cancellation_gain_db": max_gain},
        )
    if not 0.0 < target <= 120.0:
        raise BusinessError(
            E_CONFIG_INVALID,
            "target_suppression_db must be within (0, 120]",
            details={"target_suppression_db": target},
        )

    layout = str(payload["measurement_layout"]).strip()
    if layout not in VALID_LAYOUTS:
        raise BusinessError(
            E_CONFIG_INVALID,
            "measurement_layout must be mono or 5.1",
            details={"measurement_layout": layout},
        )

    min_band_samples = int(payload.get("min_band_samples", 64))
    if min_band_samples < 8:
        raise BusinessError(
            E_CONFIG_INVALID,
            "min_band_samples must be >= 8",
            details={"min_band_samples": min_band_samples},
        )

    raw_phase = payload.get("phase", {})
    if not isinstance(raw_phase, dict):
        raise BusinessError(
            E_CONFIG_INVALID, "phase policy must be an object", details={"field": "phase"}
        )
    phase = PhasePolicy(
        jump_threshold_deg=finite_number(
            raw_phase.get("jump_threshold_deg", 120.0),
            "phase.jump_threshold_deg",
        ),
        polarity_tolerance_deg=finite_number(
            raw_phase.get("polarity_tolerance_deg", 35.0),
            "phase.polarity_tolerance_deg",
        ),
        low_frequency_max_hz=finite_number(
            raw_phase.get("low_frequency_max_hz", 500.0),
            "phase.low_frequency_max_hz",
        ),
        group_delay_jump_samples=finite_number(
            raw_phase.get("group_delay_jump_samples", 2.5),
            "phase.group_delay_jump_samples",
        ),
        ambiguity_confidence=finite_number(
            raw_phase.get("ambiguity_confidence", 0.6),
            "phase.ambiguity_confidence",
        ),
    )
    if not 0.0 < phase.jump_threshold_deg < 180.0:
        raise BusinessError(
            E_CONFIG_INVALID,
            "phase.jump_threshold_deg must be within (0, 180)",
        )
    if not 0.0 < phase.polarity_tolerance_deg <= 90.0:
        raise BusinessError(
            E_CONFIG_INVALID,
            "phase.polarity_tolerance_deg must be within (0, 90]",
        )
    if not 0.0 <= phase.ambiguity_confidence <= 1.0:
        raise BusinessError(
            E_CONFIG_INVALID,
            "phase.ambiguity_confidence must be within [0, 1]",
        )

    raw_calibration = payload.get("calibration", {})
    if not isinstance(raw_calibration, dict):
        raise BusinessError(
            E_CONFIG_INVALID,
            "calibration policy must be an object",
            details={"field": "calibration"},
        )
    gain_step = finite_number(
        raw_calibration.get("gain_step_db", 0.05), "calibration.gain_step_db"
    )
    residual_floor = finite_number(
        raw_calibration.get("residual_floor", 1e-12),
        "calibration.residual_floor",
    )
    if not 0.0 < gain_step <= 6.0:
        raise BusinessError(
            E_CONFIG_INVALID, "calibration.gain_step_db must be within (0, 6]"
        )
    if not 0.0 < residual_floor < 1.0:
        raise BusinessError(
            E_CONFIG_INVALID, "calibration.residual_floor must be within (0, 1)"
        )
    calibration = CalibrationPolicy(
        gain_step_db=gain_step,
        residual_floor=residual_floor,
    )

    return ScenarioConfig(
        bands=tuple(bands),
        max_cancellation_gain_db=max_gain,
        target_suppression_db=target,
        measurement_layout=layout,
        min_band_samples=min_band_samples,
        phase=phase,
        calibration=calibration,
        estimator_version=str(payload.get("estimator_version", "band-ls-v1")),
    )


def load_config(path: Path) -> ScenarioConfig:
    payload = load_json(path)
    if not isinstance(payload, dict):
        raise BusinessError(E_CONFIG_INVALID, "config root must be an object")
    return parse_config(payload)


def changed_band_ids(
    old_bands: Iterable[Dict[str, Any]], new_bands: Iterable[BandSpec]
) -> Tuple[List[str], List[str]]:
    old_by_id = {str(item["id"]): item for item in old_bands}
    new_by_id = {band.id: band for band in new_bands}
    changed: List[str] = []
    if set(old_by_id) != set(new_by_id):
        return sorted(new_by_id), sorted(set(old_by_id) | set(new_by_id))
    for band_id, band in new_by_id.items():
        old = old_by_id[band_id]
        if any(
            float(old[key]) != float(getattr(band, key))
            for key in ("low_hz", "high_hz", "transition_hz")
        ):
            changed.append(band_id)
    return changed, []


def neighboring_bands(band_ids: Iterable[str], changed: Iterable[str]) -> List[str]:
    order = list(band_ids)
    index = {band_id: position for position, band_id in enumerate(order)}
    affected = set(changed)
    for band_id in changed:
        position = index.get(band_id)
        if position is None:
            continue
        if position > 0:
            affected.add(order[position - 1])
        if position + 1 < len(order):
            affected.add(order[position + 1])
    return [band_id for band_id in order if band_id in affected]
