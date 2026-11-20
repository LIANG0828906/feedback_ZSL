"""Explicit fingerprint configuration; there are no implicit field defaults."""

from __future__ import annotations

from dataclasses import asdict, dataclass
from typing import Any

from .errors import BizError, E_FP_INVALID
from .util import content_hash


CANONICAL_BAND_EDGES_HZ: tuple[int, ...] = (0, 500, 2000, 4000, 8000)
CHANNEL_MERGE_STRATEGIES = ("mono", "mean", "left", "right", "sum_clip", "not_recorded")


@dataclass(frozen=True)
class FingerprintConfig:
    version: int = 3
    frame_size: int = 1024
    hop_length: int = 256
    peak_count: int = 6
    pair_max_delta_frames: int = 8
    pairs_per_peak: int = 2
    band_edges_hz: tuple[int, ...] = CANONICAL_BAND_EDGES_HZ
    channel_merge: str = "mean"
    time_quant_bits: int | None = 0
    segment_ms: int = 2000
    peak_threshold: float = 0.25

    def validate(self) -> None:
        if self.version not in (1, 2, 3):
            raise BizError(
                E_FP_INVALID,
                "unsupported fingerprint version",
                {"record_id": "<config>", "field": "version", "value": self.version},
            )
        if self.frame_size < 64 or self.frame_size & (self.frame_size - 1):
            raise BizError(E_FP_INVALID, "frame_size must be a power of two >= 64", {"field": "frame_size"})
        if self.hop_length <= 0 or self.hop_length > self.frame_size:
            raise BizError(E_FP_INVALID, "invalid hop_length", {"field": "hop_length"})
        if self.time_quant_bits is not None and not 0 <= self.time_quant_bits <= 16:
            raise BizError(E_FP_INVALID, "time_quant_bits must be in 0..16", {"field": "time_quant_bits"})
        if self.version == 1 and self.time_quant_bits is not None:
            raise BizError(
                E_FP_INVALID,
                "v1 stores frame numbers and must use the frame time basis",
                {"field": "time_quant_bits", "value": self.time_quant_bits},
            )
        if self.version == 2 and self.time_quant_bits is None:
            raise BizError(
                E_FP_INVALID,
                "v2 requires an explicit millisecond quantization width",
                {"field": "time_quant_bits"},
            )
        if self.channel_merge not in CHANNEL_MERGE_STRATEGIES:
            raise BizError(
                E_FP_INVALID,
                "unknown channel merge strategy",
                {"field": "channel_merge", "value": self.channel_merge},
            )
        if self.segment_ms <= 0:
            raise BizError(E_FP_INVALID, "segment_ms must be positive", {"field": "segment_ms"})
        edges = tuple(self.band_edges_hz)
        if not edges or edges[0] != 0 or any(a >= b for a, b in zip(edges, edges[1:])):
            raise BizError(E_FP_INVALID, "band edges must start at 0 and increase", {"field": "band_edges_hz"})

    @property
    def hash_algo(self) -> str:
        if self.version == 1:
            return "v1-pair-raw"
        return "v2-pairs-quant"

    @property
    def time_basis(self) -> dict[str, Any]:
        if self.time_quant_bits is None:
            return {
                "kind": "frame",
                "sample_rate": None,
                "hop_length": self.hop_length,
            }
        return {"kind": "ms", "bits": self.time_quant_bits}

    def as_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["band_edges_hz"] = list(self.band_edges_hz)
        value["hash_algo"] = self.hash_algo
        return value

    @property
    def config_hash(self) -> str:
        return content_hash(self.as_dict())

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "FingerprintConfig":
        required = {
            "version",
            "frame_size",
            "hop_length",
            "peak_count",
            "pair_max_delta_frames",
            "pairs_per_peak",
            "band_edges_hz",
            "channel_merge",
            "time_quant_bits",
            "segment_ms",
            "peak_threshold",
        }
        missing = sorted(required.difference(value))
        if missing:
            raise BizError(
                E_FP_INVALID,
                "fingerprint config is missing fields",
                {"field": missing[0], "missing": missing},
            )
        cfg = cls(
            version=int(value["version"]),
            frame_size=int(value["frame_size"]),
            hop_length=int(value["hop_length"]),
            peak_count=int(value["peak_count"]),
            pair_max_delta_frames=int(value["pair_max_delta_frames"]),
            pairs_per_peak=int(value["pairs_per_peak"]),
            band_edges_hz=tuple(int(item) for item in value["band_edges_hz"]),
            channel_merge=str(value["channel_merge"]),
            time_quant_bits=None if value["time_quant_bits"] is None else int(value["time_quant_bits"]),
            segment_ms=int(value["segment_ms"]),
            peak_threshold=float(value["peak_threshold"]),
        )
        cfg.validate()
        return cfg
