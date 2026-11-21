"""Serializable per-layer stroke parameters and factor versions."""

from dataclasses import dataclass, asdict

from .canonical import canonical_hash


@dataclass(frozen=True)
class OutlineParams:
    spacing: float = 2.0
    width: float = 0.35
    jitter: float = 0.12
    seed: int = 1001

    def factor(self):
        return {"kind": "outline", **asdict(self)}


@dataclass(frozen=True)
class ShadowParams:
    spacing: float = 1.6
    stroke_len: float = 2.4
    width: float = 0.22
    angle_deg: float = 45.0
    jitter: float = 0.08
    cull_margin: float = 0.18
    seed: int = 2002

    def factor(self):
        return {"kind": "shadow", **asdict(self)}


@dataclass(frozen=True)
class SmearParams:
    grid: float = 3.0
    width: float = 1.4
    jitter: float = 0.9
    angle_deg: float = 0.0
    min_alpha: float = 0.25
    max_alpha: float = 0.9
    density_radius: float = 6.0
    density_cap: int = 8
    seed: int = 3003

    def factor(self):
        return {"kind": "smear", **asdict(self)}


def params_from_dict(data):
    kind = data.get("kind")
    payload = {k: v for k, v in data.items() if k != "kind"}
    if kind == "outline":
        return OutlineParams(**payload)
    if kind == "shadow":
        return ShadowParams(**payload)
    if kind == "smear":
        return SmearParams(**payload)
    raise ValueError(f"unknown parameter kind {kind!r}")


def factor_hash(factor):
    return canonical_hash(factor)
