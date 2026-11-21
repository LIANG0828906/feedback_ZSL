"""Shot damage and attenuation calculations."""

from __future__ import annotations

import math
from decimal import Decimal, localcontext

from .fixed import quantize_units
from .model import CoverSpec, Neighbor, ReplayConfig, Shot


def shot_damage_units(shot: Shot, cover: CoverSpec, config: ReplayConfig) -> int:
    angle_radians = math.radians(float(shot.incidence_angle))
    cosine = Decimal(str(math.cos(angle_radians)))
    with localcontext() as context:
        context.prec = max(50, config.fixed_point_decimals + 40)
        raw_damage = shot.momentum * cosine / cover.material_hardness
    return quantize_units(raw_damage, config.fixed_point_decimals, config.quantization_mode)


def propagation_units(
    source: CoverSpec,
    neighbor: Neighbor,
    config: ReplayConfig,
) -> int:
    if source.attenuation_radius <= 0:
        return 0
    with localcontext() as context:
        context.prec = max(50, config.fixed_point_decimals + 40)
        attenuation = Decimal(1) - neighbor.distance / source.attenuation_radius
        if attenuation <= 0:
            return 0
        source_amount = source.segment_damage_threshold * config.crack_propagation_ratio
        raw_amount = source_amount * attenuation
    return quantize_units(raw_amount, config.fixed_point_decimals, config.quantization_mode)

