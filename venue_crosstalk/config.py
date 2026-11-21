"""Band plan and analysis configuration."""

from __future__ import annotations

import math
from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import Any, Iterable

from .errors import E_CONFIG_INVALID, VenueError
from .utils import atomic_write_json, read_json, sha256_json


@dataclass(frozen=True)
class BandSpec:
    index: int
    name: str
    low_hz: float
    high_hz: float

    @property
    def bandwidth_hz(self) -> float:
        return self.high_hz - self.low_hz

    @property
    def interval(self) -> list[float]:
        return [self.low_hz, self.high_hz]

    @property
    def center_hz(self) -> float:
        return (self.low_hz + self.high_hz) / 2.0

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "name": self.name,
            "low_hz": self.low_hz,
            "high_hz": self.high_hz,
            "bandwidth_hz": self.bandwidth_hz,
        }


@dataclass(frozen=True)
class PolarityConfig:
    inversion_tolerance_deg: float = 30.0
    jump_threshold_deg: float = 75.0
    wrap_threshold_deg: float = 240.0
    low_band_max_hz: float = 500.0
    group_delay_jump_ms: float = 1.5

    def to_dict(self) -> dict[str, Any]:
        return {
            "inversion_tolerance_deg": self.inversion_tolerance_deg,
            "jump_threshold_deg": self.jump_threshold_deg,
            "wrap_threshold_deg": self.wrap_threshold_deg,
            "low_band_max_hz": self.low_band_max_hz,
            "group_delay_jump_ms": self.group_delay_jump_ms,
        }


@dataclass(frozen=True)
class AnalysisConfig:
    bands: tuple[BandSpec, ...]
    sample_rate: int = 48_000
    transition_hz: float = 60.0
    target_suppression_db: float = 20.0
    max_cancel_gain: float = 4.0
    max_primary_suppression_db: float = 0.5
    primary_leakage: float = 0.005
    polarity: PolarityConfig = field(default_factory=PolarityConfig)
    estimator_version: int = 1
    schema_version: int = 1

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "estimator_version": self.estimator_version,
            "sample_rate": self.sample_rate,
            "bands": [
                {"name": band.name, "low_hz": band.low_hz, "high_hz": band.high_hz}
                for band in self.bands
            ],
            "filter": {"transition_hz": self.transition_hz},
            "target_suppression_db": self.target_suppression_db,
            "max_cancel_gain": self.max_cancel_gain,
            "max_primary_suppression_db": self.max_primary_suppression_db,
            "primary_leakage": self.primary_leakage,
            "polarity": self.polarity.to_dict(),
        }

    @property
    def fingerprint(self) -> str:
        return sha256_json(self.to_dict())

    def band_fingerprint(self, band: BandSpec) -> str:
        return sha256_json(
            {
                "band": {
                    "name": band.name,
                    "low_hz": band.low_hz,
                    "high_hz": band.high_hz,
                },
                "transition_hz": self.transition_hz,
                "sample_rate": self.sample_rate,
                "estimator_version": self.estimator_version,
            }
        )

    def band_by_name(self, name: str) -> BandSpec:
        for band in self.bands:
            if band.name == name:
                return band
        raise KeyError(name)

    def with_target_suppression(self, target_db: float) -> "AnalysisConfig":
        target = float(target_db)
        if not math.isfinite(target) or not (0.0 <= target <= 120.0):
            raise VenueError(
                E_CONFIG_INVALID,
                "target suppression must be finite and in [0, 120] dB",
                {"target_suppression_db": target},
            )
        return replace(self, target_suppression_db=target)

    def with_bands(self, bands: Iterable[tuple[str, float, float]]) -> "AnalysisConfig":
        return replace(
            self,
            bands=validate_bands(bands, self.sample_rate),
        )


def validate_bands(
    raw_bands: Iterable[dict[str, Any] | tuple[str, float, float]],
    sample_rate: int,
) -> tuple[BandSpec, ...]:
    normalized: list[tuple[str, float, float]] = []
    for item in raw_bands:
        if isinstance(item, dict):
            normalized.append((str(item["name"]), float(item["low_hz"]), float(item["high_hz"])))
        else:
            name, low, high = item
            normalized.append((str(name), float(low), float(high)))
    if not normalized:
        raise VenueError(E_CONFIG_INVALID, "band plan must not be empty")
    previous_high: float | None = None
    seen_names: set[str] = set()
    bands: list[BandSpec] = []
    nyquist = sample_rate / 2.0
    for index, (name, low, high) in enumerate(normalized):
        if not name:
            raise VenueError(E_CONFIG_INVALID, "band name must not be empty", {"band_index": index})
        if name in seen_names:
            raise VenueError(
                E_CONFIG_INVALID,
                "band names must be unique",
                {"band": name, "band_index": index},
            )
        if not (0.0 <= low < high <= nyquist):
            raise VenueError(
                E_CONFIG_INVALID,
                "band edges must satisfy 0 <= low < high <= Nyquist",
                {"band": name, "low_hz": low, "high_hz": high, "nyquist_hz": nyquist},
            )
        if previous_high is not None and low < previous_high:
            raise VenueError(
                E_CONFIG_INVALID,
                "bands must be sorted and must not overlap",
                {"band": name, "low_hz": low, "previous_high_hz": previous_high},
            )
        bands.append(BandSpec(index=index, name=name, low_hz=low, high_hz=high))
        seen_names.add(name)
        previous_high = high
    return tuple(bands)


def config_from_dict(data: dict[str, Any]) -> AnalysisConfig:
    sample_rate = int(data.get("sample_rate", 48_000))
    bands = validate_bands(data.get("bands", []), sample_rate)
    filter_data = data.get("filter", {})
    polarity_data = data.get("polarity", {})
    config = AnalysisConfig(
        bands=bands,
        sample_rate=sample_rate,
        transition_hz=float(filter_data.get("transition_hz", 60.0)),
        target_suppression_db=float(data.get("target_suppression_db", 20.0)),
        max_cancel_gain=float(data.get("max_cancel_gain", 4.0)),
        max_primary_suppression_db=float(data.get("max_primary_suppression_db", 0.5)),
        primary_leakage=float(data.get("primary_leakage", 0.005)),
        estimator_version=int(data.get("estimator_version", 1)),
        schema_version=int(data.get("schema_version", 1)),
        polarity=PolarityConfig(
            inversion_tolerance_deg=float(
                polarity_data.get("inversion_tolerance_deg", 30.0)
            ),
            jump_threshold_deg=float(polarity_data.get("jump_threshold_deg", 75.0)),
            wrap_threshold_deg=float(polarity_data.get("wrap_threshold_deg", 240.0)),
            low_band_max_hz=float(polarity_data.get("low_band_max_hz", 500.0)),
            group_delay_jump_ms=float(polarity_data.get("group_delay_jump_ms", 1.5)),
        ),
    )
    if config.sample_rate <= 0:
        raise VenueError(E_CONFIG_INVALID, "sample_rate must be positive")
    if not math.isfinite(config.transition_hz) or config.transition_hz <= 0:
        raise VenueError(E_CONFIG_INVALID, "transition_hz must be positive")
    if not math.isfinite(config.max_cancel_gain) or config.max_cancel_gain <= 0:
        raise VenueError(E_CONFIG_INVALID, "max_cancel_gain must be positive")
    if (
        not math.isfinite(config.target_suppression_db)
        or not (0.0 <= config.target_suppression_db <= 120.0)
    ):
        raise VenueError(
            E_CONFIG_INVALID,
            "target_suppression_db must be finite and in [0, 120]",
        )
    if not (0.0 <= config.max_primary_suppression_db < 60.0):
        raise VenueError(
            E_CONFIG_INVALID,
            "max_primary_suppression_db must be in [0, 60)",
        )
    if not (0.0 <= config.primary_leakage < 1.0):
        raise VenueError(E_CONFIG_INVALID, "primary_leakage must be in [0, 1)")
    return config


def load_config(path: str | Path) -> AnalysisConfig:
    try:
        data = read_json(path)
    except (OSError, ValueError) as exc:
        raise VenueError(
            E_CONFIG_INVALID,
            f"cannot read analysis config: {exc}",
            {"path": str(path)},
        ) from exc
    return config_from_dict(data)


def save_config(path: str | Path, config: AnalysisConfig) -> None:
    atomic_write_json(path, config.to_dict())
