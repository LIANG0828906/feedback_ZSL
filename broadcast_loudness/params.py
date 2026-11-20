"""Analysis parameters, channel layouts, and deterministic serialization."""

from __future__ import annotations

import hashlib
import json
import math
from collections.abc import Mapping
from dataclasses import dataclass, replace
from typing import Any

from .errors import InvalidParameterError

SAMPLE_RATE = 48_000
BLOCK_SAMPLES = 19_200
HOP_SAMPLES = 4_800
SHORT_WINDOW_SAMPLES = 144_000
TRUE_PEAK_OVERSAMPLE = 4
SURROUND_WEIGHT = 1.41

CHANNEL_LAYOUTS: dict[int, tuple[str, ...]] = {
    1: ("M",),
    2: ("L", "R"),
    3: ("L", "R", "C"),
    4: ("L", "R", "BL", "BR"),
    5: ("L", "R", "C", "BL", "BR"),
    6: ("L", "R", "C", "LFE", "BL", "BR"),
}

BS1770_WEIGHTS: dict[int, tuple[float, ...]] = {
    1: (1.0,),
    2: (1.0, 1.0),
    3: (1.0, 1.0, 1.0),
    4: (1.0, 1.0, SURROUND_WEIGHT, SURROUND_WEIGHT),
    5: (1.0, 1.0, 1.0, SURROUND_WEIGHT, SURROUND_WEIGHT),
    6: (1.0, 1.0, 1.0, 0.0, SURROUND_WEIGHT, SURROUND_WEIGHT),
}


def channel_layout(channel_count: int) -> tuple[str, ...]:
    try:
        return CHANNEL_LAYOUTS[channel_count]
    except KeyError as exc:
        raise InvalidParameterError(
            "unsupported channel layout", channel_count=channel_count
        ) from exc


def default_weights(channel_count: int) -> tuple[float, ...]:
    try:
        return BS1770_WEIGHTS[channel_count]
    except KeyError as exc:
        raise InvalidParameterError(
            "unsupported channel layout", channel_count=channel_count
        ) from exc


def identity_matrix(channel_count: int) -> tuple[tuple[float, ...], ...]:
    return tuple(
        tuple(1.0 if row == col else 0.0 for col in range(channel_count))
        for row in range(channel_count)
    )


def downmix_51_to_stereo() -> tuple[tuple[float, ...], ...]:
    """Configurable example: FL/FR/FC/BL/BR to stereo, LFE excluded."""
    s = math.sqrt(0.5)
    return (
        (1.0, 0.0, s, 0.0, s, 0.0),
        (0.0, 1.0, s, 0.0, 0.0, s),
    )


def _round_float(value: float) -> float:
    rounded = round(float(value), 12)
    return 0.0 if abs(rounded) < 5e-13 else rounded


def _json_default(value: Any) -> Any:
    if isinstance(value, tuple):
        return list(value)
    raise TypeError(f"not JSON serializable: {type(value)!r}")


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
        default=_json_default,
    )


def digest_json(value: Any) -> str:
    return hashlib.sha256(canonical_json(value).encode("utf-8")).hexdigest()


@dataclass(frozen=True)
class AnalysisParameters:
    """All parameters that control measurement or report interpretation.

    The 400 ms block and 100 ms hop are deliberately fixed for this format:
    changing them would alter the observation sequence rather than a tuning
    parameter.
    """

    target_lufs: float = -23.0
    true_peak_limit_dbtp: float = -1.0
    absolute_gate_lufs: float = -70.0
    relative_gate_lu: float = -10.0
    weights: tuple[float, ...] = (1.0,)
    downmix: tuple[tuple[float, ...], ...] = ((1.0,),)
    output_layout: tuple[str, ...] = ("M",)
    sample_rate: int = SAMPLE_RATE
    block_samples: int = BLOCK_SAMPLES
    hop_samples: int = HOP_SAMPLES
    short_window_samples: int = SHORT_WINDOW_SAMPLES
    true_peak_oversample: int = TRUE_PEAK_OVERSAMPLE

    @classmethod
    def for_channels(
        cls,
        channel_count: int,
        *,
        target_lufs: float = -23.0,
        true_peak_limit_dbtp: float = -1.0,
        absolute_gate_lufs: float = -70.0,
        relative_gate_lu: float = -10.0,
        weights: tuple[float, ...] | None = None,
        downmix: tuple[tuple[float, ...], ...] | None = None,
        output_layout: tuple[str, ...] | None = None,
    ) -> AnalysisParameters:
        matrix = identity_matrix(channel_count) if downmix is None else downmix
        out_count = len(matrix)
        if weights is None:
            if downmix is None:
                resolved_weights = default_weights(channel_count)
            else:
                resolved_weights = tuple(1.0 for _ in range(out_count))
        else:
            resolved_weights = tuple(weights)
        if output_layout is None:
            source_layout = channel_layout(channel_count)
            resolved_layout = (
                source_layout[:out_count]
                if len(source_layout) >= out_count
                else tuple(f"OUT{index}" for index in range(out_count))
            )
        else:
            resolved_layout = tuple(output_layout)
        params = cls(
            target_lufs=_round_float(target_lufs),
            true_peak_limit_dbtp=_round_float(true_peak_limit_dbtp),
            absolute_gate_lufs=_round_float(absolute_gate_lufs),
            relative_gate_lu=_round_float(relative_gate_lu),
            weights=tuple(_round_float(v) for v in resolved_weights),
            downmix=tuple(tuple(_round_float(v) for v in row) for row in matrix),
            output_layout=resolved_layout,
        )
        return params.validate(channel_count)

    def validate(self, channel_count: int | None = None) -> AnalysisParameters:
        if self.sample_rate != SAMPLE_RATE:
            raise InvalidParameterError(
                "only 48 kHz is supported", sample_rate=self.sample_rate
            )
        if self.block_samples != BLOCK_SAMPLES:
            raise InvalidParameterError(
                "400 ms block required", block_samples=self.block_samples
            )
        if self.hop_samples != HOP_SAMPLES:
            raise InvalidParameterError(
                "100 ms hop required", hop_samples=self.hop_samples
            )
        if self.short_window_samples != SHORT_WINDOW_SAMPLES:
            raise InvalidParameterError(
                "3 s short-term window required",
                short_window_samples=self.short_window_samples,
            )
        if self.true_peak_oversample != TRUE_PEAK_OVERSAMPLE:
            raise InvalidParameterError(
                "4x true-peak oversampling required",
                true_peak_oversample=self.true_peak_oversample,
            )
        if not math.isfinite(self.target_lufs) or not -60 <= self.target_lufs <= 0:
            raise InvalidParameterError("target LUFS out of range")
        if (
            not math.isfinite(self.true_peak_limit_dbtp)
            or not -30 <= self.true_peak_limit_dbtp <= 0
        ):
            raise InvalidParameterError("true-peak limit out of range")
        if (
            not math.isfinite(self.absolute_gate_lufs)
            or not -100 <= self.absolute_gate_lufs <= -1
        ):
            raise InvalidParameterError("absolute gate out of range")
        if (
            not math.isfinite(self.relative_gate_lu)
            or not -30 <= self.relative_gate_lu <= 0
        ):
            raise InvalidParameterError("relative gate out of range")
        if channel_count is not None and channel_count not in CHANNEL_LAYOUTS:
            raise InvalidParameterError(
                "unsupported channel count", channel_count=channel_count
            )
        if not self.downmix:
            raise InvalidParameterError("downmix matrix is empty")
        width = len(self.downmix[0])
        if any(len(row) != width for row in self.downmix):
            raise InvalidParameterError("downmix matrix rows differ in length")
        if any(
            not math.isfinite(coefficient)
            for row in self.downmix
            for coefficient in row
        ):
            raise InvalidParameterError("downmix coefficients must be finite")
        if channel_count is not None and width != channel_count:
            raise InvalidParameterError(
                "downmix matrix width does not match input channels",
                matrix_width=width,
                channel_count=channel_count,
            )
        if len(self.weights) != len(self.downmix):
            raise InvalidParameterError(
                "channel weight count does not match downmix output"
            )
        if any(not math.isfinite(weight) or weight < 0.0 for weight in self.weights):
            raise InvalidParameterError(
                "channel weights must be finite and non-negative"
            )
        if len(self.output_layout) != len(self.downmix):
            raise InvalidParameterError(
                "output layout count does not match downmix output"
            )
        return self

    def with_updates(self, *, channel_count: int, **updates: Any) -> AnalysisParameters:
        unknown = set(updates) - {
            "target_lufs",
            "true_peak_limit_dbtp",
            "absolute_gate_lufs",
            "relative_gate_lu",
            "weights",
            "downmix",
            "output_layout",
        }
        if unknown:
            raise InvalidParameterError("unknown parameter", fields=sorted(unknown))
        normalized = dict(updates)
        if "weights" in normalized and normalized["weights"] is not None:
            normalized["weights"] = tuple(
                _round_float(v) for v in normalized["weights"]
            )
        if "downmix" in normalized and normalized["downmix"] is not None:
            normalized["downmix"] = tuple(
                tuple(_round_float(v) for v in row) for row in normalized["downmix"]
            )
        if "output_layout" in normalized and normalized["output_layout"] is not None:
            normalized["output_layout"] = tuple(normalized["output_layout"])
        for name in (
            "target_lufs",
            "true_peak_limit_dbtp",
            "absolute_gate_lufs",
            "relative_gate_lu",
        ):
            if name in normalized and normalized[name] is not None:
                normalized[name] = _round_float(normalized[name])
        candidate = replace(self, **normalized)
        return candidate.validate(channel_count)

    def measurement_signature(self) -> str:
        return digest_json(
            {
                "downmix": self.downmix,
                "weights": self.weights,
                "output_layout": self.output_layout,
                "oversample": self.true_peak_oversample,
                "block": self.block_samples,
                "hop": self.hop_samples,
                "short": self.short_window_samples,
                "rate": self.sample_rate,
            }
        )

    def digest(self) -> str:
        return digest_json(self.as_dict())

    def as_dict(self) -> dict[str, Any]:
        return {
            "target_lufs": self.target_lufs,
            "true_peak_limit_dbtp": self.true_peak_limit_dbtp,
            "absolute_gate_lufs": self.absolute_gate_lufs,
            "relative_gate_lu": self.relative_gate_lu,
            "weights": list(self.weights),
            "downmix": [list(row) for row in self.downmix],
            "output_layout": list(self.output_layout),
            "sample_rate": self.sample_rate,
            "block_samples": self.block_samples,
            "hop_samples": self.hop_samples,
            "short_window_samples": self.short_window_samples,
            "true_peak_oversample": self.true_peak_oversample,
        }

    @classmethod
    def from_dict(cls, value: Mapping[str, Any]) -> AnalysisParameters:
        try:
            return cls(
                target_lufs=float(value["target_lufs"]),
                true_peak_limit_dbtp=float(value["true_peak_limit_dbtp"]),
                absolute_gate_lufs=float(value["absolute_gate_lufs"]),
                relative_gate_lu=float(value["relative_gate_lu"]),
                weights=tuple(float(v) for v in value["weights"]),
                downmix=tuple(tuple(float(v) for v in row) for row in value["downmix"]),
                output_layout=tuple(str(v) for v in value["output_layout"]),
                sample_rate=int(value["sample_rate"]),
                block_samples=int(value["block_samples"]),
                hop_samples=int(value["hop_samples"]),
                short_window_samples=int(value["short_window_samples"]),
                true_peak_oversample=int(value["true_peak_oversample"]),
            ).validate()
        except (KeyError, TypeError, ValueError) as exc:
            raise InvalidParameterError("invalid parameter document") from exc
