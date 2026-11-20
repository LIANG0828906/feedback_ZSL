from __future__ import annotations

import math
from dataclasses import dataclass, field, replace
from typing import Any, Mapping

import numpy as np

from .constants import (
    ADAPTATION_METHODS,
    BIT_DEPTHS,
    TONE_CURVES,
    WHITE_POINTS,
    require_color_space,
    require_transfer,
)
from .errors import AtlasError, ErrorCode
from .hashing import array_hash, hash_value


@dataclass(frozen=True)
class AtlasHeader:
    format_version: int
    width: int
    height: int
    bit_depth: int
    color_space: str
    transfer_function: str
    storage_format: str
    grid_cols: int
    grid_rows: int
    source_peak_nits: float
    pipeline_version: int
    payload_size: int
    payload_crc32: int
    header_crc32: int

    @property
    def channels(self) -> int:
        return 4

    @property
    def shape(self) -> tuple[int, int, int]:
        return (self.height, self.width, self.channels)

    @property
    def max_code(self) -> int:
        return (1 << self.bit_depth) - 1

    def to_dict(self) -> dict[str, Any]:
        return {
            "format_version": self.format_version,
            "width": self.width,
            "height": self.height,
            "bit_depth": self.bit_depth,
            "color_space": self.color_space,
            "transfer_function": self.transfer_function,
            "storage_format": self.storage_format,
            "grid_cols": self.grid_cols,
            "grid_rows": self.grid_rows,
            "source_peak_nits": self.source_peak_nits,
            "pipeline_version": self.pipeline_version,
            "payload_size": self.payload_size,
            "payload_crc32": f"{self.payload_crc32:08x}",
            "header_crc32": f"{self.header_crc32:08x}",
        }


@dataclass(frozen=True)
class TileSpec:
    index: int
    row: int
    col: int
    x0: int
    y0: int
    x1: int
    y1: int

    @property
    def width(self) -> int:
        return self.x1 - self.x0

    @property
    def height(self) -> int:
        return self.y1 - self.y0

    @property
    def bbox(self) -> tuple[int, int, int, int]:
        return (self.x0, self.y0, self.x1, self.y1)

    @property
    def shape(self) -> tuple[int, int, int]:
        return (self.height, self.width, 4)

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "row": self.row,
            "col": self.col,
            "bbox": list(self.bbox),
            "width": self.width,
            "height": self.height,
        }


@dataclass(frozen=True)
class AtlasImage:
    header: AtlasHeader
    encoded: np.ndarray
    linear: np.ndarray
    tiles: tuple[TileSpec, ...]
    source_hash: str
    payload_hash: str
    raw_bytes: bytes

    @property
    def tile_count(self) -> int:
        return len(self.tiles)

    @property
    def fingerprint(self) -> str:
        return hash_value(
            {
                "source_hash": self.source_hash,
                "payload_hash": self.payload_hash,
                "header": self.header.to_dict(),
            }
        )

    def tile_encoded(self, index: int) -> np.ndarray:
        spec = self.tiles[index]
        return self.encoded[spec.y0 : spec.y1, spec.x0 : spec.x1, :]

    def tile_linear(self, index: int) -> np.ndarray:
        spec = self.tiles[index]
        return self.linear[spec.y0 : spec.y1, spec.x0 : spec.x1, :]


@dataclass(frozen=True)
class TileParams:
    exposure_ev: float = 0.0
    tone_curve: str = "none"
    tone_strength: float = 1.0

    def validate(self, *, tile: int | None = None) -> "TileParams":
        if not isinstance(self.exposure_ev, (int, float)) or isinstance(self.exposure_ev, bool):
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "exposure_ev must be a finite number",
                field="exposure_ev",
                tile=tile,
            )
        exposure = float(self.exposure_ev)
        if not math.isfinite(exposure) or exposure < -64.0 or exposure > 64.0:
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "exposure_ev must be finite and within [-64, 64]",
                field="exposure_ev",
                tile=tile,
                details={"value": exposure},
            )
        if self.tone_curve not in TONE_CURVES:
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                f"tone_curve must be one of {list(TONE_CURVES)}",
                field="tone_curve",
                tile=tile,
                details={"value": str(self.tone_curve)},
            )
        if not isinstance(self.tone_strength, (int, float)) or isinstance(self.tone_strength, bool):
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "tone_strength must be a number",
                field="tone_strength",
                tile=tile,
            )
        strength = float(self.tone_strength)
        if not math.isfinite(strength) or not 0.0 <= strength <= 1.0:
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "tone_strength must be within [0, 1]",
                field="tone_strength",
                tile=tile,
                details={"value": strength},
            )
        return TileParams(exposure, self.tone_curve, strength)

    def to_dict(self) -> dict[str, Any]:
        return {
            "exposure_ev": float(self.exposure_ev),
            "tone_curve": self.tone_curve,
            "tone_strength": float(self.tone_strength),
        }

    @classmethod
    def from_mapping(cls, value: "TileParams | Mapping[str, Any]") -> "TileParams":
        if isinstance(value, cls):
            return value
        if not isinstance(value, Mapping):
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "tile parameters must be a mapping or TileParams",
                field="tile_params",
            )
        allowed = {"exposure_ev", "tone_curve", "tone_strength"}
        unknown = sorted(set(value) - allowed)
        if unknown:
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                f"unknown tile parameter field(s): {', '.join(unknown)}",
                field=unknown[0],
            )
        semantic_fields = ("exposure_ev", "tone_strength")
        for field in semantic_fields:
            if field in value and (
                isinstance(value[field], bool)
                or not isinstance(value[field], (int, float))
            ):
                raise AtlasError(
                    ErrorCode.E_PARAM_CONFLICT,
                    f"{field} must be numeric",
                    field=field,
                )
        try:
            return cls(
                exposure_ev=float(value.get("exposure_ev", 0.0)),
                tone_curve=str(value.get("tone_curve", "none")),
                tone_strength=float(value.get("tone_strength", 1.0)),
            )
        except (TypeError, ValueError) as exc:
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                f"invalid tile parameter value: {exc}",
                field="tile_params",
            ) from None


@dataclass(frozen=True)
class TransformConfig:
    target_color_space: str
    target_transfer: str
    target_peak_nits: float
    source_white_point: str = "D65"
    target_white_point: str = "D65"
    adaptation: str = "bradford"

    def validate(self, *, source_peak_nits: float | None = None) -> "TransformConfig":
        require_color_space(self.target_color_space, field="target_color_space")
        require_transfer(self.target_transfer, field="target_transfer")
        if self.source_white_point not in WHITE_POINTS:
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                f"source_white_point must be one of {list(WHITE_POINTS)}",
                field="source_white_point",
                details={"value": self.source_white_point},
            )
        if self.target_white_point not in WHITE_POINTS:
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                f"target_white_point must be one of {list(WHITE_POINTS)}",
                field="target_white_point",
                details={"value": self.target_white_point},
            )
        if self.adaptation not in ADAPTATION_METHODS:
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                f"adaptation must be one of {list(ADAPTATION_METHODS)}",
                field="adaptation",
                details={"value": self.adaptation},
            )
        if isinstance(self.target_peak_nits, bool):
            peak = float("nan")
        else:
            try:
                peak = float(self.target_peak_nits)
            except (TypeError, ValueError):
                peak = float("nan")
        if not math.isfinite(peak) or not (1.0 <= peak <= 10000.0):
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "target_peak_nits must be within [1, 10000]",
                field="target_peak_nits",
                details={"value": self.target_peak_nits},
            )
        return TransformConfig(
            target_color_space=self.target_color_space,
            target_transfer=self.target_transfer,
            target_peak_nits=peak,
            source_white_point=self.source_white_point,
            target_white_point=self.target_white_point,
            adaptation=self.adaptation,
        )

    @property
    def shared_key(self) -> dict[str, Any]:
        return {
            "target_color_space": self.target_color_space,
            "target_transfer": self.target_transfer,
            "target_peak_nits": float(self.target_peak_nits),
            "source_white_point": self.source_white_point,
            "target_white_point": self.target_white_point,
            "adaptation": self.adaptation,
        }

    def to_dict(self) -> dict[str, Any]:
        return dict(self.shared_key)

    @classmethod
    def from_mapping(cls, value: "TransformConfig | Mapping[str, Any]") -> "TransformConfig":
        if isinstance(value, cls):
            return value
        if not isinstance(value, Mapping):
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "transform config must be a mapping or TransformConfig",
                field="config",
            )
        allowed = {
            "target_color_space",
            "target_transfer",
            "target_peak_nits",
            "source_white_point",
            "target_white_point",
            "adaptation",
        }
        unknown = sorted(set(value) - allowed)
        if unknown:
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                f"unknown config field(s): {', '.join(unknown)}",
                field=unknown[0],
            )
        required = {"target_color_space", "target_transfer", "target_peak_nits"}
        missing = sorted(required - set(value))
        if missing:
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                f"missing config field(s): {', '.join(missing)}",
                field=missing[0],
            )
        if (
            isinstance(value["target_peak_nits"], bool)
            or not isinstance(value["target_peak_nits"], (int, float))
        ):
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "target_peak_nits must be numeric",
                field="target_peak_nits",
            )
        try:
            return cls(
                target_color_space=str(value["target_color_space"]),
                target_transfer=str(value["target_transfer"]),
                target_peak_nits=float(value["target_peak_nits"]),
                source_white_point=str(value.get("source_white_point", "D65")),
                target_white_point=str(value.get("target_white_point", "D65")),
                adaptation=str(value.get("adaptation", "bradford")),
            )
        except (TypeError, ValueError) as exc:
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                f"invalid config value: {exc}",
                field="config",
            ) from None


@dataclass(frozen=True)
class TileErrorSummary:
    tile: int
    max_code_deviation: float
    mean_code_deviation: float
    rmse_code_deviation: float
    p99_code_deviation: float
    clipped_fraction: float
    summary_hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "tile": self.tile,
            "max_code_deviation": float(self.max_code_deviation),
            "mean_code_deviation": float(self.mean_code_deviation),
            "rmse_code_deviation": float(self.rmse_code_deviation),
            "p99_code_deviation": float(self.p99_code_deviation),
            "clipped_fraction": float(self.clipped_fraction),
            "summary_hash": self.summary_hash,
        }

    @classmethod
    def from_dict(cls, value: Mapping[str, Any]) -> "TileErrorSummary":
        return cls(
            tile=int(value["tile"]),
            max_code_deviation=float(value["max_code_deviation"]),
            mean_code_deviation=float(value["mean_code_deviation"]),
            rmse_code_deviation=float(value["rmse_code_deviation"]),
            p99_code_deviation=float(value["p99_code_deviation"]),
            clipped_fraction=float(value["clipped_fraction"]),
            summary_hash=str(value["summary_hash"]),
        )


@dataclass(frozen=True)
class CachedTile:
    key: str
    tile: int
    encoded: np.ndarray
    tile_hash: str
    summary: TileErrorSummary

    @classmethod
    def build(
        cls,
        *,
        key: str,
        tile: int,
        encoded: np.ndarray,
        summary: TileErrorSummary,
    ) -> "CachedTile":
        contiguous = np.ascontiguousarray(encoded, dtype=np.float16)
        return cls(
            key=key,
            tile=tile,
            encoded=contiguous,
            tile_hash=array_hash(contiguous),
            summary=summary,
        )


@dataclass(frozen=True)
class Version:
    number: int
    parent_version: int | None
    parent_chain_hash: str | None
    chain_hash: str
    atlas_fingerprint: str
    reason: str
    rollback_of: int | None
    config: TransformConfig
    tile_params: tuple[TileParams, ...]
    affected_tiles: tuple[int, ...]
    reused_tiles: tuple[int, ...]
    output_tile_hashes: tuple[str, ...]
    content_hash: str
    transformation_id: str
    chain_digests: dict[str, str]
    tile_summaries: tuple[TileErrorSummary, ...]
    error_summary_hash: str

    @property
    def tile_count(self) -> int:
        return len(self.tile_params)

    def summary(self) -> dict[str, Any]:
        return {
            "number": self.number,
            "parent_version": self.parent_version,
            "chain_hash": self.chain_hash,
            "content_hash": self.content_hash,
            "reason": self.reason,
            "rollback_of": self.rollback_of,
            "affected_tiles": list(self.affected_tiles),
            "reused_tiles": list(self.reused_tiles),
        }
