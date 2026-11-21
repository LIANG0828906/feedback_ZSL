from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

from .canonical import canonical_json_bytes, qfloat, sha256_json
from .errors import E_INVALID_REQUEST, fail


def _require(mapping: dict[str, Any], key: str, location: str) -> Any:
    if key not in mapping:
        fail(E_INVALID_REQUEST, location, "missing_field", field=key)
    return mapping[key]


def _number(
    mapping: dict[str, Any],
    key: str,
    location: str,
    *,
    positive: bool = False,
    minimum: float | None = None,
    maximum: float | None = None,
) -> float:
    raw = _require(mapping, key, location)
    if isinstance(raw, bool) or not isinstance(raw, (int, float)):
        fail(E_INVALID_REQUEST, f"{location}.{key}", "invalid_number")
    value = float(raw)
    if not math.isfinite(value):
        fail(E_INVALID_REQUEST, f"{location}.{key}", "non_finite_number")
    if positive and value <= 0:
        fail(E_INVALID_REQUEST, f"{location}.{key}", "must_be_positive")
    if minimum is not None and value < minimum:
        fail(E_INVALID_REQUEST, f"{location}.{key}", "below_minimum")
    if maximum is not None and value > maximum:
        fail(E_INVALID_REQUEST, f"{location}.{key}", "above_maximum")
    return qfloat(value)


def _seed(mapping: dict[str, Any], location: str) -> int:
    raw = _require(mapping, "seed", location)
    if isinstance(raw, bool) or not isinstance(raw, int) or raw < 0:
        fail(E_INVALID_REQUEST, f"{location}.seed", "invalid_seed")
    return raw


def _color(mapping: dict[str, Any], location: str) -> str:
    raw = _require(mapping, "color", location)
    if not isinstance(raw, str):
        fail(E_INVALID_REQUEST, f"{location}.color", "invalid_color")
    text = raw.strip()
    valid_lengths = (4, 7, 9)
    if not text.startswith("#") or len(text) not in valid_lengths:
        fail(E_INVALID_REQUEST, f"{location}.color", "invalid_color")
    try:
        int(text[1:], 16)
    except ValueError:
        fail(E_INVALID_REQUEST, f"{location}.color", "invalid_color")
    return text.lower()


@dataclass(frozen=True)
class OutlineParams:
    width: float = 2.8
    spacing: float = 7.0
    jitter: float = 1.1
    length: float = 6.0
    color: str = "#20242a"
    alpha: float = 1.0
    seed: int = 11

    def to_dict(self) -> dict[str, Any]:
        return {
            "width": self.width,
            "spacing": self.spacing,
            "jitter": self.jitter,
            "length": self.length,
            "color": self.color,
            "alpha": self.alpha,
            "seed": self.seed,
        }

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> "OutlineParams":
        location = "params.outline"
        if not isinstance(raw, dict):
            fail(E_INVALID_REQUEST, location, "invalid_params")
        return cls(
            width=_number(raw, "width", location, positive=True),
            spacing=_number(raw, "spacing", location, positive=True),
            jitter=_number(raw, "jitter", location, minimum=0.0),
            length=_number(raw, "length", location, positive=True),
            color=_color(raw, location),
            alpha=_number(raw, "alpha", location, minimum=0.0, maximum=1.0),
            seed=_seed(raw, location),
        )

    def factor_hash(self) -> str:
        return sha256_json(self.to_dict())

    def geometry_influence_hash(self) -> str:
        return sha256_json(
            {
                "width": self.width,
                "spacing": self.spacing,
                "jitter": self.jitter,
                "length": self.length,
                "seed": self.seed,
            }
        )


@dataclass(frozen=True)
class ShadowParams:
    width: float = 1.6
    spacing: float = 8.0
    jitter: float = 0.8
    angle_deg: float = 38.0
    color: str = "#444a52"
    alpha: float = 0.72
    reject_margin: float = 0.75
    seed: int = 23

    def to_dict(self) -> dict[str, Any]:
        return {
            "width": self.width,
            "spacing": self.spacing,
            "jitter": self.jitter,
            "angle_deg": self.angle_deg,
            "color": self.color,
            "alpha": self.alpha,
            "reject_margin": self.reject_margin,
            "seed": self.seed,
        }

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> "ShadowParams":
        location = "params.shadow"
        if not isinstance(raw, dict):
            fail(E_INVALID_REQUEST, location, "invalid_params")
        return cls(
            width=_number(raw, "width", location, positive=True),
            spacing=_number(raw, "spacing", location, positive=True),
            jitter=_number(raw, "jitter", location, minimum=0.0),
            angle_deg=_number(raw, "angle_deg", location),
            color=_color(raw, location),
            alpha=_number(raw, "alpha", location, minimum=0.0, maximum=1.0),
            reject_margin=_number(raw, "reject_margin", location, minimum=0.0),
            seed=_seed(raw, location),
        )

    def factor_hash(self) -> str:
        return sha256_json(self.to_dict())

    def density_influence_hash(self) -> str:
        return sha256_json(
            {
                "width": self.width,
                "spacing": self.spacing,
                "jitter": self.jitter,
                "angle_deg": self.angle_deg,
                "seed": self.seed,
            }
        )


@dataclass(frozen=True)
class SmearParams:
    width: float = 9.0
    spacing: float = 11.0
    length: float = 15.0
    jitter: float = 2.5
    angle_deg: float = -12.0
    opacity: float = 0.42
    min_opacity: float = 0.08
    density_radius: float = 18.0
    density_weight: float = 0.72
    seed: int = 37

    def to_dict(self) -> dict[str, Any]:
        return {
            "width": self.width,
            "spacing": self.spacing,
            "length": self.length,
            "jitter": self.jitter,
            "angle_deg": self.angle_deg,
            "opacity": self.opacity,
            "min_opacity": self.min_opacity,
            "density_radius": self.density_radius,
            "density_weight": self.density_weight,
            "seed": self.seed,
        }

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> "SmearParams":
        location = "params.smear"
        if not isinstance(raw, dict):
            fail(E_INVALID_REQUEST, location, "invalid_params")
        minimum = _number(raw, "min_opacity", location, minimum=0.0, maximum=1.0)
        opacity = _number(raw, "opacity", location, minimum=0.0, maximum=1.0)
        if minimum > opacity:
            fail(
                E_INVALID_REQUEST,
                f"{location}.min_opacity",
                "min_opacity_exceeds_opacity",
            )
        return cls(
            width=_number(raw, "width", location, positive=True),
            spacing=_number(raw, "spacing", location, positive=True),
            length=_number(raw, "length", location, positive=True),
            jitter=_number(raw, "jitter", location, minimum=0.0),
            angle_deg=_number(raw, "angle_deg", location),
            opacity=opacity,
            min_opacity=minimum,
            density_radius=_number(raw, "density_radius", location, positive=True),
            density_weight=_number(raw, "density_weight", location, minimum=0.0, maximum=1.0),
            seed=_seed(raw, location),
        )

    def factor_hash(self) -> str:
        return sha256_json(self.to_dict())


@dataclass(frozen=True)
class RenderParams:
    outline: OutlineParams
    shadow: ShadowParams
    smear: SmearParams
    grid_size: float = 128.0

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema": "npr-params-v1",
            "grid_size": self.grid_size,
            "outline": self.outline.to_dict(),
            "shadow": self.shadow.to_dict(),
            "smear": self.smear.to_dict(),
        }

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> "RenderParams":
        if not isinstance(raw, dict):
            fail(E_INVALID_REQUEST, "params", "invalid_params")
        schema = raw.get("schema", "npr-params-v1")
        if schema != "npr-params-v1":
            fail(E_INVALID_REQUEST, "params.schema", "unsupported_schema")
        grid_size = _number(raw, "grid_size", "params", positive=True)
        return cls(
            outline=OutlineParams.from_dict(_require(raw, "outline", "params")),
            shadow=ShadowParams.from_dict(_require(raw, "shadow", "params")),
            smear=SmearParams.from_dict(_require(raw, "smear", "params")),
            grid_size=grid_size,
        )

    @classmethod
    def default(cls) -> "RenderParams":
        return cls(
            outline=OutlineParams(),
            shadow=ShadowParams(),
            smear=SmearParams(),
        )

    @property
    def params_hash(self) -> str:
        return sha256_json(self.to_dict())

    def factor_versions(self, scene: Any) -> dict[str, str]:
        factors: dict[str, str] = {
            "outline": self.outline.factor_hash(),
            "outline_geometry": self.outline.geometry_influence_hash(),
            "shadow": self.shadow.factor_hash(),
            "shadow_density": self.shadow.density_influence_hash(),
            "smear": self.smear.factor_hash(),
            "grid": sha256_json({"grid_size": self.grid_size}),
        }
        for zone_id in scene.draw_order:
            factors[f"geometry.{zone_id}"] = scene.zone_factor(zone_id)
        return factors


def params_bytes(params: RenderParams) -> bytes:
    return canonical_json_bytes(params.to_dict())


__all__ = [
    "OutlineParams",
    "RenderParams",
    "ShadowParams",
    "SmearParams",
    "params_bytes",
]
