"""Canonical, validated conversion and scheduling configuration."""

from __future__ import annotations

import json
import zlib
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .errors import E_CONFIG_INVALID, E_CONVERSION_MISMATCH, error
from .model import PRIMARIES_BY_NAME, TRANSFER_BY_NAME, Primaries, TransferCode


VALID_TONE_MAPS = ("none", "reinhard", "aces")
VALID_WHITE_POINTS = ("d65", "d60", "d50")
MEMORY_PROFILES: dict[str, int] = {
    "full": 6,
    "banded": 4,
    "streaming": 2,
}


def _canonical_json(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True, allow_nan=False)


def _number(value: Any, field_name: str, minimum: float | None = None, maximum: float | None = None) -> float:
    try:
        result = float(value)
    except (TypeError, ValueError) as exc:
        raise error(E_CONFIG_INVALID, "parameter must be numeric", field=field_name, actual=value) from exc
    if result != result or result in (float("inf"), float("-inf")):
        raise error(E_CONFIG_INVALID, "parameter must be finite", field=field_name, actual=value)
    if minimum is not None and result < minimum:
        raise error(E_CONFIG_INVALID, "parameter below minimum", field=field_name, actual=result, minimum=minimum)
    if maximum is not None and result > maximum:
        raise error(E_CONFIG_INVALID, "parameter above maximum", field=field_name, actual=result, maximum=maximum)
    return result


def _integer(value: Any, field_name: str, minimum: int = 0, maximum: int | None = None) -> int:
    try:
        result = int(value)
    except (TypeError, ValueError) as exc:
        raise error(E_CONFIG_INVALID, "parameter must be an integer", field=field_name, actual=value) from exc
    if isinstance(value, bool):
        raise error(E_CONFIG_INVALID, "parameter must be an integer", field=field_name, actual=value)
    if not isinstance(value, (int, str)) and result != value:
        raise error(E_CONFIG_INVALID, "parameter must be an integer", field=field_name, actual=value)
    if isinstance(value, str) and (not value.strip() or str(result) != value.strip()):
        raise error(E_CONFIG_INVALID, "parameter must be an integer", field=field_name, actual=value)
    if result < minimum:
        raise error(E_CONFIG_INVALID, "parameter below minimum", field=field_name, actual=result, minimum=minimum)
    if maximum is not None and result > maximum:
        raise error(E_CONFIG_INVALID, "parameter above maximum", field=field_name, actual=result, maximum=maximum)
    return result


def _parse_primaries(value: Any, field_name: str) -> Primaries:
    if isinstance(value, Primaries):
        return value
    if isinstance(value, int):
        try:
            return Primaries(value)
        except ValueError as exc:
            raise error(E_CONFIG_INVALID, "unknown primaries code", field=field_name, actual=value) from exc
    if isinstance(value, str):
        key = value.strip().lower()
        if key in PRIMARIES_BY_NAME:
            return PRIMARIES_BY_NAME[key]
    raise error(E_CONFIG_INVALID, "unknown primaries", field=field_name, actual=value)


def _parse_transfer(value: Any, field_name: str) -> TransferCode:
    if isinstance(value, TransferCode):
        return value
    if isinstance(value, int):
        try:
            return TransferCode(value)
        except ValueError as exc:
            raise error(E_CONFIG_INVALID, "unknown transfer code", field=field_name, actual=value) from exc
    if isinstance(value, str):
        key = value.strip().lower()
        if key in TRANSFER_BY_NAME:
            return TRANSFER_BY_NAME[key]
    raise error(E_CONFIG_INVALID, "unknown transfer function", field=field_name, actual=value)


def _parse_white(value: Any, field_name: str) -> str:
    key = str(value).strip().lower().replace("-", "").replace("_", "")
    mapping = {"d65": "d65", "d60": "d60", "d50": "d50"}
    if key not in mapping:
        raise error(E_CONFIG_INVALID, "unknown white point", field=field_name, actual=value)
    return mapping[key]


@dataclass(frozen=True)
class ColorSpaceConfig:
    primaries: Primaries
    transfer: TransferCode
    white_point: str = "d65"
    peak_nits: float = 100.0

    @classmethod
    def from_dict(cls, value: dict[str, Any], field_name: str, default_peak: float) -> "ColorSpaceConfig":
        if not isinstance(value, dict):
            raise error(E_CONFIG_INVALID, "color space must be an object", field=field_name, actual=type(value).__name__)
        return cls(
            primaries=_parse_primaries(value.get("primaries", "rec709"), f"{field_name}.primaries"),
            transfer=_parse_transfer(value.get("transfer", "sdr-gamma"), f"{field_name}.transfer"),
            white_point=_parse_white(value.get("white_point", "d65"), f"{field_name}.white_point"),
            peak_nits=_number(value.get("peak_nits", default_peak), f"{field_name}.peak_nits", 1.0, 10000.0),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "primaries": {
                Primaries.REC709: "rec709",
                Primaries.DISPLAY_P3: "display-p3",
                Primaries.REC2020: "rec2020",
            }[self.primaries],
            "transfer": {
                TransferCode.SDR_GAMMA: "sdr-gamma",
                TransferCode.PQ: "pq",
                TransferCode.HLG: "hlg",
            }[self.transfer],
            "white_point": self.white_point,
            "peak_nits": self.peak_nits,
        }


@dataclass(frozen=True)
class ObservationSpec:
    """The single observation window shared by preview and offline output."""

    window: tuple[int, int, int, int] | None = None
    sync_marker: str = "sync-0001"
    tolerance_code: int = 1

    @classmethod
    def from_dict(cls, value: dict[str, Any] | None) -> "ObservationSpec":
        value = value or {}
        if not isinstance(value, dict):
            raise error(E_CONFIG_INVALID, "observation must be an object", field="observation")
        window_raw = value.get("window")
        window: tuple[int, int, int, int] | None = None
        if window_raw is not None:
            if not isinstance(window_raw, (list, tuple)) or len(window_raw) != 4:
                raise error(E_CONFIG_INVALID, "observation window must be [x,y,width,height]", field="observation.window")
            window = tuple(_integer(part, "observation.window", 0) for part in window_raw)  # type: ignore[assignment]
            if window[2] <= 0 or window[3] <= 0:
                raise error(E_CONFIG_INVALID, "observation window width/height must be positive", field="observation.window")
        marker = str(value.get("sync_marker", "sync-0001")).strip()
        if not marker or len(marker) > 96:
            raise error(E_CONFIG_INVALID, "sync_marker must be 1..96 characters", field="observation.sync_marker", actual=marker)
        tolerance = _integer(value.get("tolerance_code", 1), "observation.tolerance_code", 0, 65535)
        return cls(window=window, sync_marker=marker, tolerance_code=tolerance)

    def validate_for_sequence(self, width: int, height: int) -> None:
        if self.window is None:
            return
        x, y, w, h = self.window
        if x + w > width or y + h > height or w <= 0 or h <= 0:
            raise error(
                E_CONFIG_INVALID,
                "observation window lies outside the sequence",
                field="observation.window",
                window=list(self.window),
                width=width,
                height=height,
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "window": list(self.window) if self.window is not None else None,
            "sync_marker": self.sync_marker,
            "tolerance_code": self.tolerance_code,
        }


@dataclass(frozen=True)
class MemoryConfig:
    parallelism: int
    tile_budget_bytes: int
    run_budget_bytes: int
    downgrade_profiles: tuple[str, ...] = ("full", "banded", "streaming")
    estimator_overhead_bytes: int = 12_288

    @classmethod
    def from_dict(cls, value: dict[str, Any] | None) -> "MemoryConfig":
        value = value or {}
        if not isinstance(value, dict):
            raise error(E_CONFIG_INVALID, "memory must be an object", field="memory")
        parallelism = _integer(value.get("parallelism", 2), "memory.parallelism", 1, 64)
        tile_budget = _integer(
            value.get("tile_budget_bytes", 8 * 1024 * 1024),
            "memory.tile_budget_bytes",
            1,
        )
        run_budget_default = max(tile_budget, parallelism * tile_budget)
        run_budget = _integer(
            value.get("run_budget_bytes", run_budget_default),
            "memory.run_budget_bytes",
            1,
        )
        raw_profiles = value.get("downgrade_profiles", ["full", "banded", "streaming"])
        if not isinstance(raw_profiles, list) or not raw_profiles:
            raise error(E_CONFIG_INVALID, "downgrade_profiles must be a non-empty list", field="memory.downgrade_profiles")
        profiles = tuple(str(item) for item in raw_profiles)
        if len(set(profiles)) != len(profiles):
            raise error(E_CONFIG_INVALID, "downgrade profiles must be unique", field="memory.downgrade_profiles")
        unknown = [profile for profile in profiles if profile not in MEMORY_PROFILES]
        if unknown:
            raise error(
                E_CONFIG_INVALID,
                "unknown memory profile",
                field="memory.downgrade_profiles",
                unknown=unknown,
                allowed=sorted(MEMORY_PROFILES),
            )
        overhead = _integer(value.get("estimator_overhead_bytes", 12_288), "memory.estimator_overhead_bytes", 0)
        return cls(
            parallelism=parallelism,
            tile_budget_bytes=tile_budget,
            run_budget_bytes=run_budget,
            downgrade_profiles=profiles,
            estimator_overhead_bytes=overhead,
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "parallelism": self.parallelism,
            "tile_budget_bytes": self.tile_budget_bytes,
            "run_budget_bytes": self.run_budget_bytes,
            "downgrade_profiles": list(self.downgrade_profiles),
            "estimator_overhead_bytes": self.estimator_overhead_bytes,
        }


@dataclass(frozen=True)
class JobConfig:
    source: ColorSpaceConfig
    target: ColorSpaceConfig
    tone_map_curve: str = "reinhard"
    global_exposure_stops: float = 0.0
    block_exposure_stops: dict[int, float] = field(default_factory=dict)
    block_tone_map: dict[int, str] = field(default_factory=dict)
    observation: ObservationSpec = field(default_factory=ObservationSpec)
    memory: MemoryConfig = field(default_factory=lambda: MemoryConfig.from_dict({}))
    preview_refresh_ms: int = 250
    expected_conversion_id: int | None = None

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "JobConfig":
        if not isinstance(value, dict):
            raise error(E_CONFIG_INVALID, "configuration root must be an object", field="root")
        source = ColorSpaceConfig.from_dict(value.get("source", {}), "source", 100.0)
        target = ColorSpaceConfig.from_dict(value.get("target", {}), "target", 1000.0)
        curve = str(value.get("tone_map_curve", "reinhard")).strip().lower()
        if curve not in VALID_TONE_MAPS:
            raise error(E_CONFIG_INVALID, "unknown tone map curve", field="tone_map_curve", actual=curve, allowed=list(VALID_TONE_MAPS))
        exposure = _number(value.get("global_exposure_stops", 0.0), "global_exposure_stops", -16.0, 16.0)
        raw_exposures = value.get("block_exposure_stops", {})
        if not isinstance(raw_exposures, dict):
            raise error(E_CONFIG_INVALID, "block_exposure_stops must be an object", field="block_exposure_stops")
        block_exposures: dict[int, float] = {}
        for raw_tile, raw_value in raw_exposures.items():
            tile_id = _integer(raw_tile, "block_exposure_stops.tile_id", 0)
            block_exposures[tile_id] = _number(raw_value, f"block_exposure_stops.{tile_id}", -16.0, 16.0)
        raw_curves = value.get("block_tone_map", {})
        if not isinstance(raw_curves, dict):
            raise error(E_CONFIG_INVALID, "block_tone_map must be an object", field="block_tone_map")
        block_curves: dict[int, str] = {}
        for raw_tile, raw_curve in raw_curves.items():
            tile_id = _integer(raw_tile, "block_tone_map.tile_id", 0)
            item = str(raw_curve).strip().lower()
            if item not in VALID_TONE_MAPS:
                raise error(
                    E_CONFIG_INVALID,
                    "unknown block tone map curve",
                    field=f"block_tone_map.{tile_id}",
                    actual=raw_curve,
                    allowed=list(VALID_TONE_MAPS),
                )
            block_curves[tile_id] = item
        expected_id = value.get("expected_conversion_id")
        if expected_id is not None:
            expected_id = _integer(expected_id, "expected_conversion_id", 0, 0xFFFFFFFF)
        refresh = _integer(value.get("preview_refresh_ms", 250), "preview_refresh_ms", 16, 60_000)
        return cls(
            source=source,
            target=target,
            tone_map_curve=curve,
            global_exposure_stops=exposure,
            block_exposure_stops=block_exposures,
            block_tone_map=block_curves,
            observation=ObservationSpec.from_dict(value.get("observation")),
            memory=MemoryConfig.from_dict(value.get("memory")),
            preview_refresh_ms=refresh,
            expected_conversion_id=expected_id,
        )

    @classmethod
    def load(cls, path: str | Path) -> "JobConfig":
        try:
            raw = json.loads(Path(path).read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError, UnicodeError) as exc:
            raise error(E_CONFIG_INVALID, "cannot load configuration", field="path", path=str(path), reason=str(exc)) from exc
        return cls.from_dict(raw)

    def conversion_payload(self) -> dict[str, Any]:
        """Parameters that affect converted code values and block fingerprints."""

        return {
            "source": self.source.to_dict(),
            "target": self.target.to_dict(),
            "tone_map_curve": self.tone_map_curve,
            "global_exposure_stops": self.global_exposure_stops,
            "block_exposure_stops": {str(key): self.block_exposure_stops[key] for key in sorted(self.block_exposure_stops)},
            "block_tone_map": {str(key): self.block_tone_map[key] for key in sorted(self.block_tone_map)},
            "observation": self.observation.to_dict(),
        }

    def conversion_id(self) -> int:
        payload = _canonical_json(self.conversion_payload()).encode("utf-8")
        return zlib.crc32(payload) & 0xFFFFFFFF

    def config_hash(self) -> str:
        import hashlib

        envelope = {
            "conversion": self.conversion_payload(),
            "memory": self.memory.to_dict(),
            "preview_refresh_ms": self.preview_refresh_ms,
        }
        return hashlib.sha256(_canonical_json(envelope).encode("utf-8")).hexdigest()

    def canonical_json(self) -> str:
        return _canonical_json(self.to_dict())

    def to_dict(self) -> dict[str, Any]:
        return {
            "source": self.source.to_dict(),
            "target": self.target.to_dict(),
            "tone_map_curve": self.tone_map_curve,
            "global_exposure_stops": self.global_exposure_stops,
            "block_exposure_stops": {str(key): value for key, value in sorted(self.block_exposure_stops.items())},
            "block_tone_map": {str(key): value for key, value in sorted(self.block_tone_map.items())},
            "observation": self.observation.to_dict(),
            "memory": self.memory.to_dict(),
            "preview_refresh_ms": self.preview_refresh_ms,
            "expected_conversion_id": self.expected_conversion_id,
        }

    def validate_for_sequence(
        self,
        width: int,
        height: int,
        block_count: int,
        source_transfer: TransferCode,
        conversion_id: int | None = None,
    ) -> None:
        self.observation.validate_for_sequence(width, height)
        if self.source.transfer != source_transfer:
            raise error(
                E_CONFIG_INVALID,
                "source transfer does not match the sequence header",
                field="source.transfer",
                actual=self.source.transfer.name,
                expected=source_transfer.name,
            )
        for tile_id in sorted(set(self.block_exposure_stops) | set(self.block_tone_map)):
            if tile_id < 0 or tile_id >= block_count:
                raise error(
                    E_CONFIG_INVALID,
                    "block override refers to an unknown tile",
                    field="block_overrides.tile_id",
                    tile_id=tile_id,
                    block_count=block_count,
                )
        if (
            self.expected_conversion_id is not None
            and conversion_id is not None
            and self.expected_conversion_id != conversion_id
        ):
            raise error(
                E_CONVERSION_MISMATCH,
                "expected conversion id does not match the sequence header",
                field="expected_conversion_id",
                actual=self.expected_conversion_id,
                expected=conversion_id,
            )

    def effective_exposure(self, block_exposure: float, tile_id: int) -> float:
        base = self.block_exposure_stops.get(tile_id, block_exposure)
        return float(base) + self.global_exposure_stops

    def effective_curve(self, tile_id: int) -> str:
        return self.block_tone_map.get(tile_id, self.tone_map_curve)
