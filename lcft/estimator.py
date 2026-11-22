"""Pure, deterministic sentence-level precomputation.

This estimator deliberately has no access to memory budgets, clocks, thread
counts or previously published versions.  A budget degradation stage may keep
less of these rows in memory, but it may never change their values.
"""

from __future__ import annotations

import hashlib
import unicodedata
from fractions import Fraction
from typing import Any

from .canonical import canonical_bytes, sha256_value
from .errors import param_error
from .params import ESTIMATOR_VERSION, LayoutParams, fraction_text
from .timebase import NormalizedSubtitle


def count_readable_units(text: str) -> int:
    units = 0
    for character in unicodedata.normalize("NFC", text):
        category = unicodedata.category(character)
        if category[0] in ("L", "N", "S") or category == "Cf":
            units += 1
    return units


def text_hash(text: str) -> str:
    normalized = unicodedata.normalize("NFC", text)
    return hashlib.sha256(normalized.encode("utf-8")).hexdigest()


def _ceil_fraction(value: Fraction) -> int:
    return -((-value.numerator) // value.denominator)


def effective_speed(
    item: NormalizedSubtitle, params: LayoutParams, *, description: bool
) -> Fraction:
    if item.max_read_speed is not None:
        try:
            speed = Fraction(item.max_read_speed)
        except (ValueError, ZeroDivisionError) as exc:
            raise param_error(
                f"sentence.{item.id}.max_read_speed",
                "invalid read speed",
                value=item.max_read_speed,
            ) from exc
    elif description:
        speed = params.description_read_speed
    else:
        speed = params.default_max_read_speed
    if speed <= 0:
        raise param_error(
            f"sentence.{item.id}.max_read_speed",
            "read speed must be positive",
            value=fraction_text(speed),
        )
    return speed


def estimate_param_hash(
    item: NormalizedSubtitle,
    params: LayoutParams,
    tps: int,
    *,
    description: bool,
) -> str:
    speed = effective_speed(item, params, description=description)
    minimum_seconds = (
        params.description_min_seconds if description else params.min_display_seconds
    )
    return sha256_value(
        {
            "estimator_version": ESTIMATOR_VERSION,
            "speed": fraction_text(speed),
            "min_seconds": fraction_text(minimum_seconds),
            "max_stretch_ratio": fraction_text(params.max_stretch_ratio),
            "tps": tps,
        }
    )


def input_hash(item: NormalizedSubtitle, *, description: bool) -> str:
    return sha256_value(
        {
            "id": item.id,
            "kind": "description" if description else "speech",
            "text_hash": text_hash(item.text),
            "speaker": item.speaker,
            "start_sample": item.start_sample,
            "end_sample": item.end_sample,
            "max_read_speed": item.max_read_speed,
            "priority": item.priority,
        }
    )


def estimate_sentence(
    item: NormalizedSubtitle,
    params: LayoutParams,
    tps: int,
    *,
    description: bool,
    order: int,
) -> dict[str, Any]:
    units = count_readable_units(item.text)
    speed = effective_speed(item, params, description=description)
    ideal_ticks = _ceil_fraction(Fraction(units) * tps / speed) if units else 0
    minimum_seconds = (
        params.description_min_seconds if description else params.min_display_seconds
    )
    minimum_ticks = params.seconds_to_ticks(minimum_seconds, tps, mode="ceil")
    required_ticks = max(ideal_ticks, minimum_ticks)
    original_duration = item.end_sample - item.start_sample
    max_ticks = _ceil_fraction(
        Fraction(max(required_ticks, 1)) * params.max_stretch_ratio
    )
    if original_duration >= required_ticks:
        target_ticks = original_duration
    else:
        target_ticks = min(required_ticks, max_ticks)
    param_estimate_hash = estimate_param_hash(
        item, params, tps, description=description
    )
    result: dict[str, Any] = {
        "id": item.id,
        "order": order,
        "kind": "description" if description else "speech",
        "text_hash": text_hash(item.text),
        "input_hash": input_hash(item, description=description),
        "param_estimate_hash": param_estimate_hash,
        "readable_units": units,
        "max_read_speed": fraction_text(speed),
        "ideal_ticks": ideal_ticks,
        "min_ticks": required_ticks,
        "target_ticks": target_ticks,
        "original_start_ticks": item.start_sample,
        "original_end_ticks": item.end_sample,
        "speaker": item.speaker,
        "priority": item.priority,
        "original_frame_start": item.frame_start,
        "original_frame_end": item.frame_end,
    }
    result["estimate_hash"] = sha256_value(
        {
            key: value
            for key, value in result.items()
            if key not in ("estimate_hash",)
        }
    )
    return result


def estimate_cache_key(estimate: dict[str, Any]) -> str:
    return sha256_value(
        {
            "input_hash": estimate["input_hash"],
            "param_estimate_hash": estimate["param_estimate_hash"],
        }
    )


def estimates_equal_for_reuse(
    previous: dict[str, Any], current: dict[str, Any]
) -> bool:
    keys = (
        "input_hash",
        "param_estimate_hash",
        "readable_units",
        "max_read_speed",
        "ideal_ticks",
        "min_ticks",
        "target_ticks",
        "original_start_ticks",
        "original_end_ticks",
        "speaker",
        "priority",
    )
    return canonical_bytes({key: previous.get(key) for key in keys}) == canonical_bytes(
        {key: current.get(key) for key in keys}
    )

